#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# End-to-end check of restore --mode copy and in-place with the real
# binary and the default 4 MiB blocks.
#
#   scripts/restore-modes-e2e.sh [VMOBJ_BINARY]
#
# It makes a 64 MiB disk, a full and three incrementals, then steps one
# target through the restore points in place (forwards, backwards, and
# again with nothing to do), checks the write counts, updates a file in
# copy mode and checks its inode is kept. Run as root (or with sudo
# available), it repeats the in-place steps on a loop device, the same
# path a zvol takes. Scratch files go in a temporary folder.
set -euo pipefail
vmobj=$(realpath "${1:-$(command -v vmobj)}")
w=$(mktemp -d)
loop=""
cleanup() { [ -n "$loop" ] && $sudo losetup -d "$loop" 2>/dev/null; rm -rf "$w"; }
sudo=""
[ "$(id -u)" -eq 0 ] || sudo=sudo
trap cleanup EXIT
cd "$w"

step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*"; exit 1; }
scribble() { dd if=/dev/urandom of=disk.raw bs=4k seek="$1" count="${2:-1}" conv=notrunc status=none; }
restore() { "$vmobj" restore --repo repo --key-file k --vm d --progress none "$@"; }
# The number on a summary line: restore ... | field Written
field() { sed -n "s/^  $1: *\\([0-9.]*\\) \\([KMG]\\?i\\?B\\).*/\\1 \\2/p"; }

step "a disk, a full and three incrementals"
"$vmobj" key init --key-file k >/dev/null
"$vmobj" init --repo repo --key-file k >/dev/null
head -c 48M /dev/urandom > disk.raw && truncate -s 64M disk.raw
b() { "$vmobj" backup --repo repo --key-file k --source disk.raw --vm d --type "$1" --state-dir st --progress none >/dev/null; cp disk.raw "want-$2.raw"; }
b full 0
scribble 10 3; scribble 5000; scribble 15000 2   # 6 pieces of 4 KiB
b incremental 1
scribble 2000                                     # 1 more
b incremental 2
dd if=/dev/zero of=disk.raw bs=4k seek=10 count=3 conv=notrunc status=none   # zero 3 again
b incremental 3
mapfile -t names < <("$vmobj" list --repo repo --key-file k --vm d | sed -n 's/^  //p')
[ ${#names[@]} -eq 4 ] || fail "expected 4 backups, got ${#names[@]}"
echo "   ${names[*]}"

# check_steps TARGET: walks TARGET through the restore points, in place.
check_steps() {
  local t=$1 i out
  for i in 0 1 2 3 1 0; do
    out=$(restore --manifest "${names[$i]}" --output "$t" --mode in-place --check 2>&1) || { echo "$out"; fail "in-place to backup $i"; }
    $sudo cmp -s "want-$i.raw" "$t" || fail "after the in-place step to backup $i the target differs"
    echo "   -> backup $i: $(field Written <<<"$out") written; $(grep -c 'every block matches' <<<"$out") check"
  done
  out=$(restore --manifest "${names[0]}" --output "$t" --mode in-place 2>&1)
  grep -q "Written:   0 B" <<<"$out" || { echo "$out"; fail "a repeat in-place restore wrote something"; }
  echo "   a repeat writes nothing"
}

step "in place on a file, through every restore point and back"
restore --manifest "${names[0]}" --output target.raw >/dev/null   # rename mode makes the first copy
check_steps target.raw
out=$(restore --manifest "${names[1]}" --output target.raw --mode in-place)
grep -q "Written:   24.00 KiB" <<<"$out" || { echo "$out"; fail "full -> incremental 1 should write exactly the 6 changed 4 KiB pieces"; }
echo "   full -> incremental 1 wrote exactly the 6 changed pieces (24 KiB)"

step "copy mode keeps the file and writes only what differs"
restore --manifest "${names[0]}" --output target.raw --mode in-place >/dev/null
ino=$(stat -c %i target.raw)
mkdir scratch
out=$(restore --manifest "${names[2]}" --output target.raw --mode copy --scratch scratch --check)
cmp -s want-2.raw target.raw || fail "copy mode result differs"
[ "$(stat -c %i target.raw)" = "$ino" ] || fail "copy mode replaced the file"
grep -q "Written:   28.00 KiB" <<<"$out" || { echo "$out"; fail "copy mode should write 7 pieces (28 KiB)"; }
[ -z "$(ls -A scratch)" ] || fail "the scratch copy was left behind"
echo "   same inode, 28 KiB written, scratch emptied"

step "refusals"
restore --manifest "${names[0]}" --output missing.raw --mode in-place >/dev/null 2>&1 && fail "in-place onto a missing file worked"
restore --manifest "${names[0]}" --output r.raw --check >/dev/null 2>&1 && fail "--check with rename mode worked"
echo "   ok"

if [ -z "$sudo" ] || sudo -n true 2>/dev/null; then
  step "in place on a block device (loop)"
  truncate -s 64M dev.img
  loop=$($sudo losetup --find --show dev.img)
  echo "   $loop"
  restore() { $sudo "$vmobj" restore --repo repo --key-file k --vm d --progress none "$@"; }
  check_steps "$loop"
else
  echo; echo "(skipping the block device test: needs root or sudo)"
fi

printf '\nPASS: restore modes end to end\n'
