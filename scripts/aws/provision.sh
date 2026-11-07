#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Sets up an AWS S3 bucket and a least-privilege IAM user for vmobj.
# Run scripts/aws/provision.sh --help for the options.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/aws/provision.sh --bucket NAME --region REGION [options]

Creates, for vmobj:
  - an S3 bucket with Object Lock and versioning on, public access blocked,
    default encryption, and a lifecycle rule that cleans up abandoned
    uploads and leftover delete markers. No default retention: vmobj sets
    each object's lock itself.
  - an IAM user whose only permissions are what vmobj needs on that bucket.
    It is explicitly denied s3:BypassGovernanceRetention, so neither vmobj
    nor anyone holding this key can delete a locked backup early.
  - an access key for that user, written straight into an AWS credentials
    file as a named profile. The secret is never printed.

Run it with credentials that may create buckets and IAM users (an
administrator profile). Those credentials are only used here; vmobj never
sees them. Running it again on an existing bucket checks the bucket and
reapplies the settings and the policy; it makes a second key only with
--new-key.

Options:
  --bucket NAME            bucket to create (globally unique, 3-63 lowercase
                           letters, digits, dots and hyphens)
  --region REGION          AWS region, e.g. us-west-2 (default: $AWS_REGION)
  --purpose test|backups   tag on the bucket (default: test). teardown.sh
                           deletes a "backups" bucket only when told twice.
  --user NAME              IAM user for vmobj (default: vmobj-BUCKET)
  --profile NAME           credentials profile to write (default: the user name)
  --credentials-file PATH  where to write it (default: ~/.aws/credentials).
                           Use a separate file to carry the key to another
                           host, then delete the file.
  --admin-profile NAME     AWS CLI profile to run as (default: the AWS CLI's
                           own default)
  --no-object-lock         a plain versioned bucket (for repositories made
                           without vmobj init --object-lock)
  --no-key                 create the user and policy, but no access key
  --new-key                add another access key even if the user has one
  --dry-run                show what would happen; change nothing
  -h, --help               this text

Needs: aws (AWS CLI v2) and jq.
EOF
}

bucket="" region=${AWS_REGION:-} purpose=test user="" profile="" credfile="$HOME/.aws/credentials"
admin=() lock=true make_key=true new_key=false dry=false
while [ $# -gt 0 ]; do
  case $1 in
    --bucket) bucket=$2; shift 2 ;;
    --region) region=$2; shift 2 ;;
    --purpose) purpose=$2; shift 2 ;;
    --user) user=$2; shift 2 ;;
    --profile) profile=$2; shift 2 ;;
    --credentials-file) credfile=$2; shift 2 ;;
    --admin-profile) admin=(--profile "$2"); shift 2 ;;
    --no-object-lock) lock=false; shift ;;
    --no-key) make_key=false; shift ;;
    --new-key) new_key=true; shift ;;
    --dry-run) dry=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

die() { echo "provision: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
run() { # print, then run unless --dry-run
  local shown=("$@")
  [ "${shown[0]}" = awsc ] && shown[0]=aws
  printf '   $ %s\n' "${shown[*]}"
  $dry || "$@"
}
awsc() { aws "${admin[@]}" --region "$region" "$@"; }

[ -n "$bucket" ] || die "--bucket is required (see --help)"
[ -n "$region" ] || die "--region is required, or set AWS_REGION"
[[ $bucket =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || die "bucket name '$bucket' isn't valid for S3"
case $purpose in test|backups) ;; *) die "--purpose must be test or backups" ;; esac
user=${user:-vmobj-$bucket}
user=${user:0:64}
profile=${profile:-$user}
command -v aws >/dev/null || die "the AWS CLI (aws) is not installed"
command -v jq >/dev/null || die "jq is not installed"

step "who is running this"
ident=$(awsc sts get-caller-identity --output json) || die "the AWS CLI has no working credentials${admin[1]:+ for profile ${admin[1]}}"
account=$(jq -r .Account <<<"$ident")
echo "   account $account as $(jq -r .Arn <<<"$ident")"
[[ $(jq -r .Arn <<<"$ident") == *":root" ]] && echo "   note: these are the account's root credentials; an administrator IAM user or SSO role is safer"

# ---- the bucket ---------------------------------------------------------------

step "bucket $bucket in $region"
if awsc s3api head-bucket --bucket "$bucket" >/dev/null 2>&1; then
  echo "   exists and this account can reach it; checking its settings"
  where=$(awsc s3api get-bucket-location --bucket "$bucket" --query LocationConstraint --output text)
  [ "$where" = None ] && where=us-east-1
  [ "$where" = "$region" ] || die "the bucket is in $where, not $region"
  if $lock; then
    awsc s3api get-object-lock-configuration --bucket "$bucket" --query ObjectLockConfiguration.ObjectLockEnabled --output text 2>/dev/null | grep -q Enabled \
      || die "the bucket exists without Object Lock, which can only be turned on when a bucket is created. Use another name, or --no-object-lock."
  fi
else
  args=(s3api create-bucket --bucket "$bucket")
  [ "$region" = us-east-1 ] || args+=(--create-bucket-configuration "LocationConstraint=$region")
  $lock && args+=(--object-lock-enabled-for-bucket)
  run awsc "${args[@]}" >/dev/null
fi

step "bucket settings"
run awsc s3api put-public-access-block --bucket "$bucket" \
  --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
# Object Lock turns versioning on by itself; a bucket without it gets it here,
# so prune's version handling works the same either way.
run awsc s3api put-bucket-versioning --bucket "$bucket" --versioning-configuration Status=Enabled
run awsc s3api put-bucket-encryption --bucket "$bucket" \
  --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"},"BucketKeyEnabled":true}]}'
# Lifecycle never removes data a backup uses: it only clears multipart
# uploads that were never finished and delete markers with nothing behind them.
run awsc s3api put-bucket-lifecycle-configuration --bucket "$bucket" --lifecycle-configuration '{
  "Rules": [{
    "ID": "vmobj-housekeeping",
    "Status": "Enabled",
    "Filter": {},
    "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 7},
    "Expiration": {"ExpiredObjectDeleteMarker": true}
  }]
}'
run awsc s3api put-bucket-tagging --bucket "$bucket" \
  --tagging "TagSet=[{Key=vmobj:purpose,Value=$purpose},{Key=vmobj:user,Value=$user}]"

# ---- the user and its policy -----------------------------------------------------

step "IAM user $user"
if awsc iam get-user --user-name "$user" >/dev/null 2>&1; then
  echo "   exists"
else
  run awsc iam create-user --user-name "$user" --tags "Key=vmobj:bucket,Value=$bucket" >/dev/null
fi

policy=$(jq -n --arg b "$bucket" '{
  Version: "2012-10-17",
  Statement: [
    { Sid: "Objects", Effect: "Allow",
      Action: ["s3:PutObject", "s3:GetObject", "s3:GetObjectVersion", "s3:DeleteObject",
               "s3:DeleteObjectVersion", "s3:RestoreObject", "s3:PutObjectRetention",
               "s3:GetObjectRetention"],
      Resource: "arn:aws:s3:::\($b)/*" },
    { Sid: "Bucket", Effect: "Allow",
      Action: ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketObjectLockConfiguration",
               "s3:GetBucketLocation"],
      Resource: "arn:aws:s3:::\($b)" },
    { Sid: "NeverBypassOrLoosen", Effect: "Deny",
      Action: ["s3:BypassGovernanceRetention", "s3:PutBucketObjectLockConfiguration",
               "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:PutLifecycleConfiguration",
               "s3:PutBucketVersioning", "s3:DeleteBucket"],
      Resource: ["arn:aws:s3:::\($b)", "arn:aws:s3:::\($b)/*"] }
  ]
}')
step "policy vmobj-bucket-access on $user"
if $dry; then
  printf '   would attach:\n%s\n' "$policy"
else
  run awsc iam put-user-policy --user-name "$user" --policy-name vmobj-bucket-access --policy-document "$policy"
fi

# ---- the access key ------------------------------------------------------------

if $make_key; then
  step "access key -> profile [$profile] in $credfile"
  have=$(awsc iam list-access-keys --user-name "$user" --query 'length(AccessKeyMetadata)' --output text 2>/dev/null || echo 0)
  if [ "$have" != 0 ] && ! $new_key; then
    echo "   the user already has $have access key(s); not making another (use --new-key to add one)"
  elif [ -f "$credfile" ] && grep -q "^\[$profile\]" "$credfile"; then
    die "profile [$profile] already exists in $credfile; remove it, or pick another with --profile"
  elif $dry; then
    echo "   would create a key and append profile [$profile] to $credfile"
  else
    mkdir -p "$(dirname "$credfile")"
    chmod 700 "$(dirname "$credfile")" 2>/dev/null || true
    # The key stays in shell variables and goes into the file with printf, a
    # shell builtin, so the secret never appears on a command line or screen.
    keyjson=$(awsc iam create-access-key --user-name "$user" --output json)
    id=$(jq -r .AccessKey.AccessKeyId <<<"$keyjson")
    secret=$(jq -r .AccessKey.SecretAccessKey <<<"$keyjson")
    unset keyjson
    ( umask 077
      if [ -s "$credfile" ]; then
        [ -n "$(tail -c1 "$credfile")" ] && printf '\n' >> "$credfile"
        printf '\n' >> "$credfile"
      fi
      printf '[%s]\naws_access_key_id = %s\naws_secret_access_key = %s\n' "$profile" "$id" "$secret" >> "$credfile" )
    unset secret
    chmod 600 "$credfile"
    echo "   key $id written (the secret is only in the file)"
  fi
fi

step "done"
cat <<EOF
   Repository address for vmobj (add a /PREFIX after the bucket to share it):

     s3://$bucket?region=$region&profile=$profile

EOF
if $lock; then
  echo "   Make the repository with:  vmobj init --object-lock --repo 's3://$bucket/PREFIX?region=$region&profile=$profile' --key-file KEY"
else
  echo "   Make the repository with:  vmobj init --repo 's3://$bucket/PREFIX?region=$region&profile=$profile' --key-file KEY"
fi
$dry && echo "   (dry run: nothing was changed)"
exit 0
