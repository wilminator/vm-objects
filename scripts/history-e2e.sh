#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# End-to-end check of snapshot history (milestone 9) on real ZFS: "backup
# --history" and "restore-history". Run as root on a machine with ZFS.
#
#   scripts/history-e2e.sh PARENT_DATASET [VMOBJ_BINARY]
#
# Makes a 64 MiB zvol with three snapshots and backs up its history, adds
# a fourth and checks only that one is backed up, rebuilds the history in
# a second zvol and compares every snapshot with its original (data, size
# of what each wrote, and the original date in vmobj:created), backs the
# rebuilt zvol up again to check the dates carry over, then replays with
# --verify end, staged, and --resume. Everything it makes is removed at
# the end.
set -euo pipefail
parent=${1:?usage: history-e2e.sh PARENT_DATASET [VMOBJ_BINARY]}
vmobj=$(realpath "${2:-$(command -v vmobj)}")
src="$parent/hist-src" dst="$parent/hist-dst"
work=$(mktemp -d)
export TZ=UTC # vmobj prints local times; keep them comparable
cleanup() { for d in "$src" "$dst" "$dst-end" "$dst-staged"; do zfs destroy -r "$d" 2>/dev/null || true; done; rm -rf "$work"; }
trap cleanup EXIT
step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*"; exit 1; }
settle() { command -v udevadm >/dev/null && udevadm settle 2>/dev/null; true; }
wait_dev() { settle; for _ in $(seq 100); do [ -e "$1" ] && return; sleep 0.2; done; fail "$1 never appeared"; }
wait_gone() { settle; for _ in $(seq 100); do [ ! -e "$1" ] && return; sleep 0.2; done; fail "$1 is still there"; }
v() { "$vmobj" "$@" --repo "$work/repo" --key-file "$work/k"; }

step "a zvol with three snapshots, a second apart"
zfs create -V 64M -o volblocksize=16K "$src"
wait_dev "/dev/zvol/$src"
dd if=/dev/urandom of="/dev/zvol/$src" bs=1M count=24 oflag=direct status=none
zfs snapshot "$src@first"; sleep 1.1
dd if=/dev/urandom of="/dev/zvol/$src" bs=64K seek=100 count=8 oflag=direct status=none   # 512 KiB
zfs snapshot "$src@second"; sleep 1.1
dd if=/dev/urandom of="/dev/zvol/$src" bs=4K seek=9000 count=16 oflag=direct status=none  # 64 KiB
zfs snapshot "$src@third"
"$vmobj" key init --key-file "$work/k"
"$vmobj" init --repo "$work/repo" --key-file "$work/k"
st="--state-dir $work/state --progress none"

step "back up the history"
# shellcheck disable=SC2086
v backup --history --source "zfs:$src" --vm disk --hash $st | tee "$work/b1.log"
grep -q "3 snapshots backed up, 0 already there" "$work/b1.log" || fail "expected 3 new snapshots"
v list --vm disk --long | tee "$work/list.log"
for s in first second third; do grep -q "@$s (" "$work/list.log" || fail "list --long doesn't show @$s"; done
zfs list -H -o name -t snapshot -r "$src" | grep -q '@vmobj-' && fail "a vmobj snapshot was left behind"
zfs list "$src@first" >/dev/null || fail "an original snapshot is gone"

step "a fourth snapshot: only it is backed up"
dd if=/dev/zero of="/dev/zvol/$src" bs=4K seek=200 count=4 oflag=direct status=none   # 16 KiB zeroed
zfs snapshot "$src@fourth"
# shellcheck disable=SC2086
v backup --history --source "zfs:$src" --vm disk --hash $st | tee "$work/b2.log"
grep -q "1 snapshots backed up, 3 already there" "$work/b2.log" || fail "expected 1 new, 3 skipped"

step "one existing snapshot on its own"
# shellcheck disable=SC2086
v backup --source "zfs:$src@second" --vm single --type full $st
zfs list "$src@second" >/dev/null || fail "backing up @second destroyed it"
v list --vm single --long | grep -q "@second (" || fail "the single backup has no snapshot record"

step "rebuild the history in a new zvol"
v restore-history --vm disk --zvol "$dst" --like "$src" --progress none | tee "$work/r1.log"
grep -q "holds all 4 snapshots" "$work/r1.log" || fail "replay didn't finish"
[ "$(grep -c 'sha256 matches the original' "$work/r1.log")" -eq 4 ] || fail "not every step was proven by its sha256"

step "compare each snapshot with its original"
for s in first second third fourth; do
  a=$(zfs get -Hp -o value written "$src@$s") b=$(zfs get -Hp -o value written "$dst@$s")
  want=$(date -u -d "@$(zfs get -Hp -o value creation "$src@$s")" +%Y-%m-%dT%H:%M:%SZ)
  got=$(zfs get -H -o value vmobj:created "$dst@$s")
  [ "$got" = "$want" ] || fail "@$s: vmobj:created is $got, the original was taken $want"
  zfs clone -o readonly=on "$src@$s" "$parent/hcmp-a"; zfs clone -o readonly=on "$dst@$s" "$parent/hcmp-b"
  wait_dev "/dev/zvol/$parent/hcmp-a"; wait_dev "/dev/zvol/$parent/hcmp-b"
  if cmp -s "/dev/zvol/$parent/hcmp-a" "/dev/zvol/$parent/hcmp-b"; then same=yes; else same=no; fi
  zfs destroy "$parent/hcmp-a"; zfs destroy "$parent/hcmp-b"
  wait_gone "/dev/zvol/$parent/hcmp-a"; wait_gone "/dev/zvol/$parent/hcmp-b"
  [ "$same" = yes ] || fail "@$s differs from the original"
  echo "   @$s identical, dated $got; written: original $((a / 1024)) KiB, replay $((b / 1024)) KiB"
  if [ "$s" != first ] && [ "$b" -gt $(( a * 3 + 262144 )) ]; then fail "the replayed @$s wrote far more than the original"; fi
done

step "back up the rebuilt zvol: the original dates carry over"
# shellcheck disable=SC2086
v backup --history --source "zfs:$dst" --vm again $st >/dev/null
v restore-history --vm disk --list | sed -n 's/^  \([^ ]*\) *\([0-9-]* [0-9:]*\).*/\1 \2/p' > "$work/d1"
v restore-history --vm again --list | sed -n 's/^  \([^ ]*\) *\([0-9-]* [0-9:]*\).*/\1 \2/p' > "$work/d2"
[ -s "$work/d1" ] || fail "restore-history --list printed no history"
diff "$work/d1" "$work/d2" || fail "the rebuilt zvol's history has other names or dates"
echo "   ok: same names and dates"

step "replay again with --verify end"
v restore-history --vm disk --zvol "$dst-end" --like "$src" --verify end --progress none >/dev/null
for s in first second third fourth; do zfs list "$dst-end@$s" >/dev/null || fail "--verify end lost @$s"; done
echo "   ok"
zfs destroy -r "$dst-end"

step "replay again, staged"
mkdir -p "$work/journal"
v restore-history --vm disk --zvol "$dst-staged" --like "$src" --staged "$work/journal" --progress none >/dev/null
[ -z "$(ls -A "$work/journal")" ] || fail "a journal was left behind"
echo "   ok: no journal left"
zfs destroy -r "$dst-staged"

step "an existing zvol is refused without --resume; with it, done snapshots are skipped"
if v restore-history --vm disk --zvol "$dst" --like "$src" --progress none >/dev/null 2>&1; then fail "replayed into an existing zvol"; fi
zfs destroy "$dst@fourth"
v restore-history --vm disk --zvol "$dst" --resume --progress none | tee "$work/r2.log"
[ "$(grep -c 'skipped (already there)' "$work/r2.log")" -eq 3 ] || fail "--resume didn't skip the 3 snapshots there"
zfs list "$dst@fourth" >/dev/null || fail "--resume didn't recreate @fourth"

printf '\nPASS: snapshot history end to end\n'
