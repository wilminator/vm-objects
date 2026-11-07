#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# End-to-end check of zfs: sources. Run as root on a machine with ZFS.
#
#   scripts/zfs-e2e.sh PARENT_DATASET [VMOBJ_BINARY]
#
# Creates a 64 MiB test zvol PARENT_DATASET/vmobj-e2e, backs it up from
# snapshots (full, then incremental after changing it), restores both and
# compares them with copies taken at backup time, then checks that vmobj
# removed its snapshots and clones. The test zvol and a temporary local
# repository are destroyed at the end; nothing else is touched.
set -euo pipefail

parent=${1:?usage: zfs-e2e.sh PARENT_DATASET [VMOBJ_BINARY]}
vmobj=${2:-vmobj}
vol="$parent/vmobj-e2e"
work=$(mktemp -d)
cleanup() {
  zfs destroy -r "$vol" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

if zfs list "$vol" >/dev/null 2>&1; then
  echo "FAIL: $vol already exists; remove it or pick another parent"; exit 1
fi

step() { printf '\n== %s\n' "$*"; }
dev="/dev/zvol/$vol"

step "test zvol"
zfs create -V 64M -o volblocksize=16K "$vol"
for _ in $(seq 50); do [ -e "$dev" ] && break; sleep 0.2; done
dd if=/dev/urandom of="$dev" bs=1M count=20 oflag=direct status=none
dd if=/dev/urandom of="$dev" bs=1M seek=40 count=8 oflag=direct status=none
sync

"$vmobj" key init --key-file "$work/k"
"$vmobj" init --repo "$work/repo" --key-file "$work/k" --block-size 1MiB

step "full backup from a snapshot"
dd if="$dev" of="$work/want-full.raw" bs=1M status=none
"$vmobj" backup --repo "$work/repo" --key-file "$work/k" --source "zfs:$vol" --vm zfs-e2e --type full --state-dir "$work/state"

step "change the zvol, incremental backup"
dd if=/dev/urandom of="$dev" bs=64K seek=100 count=4 oflag=direct status=none
dd if=/dev/zero of="$dev" bs=1M seek=5 count=1 oflag=direct status=none
sync
dd if="$dev" of="$work/want-inc.raw" bs=1M status=none
"$vmobj" backup --repo "$work/repo" --key-file "$work/k" --source "zfs:$vol" --vm zfs-e2e --type incremental --state-dir "$work/state"

step "vmobj left no snapshots or clones behind"
left=$(zfs list -H -t all -o name -r "$parent" | grep -E 'vmobj-[0-9]{8}T' || true)
[ -z "$left" ] || { echo "FAIL: left behind:"; echo "$left"; exit 1; }
echo "OK: none"

step "restore and compare"
mapfile -t names < <("$vmobj" list --repo "$work/repo" --key-file "$work/k" --vm zfs-e2e | sed -n 's/^  //p')
for name in "${names[@]}"; do
  case "$name" in *-full) want=full ;; *-incremental) want=inc ;; esac
  "$vmobj" restore --repo "$work/repo" --key-file "$work/k" --vm zfs-e2e --manifest "$name" --output "$work/got-$want.raw"
  cmp "$work/want-$want.raw" "$work/got-$want.raw"
  echo "OK: $name restores byte-identical"
done

printf '\nPASS: zfs end-to-end checks on %s\n' "$vol"
