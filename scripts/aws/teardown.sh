#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Removes what scripts/aws/provision.sh made. Run with --help for the options.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/aws/teardown.sh --bucket NAME --region REGION [options]

Removes, in this order:
  1. every object version and delete marker in the bucket. Objects under a
     Governance-mode lock (the kind vmobj writes) are deleted with the
     governance bypass, which your administrator credentials need
     (s3:BypassGovernanceRetention). Objects under a Compliance-mode lock
     can't be deleted by anyone until the lock runs out; the script lists
     how many and stops before deleting the bucket.
  2. the bucket.
  3. the IAM user's access keys, its inline policy and the user.
  4. the profile from the credentials file.

A bucket tagged vmobj:purpose=backups is refused unless you also pass
--destroy-backups with the bucket's name again. That deletes real backups
for good.

Options:
  --bucket NAME            the bucket
  --region REGION          its region (default: $AWS_REGION)
  --user NAME              IAM user to remove (default: from the bucket's
                           vmobj:user tag, else vmobj-BUCKET)
  --profile NAME           credentials profile to remove (default: the user name)
  --credentials-file PATH  default: ~/.aws/credentials
  --admin-profile NAME     AWS CLI profile to run as
  --keep-user              leave the IAM user, its policy and the profile
  --empty-only             delete the objects but keep the bucket and user
  --destroy-backups NAME   required for a bucket tagged purpose=backups
  --dry-run                count what would be deleted; change nothing
  -h, --help               this text

Needs: aws (AWS CLI v2) and jq.
EOF
}

bucket="" region=${AWS_REGION:-} user="" profile="" credfile="$HOME/.aws/credentials"
admin=() keep_user=false empty_only=false confirm="" dry=false
while [ $# -gt 0 ]; do
  case $1 in
    --bucket) bucket=$2; shift 2 ;;
    --region) region=$2; shift 2 ;;
    --user) user=$2; shift 2 ;;
    --profile) profile=$2; shift 2 ;;
    --credentials-file) credfile=$2; shift 2 ;;
    --admin-profile) admin=(--profile "$2"); shift 2 ;;
    --keep-user) keep_user=true; shift ;;
    --empty-only) empty_only=true; keep_user=true; shift ;;
    --destroy-backups) confirm=$2; shift 2 ;;
    --dry-run) dry=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

die() { echo "teardown: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
awsc() { aws "${admin[@]}" --region "$region" "$@"; }

[ -n "$bucket" ] || die "--bucket is required (see --help)"
[ -n "$region" ] || die "--region is required, or set AWS_REGION"
command -v aws >/dev/null || die "the AWS CLI (aws) is not installed"
command -v jq >/dev/null || die "jq is not installed"

step "bucket $bucket"
bucket_there=true
if ! awsc s3api head-bucket --bucket "$bucket" >/dev/null 2>&1; then
  echo "   not found (or not reachable with these credentials); skipping to the user"
  bucket_there=false
fi

if $bucket_there; then
  tags=$(awsc s3api get-bucket-tagging --bucket "$bucket" --output json 2>/dev/null || echo '{"TagSet":[]}')
  purpose=$(jq -r '.TagSet[] | select(.Key=="vmobj:purpose") | .Value' <<<"$tags")
  tagged_user=$(jq -r '.TagSet[] | select(.Key=="vmobj:user") | .Value' <<<"$tags")
  echo "   purpose tag: ${purpose:-none}"
  if [ "$purpose" != test ] && [ "$confirm" != "$bucket" ]; then
    die "this bucket is tagged '${purpose:-nothing}', not 'test'. If you really mean to delete every backup in it for good, add: --destroy-backups $bucket"
  fi
  user=${user:-${tagged_user:-vmobj-$bucket}}

  step "emptying (all versions and delete markers)"
  total=0 compliance=0
  while :; do
    page=$(awsc s3api list-object-versions --bucket "$bucket" --max-items 1000 --output json)
    batch=$(jq -c '{Objects: ([(.Versions // [])[], (.DeleteMarkers // [])[]] | map({Key, VersionId})), Quiet: true}' <<<"$page")
    n=$(jq '.Objects | length' <<<"$batch")
    [ "$n" -gt 0 ] || break
    if $dry; then
      total=$(jq '[(.Versions // [])[], (.DeleteMarkers // [])[]] | length' \
        <<<"$(awsc s3api list-object-versions --bucket "$bucket" --output json)")
      echo "   would delete $total object versions and delete markers"
      break
    fi
    result=$(awsc s3api delete-objects --bucket "$bucket" --bypass-governance-retention --delete "$batch" --output json)
    errs=$(jq '(.Errors // []) | length' <<<"$result")
    total=$(( total + n - errs ))
    if [ "$errs" -gt 0 ]; then
      jq -r '(.Errors // [])[:5][] | "   could not delete \(.Key): \(.Code) \(.Message)"' <<<"$result"
      compliance=$errs
      # Anything left now is locked in a way the bypass can't lift.
      break
    fi
    printf '   deleted %d so far\n' "$total"
  done
  if [ "$compliance" -gt 0 ]; then
    die "$compliance or more versions could not be deleted (shown above). A Compliance-mode lock can't be lifted by anyone; delete them after it runs out, then run this again."
  fi
  $dry || echo "   empty ($total deleted)"

  if ! $empty_only; then
    step "deleting the bucket"
    if $dry; then echo "   would delete $bucket"; else awsc s3api delete-bucket --bucket "$bucket"; echo "   deleted"; fi
  fi
fi

user=${user:-vmobj-$bucket}
profile=${profile:-$user}
if ! $keep_user; then
  step "IAM user $user"
  if awsc iam get-user --user-name "$user" >/dev/null 2>&1; then
    for k in $(awsc iam list-access-keys --user-name "$user" --query 'AccessKeyMetadata[].AccessKeyId' --output text); do
      if $dry; then echo "   would delete access key $k"; else awsc iam delete-access-key --user-name "$user" --access-key-id "$k"; echo "   deleted access key $k"; fi
    done
    for p in $(awsc iam list-user-policies --user-name "$user" --query 'PolicyNames[]' --output text); do
      if $dry; then echo "   would delete policy $p"; else awsc iam delete-user-policy --user-name "$user" --policy-name "$p"; echo "   deleted policy $p"; fi
    done
    for p in $(awsc iam list-attached-user-policies --user-name "$user" --query 'AttachedPolicies[].PolicyArn' --output text); do
      if $dry; then echo "   would detach $p"; else awsc iam detach-user-policy --user-name "$user" --policy-arn "$p"; echo "   detached $p"; fi
    done
    if $dry; then echo "   would delete user $user"; else awsc iam delete-user --user-name "$user"; echo "   deleted user $user"; fi
  else
    echo "   not found"
  fi

  step "profile [$profile] in $credfile"
  if [ -f "$credfile" ] && grep -q "^\[$profile\]" "$credfile"; then
    if $dry; then
      echo "   would remove it"
    else
      # Drop the section: from its header up to the next header.
      tmp=$(mktemp "$credfile.XXXXXX")
      awk -v s="[$profile]" '/^\[/{skip = ($0 == s)} !skip' "$credfile" > "$tmp"
      chmod 600 "$tmp" && mv "$tmp" "$credfile"
      echo "   removed"
    fi
  else
    echo "   not present"
  fi
fi

step "done"
$dry && echo "   (dry run: nothing was changed)"
exit 0
