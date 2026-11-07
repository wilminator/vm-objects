#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Sets up a Backblaze B2 bucket and a bucket-only application key for vmobj
# (or for another backup tool, with --no-object-lock). Run with --help.
set -euo pipefail

usage() {
  cat <<'EOF2'
Usage: scripts/b2/provision.sh --bucket NAME [options]

Creates, through the b2 command-line tool:
  - a private B2 bucket with Object Lock (file lock) on and B2-managed
    encryption. No default retention: vmobj sets each file's lock itself.
    A lifecycle rule removes hidden (deleted) file versions after
    --keep-deleted-days, and abandoned large uploads after 7 days; B2 can
    never remove a version that is still locked.
  - an application key that works only on that bucket, with only what
    vmobj needs. It does not get bypassGovernance, so neither vmobj nor
    anyone holding the key can delete a locked backup early, nor change
    the bucket's settings.
  - that key written straight into an AWS-style credentials file as a
    named profile, which vmobj's S3 client reads. The secret is never
    printed.

Before running it, sign the b2 tool in with a key that may create buckets
and keys (your account's master key, or a key with all capabilities):

  b2 account authorize

It asks for the key ID and key, so neither lands in your shell history.
vmobj never sees that key.

Options:
  --bucket NAME            bucket to create (globally unique in B2, 6-63
                           letters, digits and hyphens)
  --purpose test|backups   stored with the bucket (default: test).
                           teardown.sh removes a "backups" bucket only when
                           told twice.
  --key-name NAME          application key name (default: vmobj-BUCKET)
  --profile NAME           credentials profile to write (default: the key name)
  --credentials-file PATH  where (default: ~/.aws/credentials). A separate
                           file carries the key to another host; delete it
                           afterwards.
  --keep-deleted-days N    days a deleted or replaced file stays recoverable
                           (default: 1 with Object Lock, where locks do the
                           protecting; 30 without)
  --no-object-lock         a bucket without file lock, for tools that don't
                           set locks (restic): deleted files then stay
                           recoverable for --keep-deleted-days
  --no-key                 create the bucket only
  --dry-run                show what would happen; change nothing
  -h, --help               this text

Needs: b2 (the Backblaze CLI, version 4: pip install b2) and jq.
EOF2
}

bucket="" purpose=test keyname="" profile="" credfile="$HOME/.aws/credentials"
lock=true keepdays="" make_key=true dry=false
while [ $# -gt 0 ]; do
  case $1 in
    --bucket) bucket=$2; shift 2 ;;
    --purpose) purpose=$2; shift 2 ;;
    --key-name) keyname=$2; shift 2 ;;
    --profile) profile=$2; shift 2 ;;
    --credentials-file) credfile=$2; shift 2 ;;
    --keep-deleted-days) keepdays=$2; shift 2 ;;
    --no-object-lock) lock=false; shift ;;
    --no-key) make_key=false; shift ;;
    --dry-run) dry=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

die() { echo "provision: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
run() { printf '   $ %s\n' "$*"; $dry || "$@"; }

[ -n "$bucket" ] || die "--bucket is required (see --help)"
[[ $bucket =~ ^[a-zA-Z0-9-]{6,63}$ ]] || die "bucket name '$bucket' isn't valid for B2 (6-63 letters, digits and hyphens)"
[[ $bucket == b2-* ]] && die "B2 bucket names can't start with b2-"
case $purpose in test|backups) ;; *) die "--purpose must be test or backups" ;; esac
if [ -z "$keepdays" ]; then if $lock; then keepdays=1; else keepdays=30; fi; fi
if ! [[ $keepdays =~ ^[0-9]+$ ]] || [ "$keepdays" -lt 1 ]; then die "--keep-deleted-days must be a whole number of days, at least 1"; fi
keyname=${keyname:-vmobj-$bucket}
profile=${profile:-$keyname}
command -v b2 >/dev/null || die "the b2 tool is not installed (pip install b2)"
command -v jq >/dev/null || die "jq is not installed"

step "who is running this"
# The account details include the signed-in key itself, so they stay in a
# variable and only the fields needed are read out.
acct=$(b2 account get 2>/dev/null) || die "the b2 tool isn't signed in; run: b2 account authorize"
endpoint=$(jq -r '.s3endpoint // .s3ApiUrl // .apiInfo.storageApi.s3ApiUrl // empty' <<<"$acct")
account=$(jq -r '.accountId // empty' <<<"$acct")
unset acct
[ -n "$endpoint" ] || die "couldn't read the account's S3 endpoint from 'b2 account get'"
region=$(sed -E 's#^https?://s3\.([^.]+)\..*#\1#' <<<"$endpoint")
echo "   account $account, S3 endpoint $endpoint (region $region)"

# ---- the bucket ---------------------------------------------------------------

rules=$(jq -cn --argjson d "$keepdays" '{fileNamePrefix:"", daysFromHidingToDeleting:$d, daysFromUploadingToHiding:null, daysFromStartingToCancelingUnfinishedLargeFiles:7}')
info=$(jq -cn --arg p "$purpose" --arg k "$keyname" '{"vmobj-purpose":$p, "vmobj-key":$k}')
step "bucket $bucket"
if existing=$(b2 bucket get "$bucket" 2>/dev/null); then
  echo "   exists in this account; checking it"
  if $lock && ! grep -Eq '"isFileLockEnabled": *true' <<<"$existing"; then
    die "the bucket exists without file lock; B2 can turn it on for an existing bucket, but vmobj's checks expect it from the start. Use another name, or --no-object-lock."
  fi
  [ "$(jq -r '.bucketType // empty' <<<"$existing")" = allPrivate ] || die "the bucket is not private"
  run b2 bucket update --bucket-info "$info" --lifecycle-rule "$rules" --default-server-side-encryption SSE-B2 "$bucket" allPrivate >/dev/null
else
  args=(bucket create --bucket-info "$info" --lifecycle-rule "$rules" --default-server-side-encryption SSE-B2)
  $lock && args+=(--file-lock-enabled)
  run b2 "${args[@]}" "$bucket" allPrivate >/dev/null
fi

# ---- the key -------------------------------------------------------------------

# What vmobj (or restic) does on the bucket, and nothing more: no
# bypassGovernance, no bucket or key management.
caps=listBuckets,readBuckets,listFiles,readFiles,writeFiles,deleteFiles,readBucketEncryption,readBucketRetentions,readFileRetentions,writeFileRetentions,readFileLegalHolds
if $make_key; then
  step "application key $keyname -> profile [$profile] in $credfile"
  if b2 key list 2>/dev/null | awk '{print $2}' | grep -qx "$keyname"; then
    echo "   a key named $keyname already exists; not making another (delete it with teardown.sh --key-only, or pick --key-name)"
  elif [ -f "$credfile" ] && grep -q "^\[$profile\]" "$credfile"; then
    die "profile [$profile] already exists in $credfile; remove it, or pick another with --profile"
  elif $dry; then
    echo "   would create a key on $bucket with: $caps"
  else
    mkdir -p "$(dirname "$credfile")"
    chmod 700 "$(dirname "$credfile")" 2>/dev/null || true
    # "b2 key create" prints the key ID and the key on one line. It stays in
    # shell variables and goes into the file with printf, a builtin, so the
    # secret never appears on a command line or the screen.
    out=$(b2 key create --bucket "$bucket" "$keyname" "$caps")
    read -r id secret <<<"$out"
    unset out
    if [ -z "$id" ] || [ -z "$secret" ]; then die "couldn't read the new key from b2's output"; fi
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
url="s3://$bucket?endpoint=$endpoint&region=$region&profile=$profile"
cat <<EOF2
   Repository address for vmobj (add a /PREFIX after the bucket to share it):

     $url

EOF2
if $lock; then
  echo "   Make the repository with:  vmobj init --object-lock --repo 's3://$bucket/PREFIX?endpoint=$endpoint&region=$region&profile=$profile' --key-file KEY"
  echo "   B2 has one storage class: back up with --profile offsite (or standard, or test for tests)."
else
  echo "   For restic:  RESTIC_REPOSITORY=s3:$endpoint/$bucket/PREFIX  AWS_PROFILE=$profile"
fi
$dry && echo "   (dry run: nothing was changed)"
exit 0
