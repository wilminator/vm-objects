#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# End-to-end check of the vmobj binary against any repository location.
#
#   scripts/e2e.sh REPO [VMOBJ_BINARY]
#
# REPO is a folder or an s3:// address. Use an empty folder or an unused
# prefix: the script creates a new repository there. It builds a 40 MiB test
# disk, runs a full, an incremental and a differential backup with changes
# in between, restores every one, and checks each restore is byte-identical
# to the disk at the time of its backup. All scratch files go in a temporary
# folder that is removed at the end; the repository is left for inspection.
#
# E2E_BACKUP_ARGS adds options to every backup, for example
#   E2E_BACKUP_ARGS="--profile standard"   for services that only know Standard.
# E2E_INIT_ARGS adds options to "vmobj init", for example
#   E2E_INIT_ARGS="--object-lock"          for a bucket with Object Lock.
set -euo pipefail

repo=${1:?usage: e2e.sh REPO [VMOBJ_BINARY]}
vmobj=${2:-vmobj}
read -ra extra <<< "${E2E_BACKUP_ARGS:-}"
read -ra init_extra <<< "${E2E_INIT_ARGS:-}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

key="$work/test.key"
disk="$work/disk.raw"
state="$work/state"
step() { printf '\n== %s\n' "$*"; }

# Write 1 MiB of random data at MiB offset $1.
scribble() { dd if=/dev/urandom of="$disk" bs=1M seek="$1" count=1 conv=notrunc status=none; }

step "key and repository"
"$vmobj" key init --key-file "$key"
# Small blocks keep the test quick; real repositories use the defaults.
"$vmobj" init --repo "$repo" --key-file "$key" --block-size 1MiB --extent-size 64KiB --pack-size 4MiB "${init_extra[@]}"

step "full backup"
truncate -s 40M "$disk"
for m in 0 3 7 12 20 33; do scribble "$m"; done
cp "$disk" "$work/want-full.raw"
"$vmobj" backup --repo "$repo" --key-file "$key" --source "$disk" --vm e2e --type full --state-dir "$state" "${extra[@]}"

step "incremental backup"
scribble 5; scribble 21
cp "$disk" "$work/want-inc.raw"
"$vmobj" backup --repo "$repo" --key-file "$key" --source "$disk" --vm e2e --type incremental --state-dir "$state" "${extra[@]}"

step "differential backup"
scribble 38
cp "$disk" "$work/want-diff.raw"
"$vmobj" backup --repo "$repo" --key-file "$key" --source "$disk" --vm e2e --type differential --state-dir "$state" "${extra[@]}"

step "list"
"$vmobj" list --repo "$repo" --key-file "$key" --vm e2e
mapfile -t names < <("$vmobj" list --repo "$repo" --key-file "$key" --vm e2e | sed -n 's/^  //p')
[ "${#names[@]}" -eq 3 ] || { echo "FAIL: expected 3 backups, found ${#names[@]}"; exit 1; }

step "verify and restore each backup"
for name in "${names[@]}"; do
  case "$name" in
    *-full) want=full ;;
    *-incremental) want=inc ;;
    *-differential) want=diff ;;
    *) echo "FAIL: unexpected backup name $name"; exit 1 ;;
  esac
  "$vmobj" verify --repo "$repo" --key-file "$key" --vm e2e --manifest "$name" --deep
  "$vmobj" restore --repo "$repo" --key-file "$key" --vm e2e --manifest "$name" --output "$work/got-$want.raw"
  cmp "$work/want-$want.raw" "$work/got-$want.raw"
  echo "OK: $name restores byte-identical"
done

step "prune (dry run): nothing is past its keep-until date yet"
"$vmobj" prune --repo "$repo" --key-file "$key" | tee "$work/prune.txt"
grep -q "Backups examined:     3" "$work/prune.txt" || { echo "FAIL: prune did not see the 3 backups"; exit 1; }
grep -q "Past keep-until:      0" "$work/prune.txt" || { echo "FAIL: prune found expired backups"; exit 1; }

step "wrong key is refused"
"$vmobj" key init --key-file "$work/other.key"
if "$vmobj" list --repo "$repo" --key-file "$work/other.key" >/dev/null 2>&1; then
  echo "FAIL: a different key opened the repository"; exit 1
fi
echo "OK: refused"

printf '\nPASS: all end-to-end checks against %s\n' "$repo"
