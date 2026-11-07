#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# End-to-end check of zfs-waypoints.sh and zfs-replay.sh together. Run as
# root on a machine with ZFS.
#
#   scripts/zfs-replay-e2e.sh PARENT_DATASET [VMOBJ_BINARY]
#
# Makes a 64 MiB zvol with three snapshots, backs them up with
# zfs-waypoints.sh, adds a fourth with "zfs-waypoints.sh add", rebuilds
# them all into a second zvol with zfs-replay.sh, and checks every
# replayed snapshot holds the same data as its original and that the later
# snapshots take only about what changed. Both zvols
# and the work folder are removed at the end.
set -euo pipefail
parent=${1:?usage: zfs-replay-e2e.sh PARENT_DATASET [VMOBJ_BINARY]}
vmobj=$(realpath "${2:-$(command -v vmobj)}")
here=$(cd "$(dirname "$0")" && pwd)
src="$parent/replay-src" dst="$parent/replay-dst"
work=$(mktemp -d)
cleanup() { zfs destroy -r "$src" 2>/dev/null || true; zfs destroy -r "$dst" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
step() { printf '\n== %s\n' "$*"; }
settle() { command -v udevadm >/dev/null && udevadm settle 2>/dev/null; true; }
wait_dev() { settle; for _ in $(seq 100); do [ -e "$1" ] && return; sleep 0.2; done; echo "FAIL: $1 never appeared"; exit 1; }
# The comparison clones reuse their names, so wait for each old link to go.
wait_gone() { settle; for _ in $(seq 100); do [ ! -e "$1" ] && return; sleep 0.2; done; echo "FAIL: $1 is still there"; exit 1; }

step "a zvol with three snapshots"
zfs create -V 64M -o volblocksize=16K "$src"
wait_dev "/dev/zvol/$src"
dd if=/dev/urandom of="/dev/zvol/$src" bs=1M count=24 oflag=direct status=none
zfs snapshot "$src@first"
dd if=/dev/urandom of="/dev/zvol/$src" bs=64K seek=100 count=8 oflag=direct status=none   # 512 KiB
zfs snapshot "$src@second"
dd if=/dev/urandom of="/dev/zvol/$src" bs=4K seek=9000 count=16 oflag=direct status=none  # 64 KiB
zfs snapshot "$src@third"

step "back them up"
"$here/zfs-waypoints.sh" backup "$src" "$work/wp" "$vmobj"

step "one more snapshot, added to the same work folder"
dd if=/dev/zero of="/dev/zvol/$src" bs=4K seek=200 count=4 oflag=direct status=none   # 16 KiB zeroed
zfs snapshot "$src@fourth"
"$here/zfs-waypoints.sh" add "$src@fourth" "$work/wp" "$vmobj"

step "replay into a new zvol"
"$here/zfs-replay.sh" --work "$work/wp" --zvol "$dst" --like "$src" --vmobj "$vmobj"

step "compare each snapshot with its original"
for s in first second third fourth; do
  a=$(zfs get -Hp -o value written "$src@$s") b=$(zfs get -Hp -o value written "$dst@$s")
  zfs clone -o readonly=on "$src@$s" "$parent/cmp-a"; zfs clone -o readonly=on "$dst@$s" "$parent/cmp-b"
  wait_dev "/dev/zvol/$parent/cmp-a"; wait_dev "/dev/zvol/$parent/cmp-b"
  if cmp -s "/dev/zvol/$parent/cmp-a" "/dev/zvol/$parent/cmp-b"; then same=yes; else same=no; fi
  zfs destroy "$parent/cmp-a"; zfs destroy "$parent/cmp-b"
  wait_gone "/dev/zvol/$parent/cmp-a"; wait_gone "/dev/zvol/$parent/cmp-b"
  [ "$same" = yes ] || { echo "FAIL: @$s differs from the original"; exit 1; }
  echo "   @$s identical; data written for it: original $((a / 1024)) KiB, replay $((b / 1024)) KiB"
  # Later snapshots must stay near the size of what changed, not the disk.
  if [ "$s" != first ] && [ "$b" -gt $(( a * 3 + 262144 )) ]; then
    echo "FAIL: the replayed @$s wrote far more than the original"; exit 1
  fi
done

step "resume skips what's done"
"$here/zfs-replay.sh" --work "$work/wp" --zvol "$dst" --like "$src" --vmobj "$vmobj" --resume | grep -c "already there, skipped" | grep -qx 4 \
  || { echo "FAIL: --resume didn't skip the 4 existing snapshots"; exit 1; }
echo "   ok"

printf '\nPASS: zfs replay end to end\n'
