#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Failure-safety checks for the vmobj binary: what happens when the
# repository is damaged, a run is killed or stopped part-way, the disk
# fills up, or a long chain of incrementals is restored from the middle.
#
#   scripts/hardening.sh [VMOBJ_BINARY] [TEST...]
#
# Every test works in its own temporary folder on a local repository, so it
# needs no S3 service, no root and no VM. Tests (all but diskfull by default):
#
#   corrupt-block     a flipped byte in a stored block
#   corrupt-pack      a flipped byte in an incremental's pack
#   corrupt-manifest  a flipped byte in a manifest
#   missing-block     a stored block deleted
#   kill-full         kill -9 during a full, then run it again
#   kill-incremental  kill -9 during an incremental, then run it again
#   stop-backup       one SIGTERM (same handler as Ctrl+C) during a full
#   stop-copy         one SIGTERM during a copy, then copy again
#   quit-copy         two SIGTERMs (quit at once) during a copy, then copy again
#   long-chain        a full and 20 incrementals; restore oldest, middle, newest
#   install           vmobj install, twice
#   diskfull          the repository's disk fills up (needs sudo: mounts a
#                     64 MiB tmpfs; run it by naming it)
#
# HARDENING_DISK_MIB sets the size of the test disk for the kill and stop
# tests (default 512). Raise it if a run finishes before it can be stopped,
# which the test reports as SKIP.
set -uo pipefail

vmobj=${1:-vmobj}
shift || true
if [[ $vmobj != */* ]]; then vmobj=$(command -v "$vmobj") || { echo "vmobj not found"; exit 2; }; fi
vmobj=$(cd "$(dirname "$vmobj")" && pwd)/$(basename "$vmobj")
disk_mib=${HARDENING_DISK_MIB:-512}

all=(corrupt-block corrupt-pack corrupt-manifest missing-block kill-full kill-incremental
     stop-backup stop-copy quit-copy long-chain install)
tests=("$@")
[ ${#tests[@]} -gt 0 ] || tests=("${all[@]}")

pass=0 fail=0 skip=0
results=()
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT

# ---- helpers ------------------------------------------------------------------

say() { printf '   %s\n' "$*"; }
ok() { say "ok: $*"; }
bad() { say "FAIL: $*"; return 1; }

# A fresh repository with small blocks in $w/repo, a key and a state folder.
# The repository is created with its default block size; 1 MiB blocks give
# the kill tests many objects to count.
setup() {
  w="$root/$1"
  mkdir -p "$w"
  key="$w/test.key" repo="$w/repo" state="$w/state" disk="$w/disk.raw"
  "$vmobj" key init --key-file "$key" >/dev/null
  "$vmobj" init --repo "$repo" --key-file "$key" --block-size 1MiB --extent-size 64KiB --pack-size 4MiB >/dev/null
}

# Random contents for a disk of $2 MiB at $1.
mkdisk() { head -c "$(( $2 * 1024 * 1024 ))" /dev/urandom > "$1"; }

# Overwrite 1 MiB of $disk at MiB offset $1 with random data.
scribble() { dd if=/dev/urandom of="$disk" bs=1M seek="$1" count=1 conv=notrunc status=none; }

backup() { # TYPE [extra args]
  local t=$1; shift
  "$vmobj" backup --repo "$repo" --key-file "$key" --source "$disk" --vm t --type "$t" --state-dir "$state" --progress none "$@"
}

names() { "$vmobj" list --repo "${1:-$repo}" --key-file "$key" --vm t | sed -n 's/^  //p'; }

# Backups of one kind. A backup made in the same second as another of its VM
# gets a -2, -3... suffix, so the kind isn't always at the end of the name.
kind() { names "${2:-}" | grep -E -- "-$1(-[0-9]+)?\$"; }

# Restore manifest $1 from repository $3 (default $repo) to $2 and compare it with file $4.
restore_matches() {
  local name=$1 out=$2 r=${3:-$repo} want=$4
  "$vmobj" restore --repo "$r" --key-file "$key" --vm t --manifest "$name" --output "$out" --progress none >/dev/null || { bad "restore of $name failed"; return 1; }
  cmp -s "$want" "$out" || { bad "$name restored, but differs from the disk it was taken from"; return 1; }
  rm -f "$out"
  ok "$name restores byte-identical"
}

# Flip one byte in the middle of file $1.
flip() {
  local f=$1 size off b
  size=$(stat -c %s "$f")
  off=$(( size / 2 ))
  b=$(od -An -tu1 -j "$off" -N1 "$f" | tr -d ' ')
  printf '%b' "\\$(printf '%03o' $(( b ^ 0xFF )))" | dd of="$f" bs=1 seek="$off" conv=notrunc status=none
}

count_objects() { find "$1" -type f 2>/dev/null | wc -l; }

# Start "$@" in the background, wait until folder $1 holds at least $2 more
# files than it did, then send signals. Sets $pid and $code. Returns 2 if the
# command finished before it could be stopped.
run_and_signal() { # DIR THRESHOLD SIGNALS... -- COMMAND...
  local dir=$1 threshold=$2; shift 2
  local sigs=()
  while [ "$1" != "--" ]; do sigs+=("$1"); shift; done
  shift
  local before; before=$(count_objects "$dir")
  "$@" > "$w/run.out" 2>&1 &
  pid=$!
  local i
  for (( i = 0; i < 6000; i++ )); do
    kill -0 "$pid" 2>/dev/null || break
    [ "$(count_objects "$dir")" -ge $(( before + threshold )) ] && break
    sleep 0.01
  done
  if ! kill -0 "$pid" 2>/dev/null; then
    wait "$pid"; code=$?
    return 2
  fi
  for s in "${sigs[@]}"; do kill -"$s" "$pid" 2>/dev/null; sleep 0.05; done
  wait "$pid"; code=$?
  return 0
}

no_restore_leftovers() {
  local dir=$1
  if compgen -G "$dir/.*.vmobj-restore-*" >/dev/null; then
    bad "a temporary restore file was left behind in $dir"; return 1
  fi
}

# ---- the tests ----------------------------------------------------------------

t_corrupt_block() {
  setup corrupt-block
  mkdisk "$disk" 16
  backup full >/dev/null || { bad "full backup failed"; return 1; }
  local name; name=$(names)
  local obj; obj=$(find "$repo/blocks" -type f | head -n1)
  flip "$obj"
  say "flipped one byte in ${obj#"$repo"/}"
  if "$vmobj" verify --repo "$repo" --key-file "$key" --vm t --manifest "$name" --deep >"$w/verify.out" 2>&1; then
    bad "verify --deep passed a damaged block"; return 1
  fi
  ok "verify --deep refused: $(tail -n1 "$w/verify.out")"
  if "$vmobj" restore --repo "$repo" --key-file "$key" --vm t --manifest "$name" --output "$w/out.raw" --progress none >"$w/restore.out" 2>&1; then
    bad "restore succeeded from a damaged block"; return 1
  fi
  ok "restore refused: $(tail -n1 "$w/restore.out")"
  [ ! -e "$w/out.raw" ] || { bad "a partial out.raw was left under the real name"; return 1; }
  no_restore_leftovers "$w" || return 1
  ok "nothing was written under the output name, and no temporary file was left"
}

t_corrupt_pack() {
  setup corrupt-pack
  mkdisk "$disk" 16
  backup full >/dev/null || { bad "full backup failed"; return 1; }
  cp "$disk" "$w/want-full.raw"
  scribble 3; scribble 9
  backup incremental >/dev/null || { bad "incremental backup failed"; return 1; }
  local full inc; full=$(kind full); inc=$(kind incremental)
  local pack; pack=$(find "$repo/packs" -name '*.pack' | head -n1)
  flip "$pack"
  say "flipped one byte in ${pack#"$repo"/}"
  if "$vmobj" restore --repo "$repo" --key-file "$key" --vm t --manifest "$inc" --output "$w/inc.raw" --progress none >"$w/restore.out" 2>&1; then
    bad "the incremental restored from a damaged pack"; return 1
  fi
  ok "incremental restore refused: $(tail -n1 "$w/restore.out")"
  [ ! -e "$w/inc.raw" ] || { bad "a partial inc.raw was left"; return 1; }
  restore_matches "$full" "$w/full.raw" "$repo" "$w/want-full.raw" || return 1
  ok "the damage is contained: the full, which doesn't use the pack, still restores"
}

t_corrupt_manifest() {
  setup corrupt-manifest
  mkdisk "$disk" 16
  backup full >/dev/null || { bad "full backup failed"; return 1; }
  cp "$disk" "$w/want-full.raw"
  scribble 2
  backup incremental >/dev/null || { bad "incremental backup failed"; return 1; }
  local full inc; full=$(kind full); inc=$(kind incremental)
  flip "$repo/manifests/t/$inc.manifest"
  say "flipped one byte in manifests/t/$inc.manifest"
  if "$vmobj" restore --repo "$repo" --key-file "$key" --vm t --manifest "$inc" --output "$w/inc.raw" --progress none >"$w/restore.out" 2>&1; then
    bad "restore succeeded from a damaged manifest"; return 1
  fi
  ok "restore refused: $(tail -n1 "$w/restore.out")"
  restore_matches "$full" "$w/full.raw" "$repo" "$w/want-full.raw" || return 1
}

t_missing_block() {
  setup missing-block
  mkdisk "$disk" 16
  backup full >/dev/null || { bad "full backup failed"; return 1; }
  local name; name=$(names)
  local obj; obj=$(find "$repo/blocks" -type f | head -n1)
  rm -f "$obj"
  say "deleted ${obj#"$repo"/}"
  if "$vmobj" verify --repo "$repo" --key-file "$key" --vm t --manifest "$name" >"$w/verify.out" 2>&1; then
    bad "verify (without --deep) passed with a block missing"; return 1
  fi
  ok "verify refused: $(tail -n1 "$w/verify.out")"
}

t_kill_full() {
  setup kill-full
  mkdisk "$disk" "$disk_mib"
  run_and_signal "$repo/blocks" 20 KILL -- "$vmobj" backup --repo "$repo" --key-file "$key" --source "$disk" --vm t --type full --state-dir "$state" --progress none
  [ $? -eq 2 ] && { say "SKIP: the full finished before it could be killed; raise HARDENING_DISK_MIB"; return 3; }
  say "killed with -9 after $(count_objects "$repo/blocks") blocks were stored (exit $code)"
  [ -z "$(names)" ] || { bad "the killed full left a backup in the list"; return 1; }
  ok "no backup recorded for the killed run"
  backup full >"$w/again.out" 2>&1 || { bad "the second full failed: $(tail -n1 "$w/again.out")"; return 1; }
  ok "the full ran again and finished"
  local name; name=$(names)
  "$vmobj" verify --repo "$repo" --key-file "$key" --vm t --manifest "$name" --deep >/dev/null || { bad "verify --deep failed"; return 1; }
  restore_matches "$name" "$w/out.raw" "$repo" "$disk" || return 1
  # Prune never touches objects younger than an hour (--grace has a 1h
  # floor), so the leftovers of the killed run can't be removed yet. What
  # this shows is that prune --apply right after an interruption deletes
  # nothing the finished backup needs.
  "$vmobj" prune --repo "$repo" --key-file "$key" --apply >"$w/prune.out" || { bad "prune failed"; return 1; }
  say "prune --apply: $(grep -E 'Unreferenced|Too recent|Deleted' "$w/prune.out" | tr -s ' ' | paste -sd ';')"
  "$vmobj" verify --repo "$repo" --key-file "$key" --vm t --manifest "$name" --deep >/dev/null || { bad "verify --deep failed after prune"; return 1; }
  restore_matches "$name" "$w/out.raw" "$repo" "$disk" || return 1
  ok "prune after the interruption kept everything the backup needs"
}

t_kill_incremental() {
  setup kill-incremental
  mkdisk "$disk" "$disk_mib"
  backup full >/dev/null || { bad "full backup failed"; return 1; }
  # Change a lot, so the incremental writes many packs and can be caught mid-run.
  local m
  for (( m = 0; m < disk_mib; m += 2 )); do scribble "$m"; done
  run_and_signal "$repo/packs" 4 KILL -- "$vmobj" backup --repo "$repo" --key-file "$key" --source "$disk" --vm t --type incremental --state-dir "$state" --progress none
  [ $? -eq 2 ] && { say "SKIP: the incremental finished before it could be killed; raise HARDENING_DISK_MIB"; return 3; }
  say "killed with -9 after $(count_objects "$repo/packs") pack files were stored (exit $code)"
  [ "$(kind incremental | grep -c .)" -eq 0 ] || { bad "the killed incremental left a backup in the list"; return 1; }
  ok "no incremental recorded for the killed run"
  cp "$disk" "$w/want-1.raw"
  backup incremental >"$w/again.out" 2>&1 || { bad "the incremental failed when run again: $(tail -n1 "$w/again.out")"; return 1; }
  ok "the incremental ran again and finished"
  scribble 1
  cp "$disk" "$w/want-2.raw"
  backup incremental >/dev/null || { bad "the next incremental failed"; return 1; }
  local incs; mapfile -t incs < <(kind incremental)
  [ ${#incs[@]} -eq 2 ] || { bad "expected 2 incrementals, found ${#incs[@]}"; return 1; }
  restore_matches "${incs[0]}" "$w/out.raw" "$repo" "$w/want-1.raw" || return 1
  restore_matches "${incs[1]}" "$w/out.raw" "$repo" "$w/want-2.raw" || return 1
  ok "the host state stayed consistent across the kill"
}

t_stop_backup() {
  setup stop-backup
  mkdisk "$disk" "$disk_mib"
  run_and_signal "$repo/blocks" 20 TERM -- "$vmobj" backup --repo "$repo" --key-file "$key" --source "$disk" --vm t --type full --state-dir "$state" --progress none
  [ $? -eq 2 ] && { say "SKIP: the full finished before it could be stopped; raise HARDENING_DISK_MIB"; return 3; }
  [ "$code" -eq 130 ] || { bad "expected exit 130 after one stop signal, got $code: $(tail -n1 "$w/run.out")"; return 1; }
  ok "stopped cleanly with exit 130"
  grep -q "Stopping after the current block" "$w/run.out" || { bad "the stop message wasn't printed"; return 1; }
  grep -q "stopped before finishing" "$w/run.out" || { bad "the closing message wasn't printed"; return 1; }
  ok "both messages printed"
  [ -z "$(names)" ] || { bad "the stopped full left a backup in the list"; return 1; }
  ok "no backup recorded"
  backup full >/dev/null || { bad "the full failed when run again"; return 1; }
  restore_matches "$(names)" "$w/out.raw" "$repo" "$disk" || return 1
}

# Shared by stop-copy and quit-copy: $1 is the signal list, $2 the exit code wanted.
copy_interrupted() {
  mkdisk "$disk" "$disk_mib"
  backup full >/dev/null || { bad "full backup failed"; return 1; }
  local name; name=$(names)
  local dst="$w/copy"
  "$vmobj" init --repo "$dst" --key-file "$key" --block-size 1MiB --extent-size 64KiB --pack-size 4MiB >/dev/null
  # shellcheck disable=SC2086
  run_and_signal "$dst/blocks" 20 $1 -- "$vmobj" copy --from "$repo" --to "$dst" --key-file "$key" --profile standard --progress none
  [ $? -eq 2 ] && { say "SKIP: the copy finished before it could be stopped; raise HARDENING_DISK_MIB"; return 3; }
  case " $2 " in *" $code "*) ;; *) bad "expected exit $2, got $code: $(tail -n1 "$w/run.out")"; return 1 ;; esac
  ok "the copy stopped with exit $code after $(count_objects "$dst/blocks") blocks"
  [ -z "$(names "$dst")" ] || { bad "the interrupted copy recorded the backup at the destination"; return 1; }
  ok "the destination lists no backup yet"
  "$vmobj" copy --from "$repo" --to "$dst" --key-file "$key" --profile standard --progress none >"$w/copy2.out" 2>&1 || { bad "the second copy failed: $(tail -n1 "$w/copy2.out")"; return 1; }
  grep -q "Copied:    1 backups" "$w/copy2.out" || { bad "the second copy didn't report 1 backup copied"; return 1; }
  ok "the copy ran again and finished"
  restore_matches "$name" "$w/out.raw" "$dst" "$disk" || return 1
}

t_stop_copy() { setup stop-copy; copy_interrupted TERM 130; }
# The second signal normally arrives after vmobj has reset its handler, so
# the process dies of SIGTERM itself: exit 143 (128 + 15). On a fast local
# disk the clean stop can finish within the 50 ms gap first (exit 130).
# Either way, what matters is that the next copy completes.
t_quit_copy() { setup quit-copy; copy_interrupted "TERM TERM" "143 130"; }

t_long_chain() {
  setup long-chain
  mkdisk "$disk" 64
  backup full >/dev/null || { bad "full backup failed"; return 1; }
  cp "$disk" "$w/want-0.raw"
  local i
  for (( i = 1; i <= 20; i++ )); do
    scribble $(( (i * 7) % 64 ))
    backup incremental >/dev/null || { bad "incremental $i failed"; return 1; }
    cp "$disk" "$w/want-$i.raw"
  done
  local incs; mapfile -t incs < <(kind incremental)
  [ ${#incs[@]} -eq 20 ] || { bad "expected 20 incrementals, found ${#incs[@]}"; return 1; }
  ok "a full and 20 incrementals"
  restore_matches "${incs[0]}" "$w/out.raw" "$repo" "$w/want-1.raw" || return 1
  restore_matches "${incs[9]}" "$w/out.raw" "$repo" "$w/want-10.raw" || return 1
  restore_matches "${incs[19]}" "$w/out.raw" "$repo" "$w/want-20.raw" || return 1
  restore_matches "$(kind full)" "$w/out.raw" "$repo" "$w/want-0.raw" || return 1
}

t_install() {
  w="$root/install"; mkdir -p "$w/bin"
  "$vmobj" install "$w/bin" >/dev/null || { bad "install failed"; return 1; }
  cmp -s "$vmobj" "$w/bin/vmobj" || { bad "the installed file differs from the binary"; return 1; }
  [ "$(stat -c %a "$w/bin/vmobj")" = 755 ] || { bad "mode is $(stat -c %a "$w/bin/vmobj"), not 755"; return 1; }
  ok "installed, identical, mode 755"
  local before; before=$(stat -c %Y.%i "$w/bin/vmobj")
  sleep 1
  "$vmobj" install "$w/bin" >/dev/null || { bad "the second install failed"; return 1; }
  [ "$(stat -c %Y.%i "$w/bin/vmobj")" = "$before" ] || { bad "the second install rewrote an identical file"; return 1; }
  ok "the second install left the identical file alone"
  if ! "$vmobj" install --name vmobj-test "$w/bin" >/dev/null || [ ! -x "$w/bin/vmobj-test" ]; then
    bad "install --name failed"; return 1
  fi
  ok "--name works"
}

t_diskfull() {
  setup diskfull
  local mnt="$w/small"
  mkdir -p "$mnt"
  sudo mount -t tmpfs -o size=64m,uid="$(id -u)",gid="$(id -g)" tmpfs "$mnt" || { bad "couldn't mount a tmpfs (needs sudo)"; return 1; }
  # shellcheck disable=SC2064
  trap "sudo umount '$mnt' 2>/dev/null; rm -rf '$root'" EXIT
  repo="$mnt/repo"
  "$vmobj" init --repo "$repo" --key-file "$key" --block-size 1MiB --extent-size 64KiB --pack-size 4MiB >/dev/null
  mkdisk "$disk" 16
  backup full >/dev/null || { bad "the first, small full failed"; return 1; }
  local first; first=$(names)
  cp "$disk" "$w/want-first.raw"
  mkdisk "$disk" 128
  rm -rf "$state"
  if backup full >"$w/full.out" 2>&1; then
    bad "a 128 MiB full fit into a 64 MiB disk?"; return 1
  fi
  ok "the full failed: $(tail -n1 "$w/full.out")"
  grep -qi "no space" "$w/full.out" || say "note: the error doesn't say 'no space left'; check it reads clearly"
  [ "$(names | wc -l)" -eq 1 ] || { bad "the failed full recorded a backup"; return 1; }
  "$vmobj" verify --repo "$repo" --key-file "$key" --vm t --manifest "$first" --deep >/dev/null || { bad "the earlier backup no longer verifies"; return 1; }
  restore_matches "$first" "$w/out.raw" "$repo" "$w/want-first.raw" || return 1
  ok "the earlier backup is intact"
  sudo umount "$mnt"
  trap 'rm -rf "$root"' EXIT
}

# ---- run ----------------------------------------------------------------------

echo "vmobj: $vmobj ($("$vmobj" version))"
for t in "${tests[@]}"; do
  fn="t_${t//-/_}"
  if ! declare -F "$fn" >/dev/null; then echo "unknown test: $t"; exit 2; fi
  printf '\n== %s\n' "$t"
  "$fn"; rc=$?
  case $rc in
    0) pass=$((pass + 1)); results+=("PASS  $t") ;;
    3) skip=$((skip + 1)); results+=("SKIP  $t") ;;
    *) fail=$((fail + 1)); results+=("FAIL  $t") ;;
  esac
done

printf '\n== summary\n'
printf '   %s\n' "${results[@]}"
printf '\n%d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ]
