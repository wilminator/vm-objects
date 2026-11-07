#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Removes what scripts/b2/provision.sh made. Run with --help.
set -euo pipefail

usage() {
  cat <<'EOF2'
Usage: scripts/b2/teardown.sh --bucket NAME [options]

Removes, in this order:
  1. every file version in the bucket. Files under a governance-mode lock
     (the kind vmobj writes) are removed with the governance bypass, which
     the key the b2 tool is signed in with needs (the master key has it).
     Files under a compliance-mode lock can't be removed by anyone until
     the lock runs out; the script stops before deleting the bucket.
  2. the bucket.
  3. the application key provision.sh made, and its profile in the
     credentials file.

A bucket provisioned with --purpose backups is refused unless you also pass
--destroy-backups with the bucket's name again. That deletes real backups
for good.

Options:
  --bucket NAME            the bucket
  --key-name NAME          key to remove (default: from the bucket's info,
                           else vmobj-BUCKET)
  --profile NAME           credentials profile to remove (default: the key name)
  --credentials-file PATH  default: ~/.aws/credentials
  --key-only               remove only the key and its profile; keep the bucket
  --empty-only             remove the files but keep the bucket and the key
  --destroy-backups NAME   required for a bucket provisioned as backups
  --dry-run                show what would happen; change nothing
  -h, --help               this text

Needs: b2 (version 4) and jq, signed in with b2 account authorize.
EOF2
}

bucket="" keyname="" profile="" credfile="$HOME/.aws/credentials" key_only=false empty_only=false destroy="" dry=false
while [ $# -gt 0 ]; do
  case $1 in
    --bucket) bucket=$2; shift 2 ;;
    --key-name) keyname=$2; shift 2 ;;
    --profile) profile=$2; shift 2 ;;
    --credentials-file) credfile=$2; shift 2 ;;
    --key-only) key_only=true; shift ;;
    --empty-only) empty_only=true; shift ;;
    --destroy-backups) destroy=$2; shift 2 ;;
    --dry-run) dry=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

die() { echo "teardown: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
run() { printf '   $ %s\n' "$*"; $dry || "$@"; }

[ -n "$bucket" ] || die "--bucket is required (see --help)"
command -v b2 >/dev/null || die "the b2 tool is not installed (pip install b2)"
command -v jq >/dev/null || die "jq is not installed"
b2 account get >/dev/null 2>&1 || die "the b2 tool isn't signed in; run: b2 account authorize"

purpose=unknown
if info=$(b2 bucket get "$bucket" 2>/dev/null); then
  purpose=$(jq -r '.bucketInfo["vmobj-purpose"] // "unknown"' <<<"$info")
  [ -n "$keyname" ] || keyname=$(jq -r '.bucketInfo["vmobj-key"] // empty' <<<"$info")
  have_bucket=true
else
  have_bucket=false
  echo "   no bucket $bucket in this account"
fi
keyname=${keyname:-vmobj-$bucket}
profile=${profile:-$keyname}

if $have_bucket && ! $key_only; then
  if [ "$purpose" = backups ] && [ "$destroy" != "$bucket" ]; then
    die "$bucket holds real backups (purpose=backups). To delete them for good, add --destroy-backups $bucket"
  fi
  step "files in $bucket (purpose=$purpose)"
  run b2 rm --recursive --versions --bypass-governance --no-progress "b2://$bucket" \
    || die "some files couldn't be removed (a compliance-mode lock, or a key without bypassGovernance); the bucket is kept"
  if ! $empty_only; then
    step "bucket $bucket"
    run b2 bucket delete "$bucket" || die "the bucket couldn't be deleted (files left under a lock?)"
  fi
fi

if ! $empty_only; then
  step "key $keyname"
  ids=$(b2 key list 2>/dev/null | awk -v n="$keyname" '$2 == n {print $1}')
  if [ -z "$ids" ]; then
    echo "   no key named $keyname"
  else
    for id in $ids; do run b2 key delete "$id" >/dev/null; done
  fi
  step "profile [$profile] in $credfile"
  if [ -f "$credfile" ] && grep -q "^\[$profile\]" "$credfile"; then
    if $dry; then
      echo "   would remove the [$profile] section"
    else
      tmp=$(mktemp "$credfile.XXXXXX")
      awk -v p="[$profile]" '$0 == p {skip = 1; next} /^\[/ {skip = 0} !skip' "$credfile" > "$tmp"
      chmod 600 "$tmp" && mv "$tmp" "$credfile"
      echo "   removed"
    fi
  else
    echo "   not there"
  fi
fi
step "done"
$dry && echo "   (dry run: nothing was changed)"
exit 0
