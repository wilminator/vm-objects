#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Real-data test of incremental backups: back up a zvol's existing
# snapshots in order (a full, then one incremental each), then restore
# every backup and check it is byte-identical to its snapshot.
#
#   scripts/zfs-waypoints.sh backup ZVOL WORKDIR [VMOBJ_BINARY]
#   scripts/zfs-waypoints.sh add ZVOL@SNAPSHOT WORKDIR [VMOBJ_BINARY]
#   scripts/zfs-waypoints.sh verify WORKDIR [VMOBJ_BINARY]
#
# "add" backs up one more snapshot, newer than the last one backed up, as
# another incremental in an existing WORKDIR: for example a snapshot taken
# just before moving the zvol with scripts/zfs-replay.sh.
#
# Run as root. ZVOL is a zvol with two or more snapshots, for example
# tank/vms/win11; they are used oldest first. WORKDIR must not exist yet
# for "backup"; it receives a test key, a local repository, the state
# folder, hashes.txt (each snapshot's sha256) and backups.log. Put it on
# a different pool from the zvol if you can: reading and writing one pool
# at once is slower.
#
# Each snapshot is read through a read-only clone, ZVOL-wp, which the
# script creates and destroys; the snapshots themselves are never
# changed. The run reads the whole zvol twice per snapshot (hash, then
# backup), so it takes a while on large disks. "verify" restores each
# backup to WORKDIR/restore.raw, compares, and deletes it again, so it
# needs room for one restored disk (sparse: only its data).
#
# Found during milestone 4-6 testing: 8 snapshots of a 250 GiB Windows 11
# zvol, through activation, a feature update and driver changes, all
# restored identical.
set -uo pipefail

usage() { sed -n '12,14p' "$0" | sed 's/^# //' >&2; exit 2; }
[ $# -ge 2 ] || usage
cmd=$1; shift

backup() {
  local zvol=$1 work=$2 vmobj=${3:-vmobj}
  local clone="$zvol-wp" dev="/dev/zvol/$zvol-wp"
  set -e
  [ ! -e "$work" ] || { echo "$work already exists; pick a new folder" >&2; exit 1; }
  ! zfs list "$clone" >/dev/null 2>&1 || { echo "$clone already exists; remove it first" >&2; exit 1; }
  mapfile -t snaps < <(zfs list -H -t snapshot -o name -s creation -d 1 "$zvol" | sed 's/.*@//')
  [ "${#snaps[@]}" -ge 2 ] || { echo "$zvol needs at least two snapshots (has ${#snaps[@]})" >&2; exit 1; }
  trap 'zfs destroy "$clone" 2>/dev/null || true' EXIT
  mkdir -p "$work"
  "$vmobj" key init --key-file "$work/test.key"
  "$vmobj" init --repo "$work/repo" --key-file "$work/test.key"
  echo "${#snaps[@]} snapshots of $zvol, oldest first: ${snaps[*]}"
  local type=full
  for snap in "${snaps[@]}"; do
    echo "== $snap ($type)"
    zfs clone -o readonly=on "$zvol@$snap" "$clone"
    for _ in $(seq 30); do [ -e "$dev" ] && break; sleep 1; done
    [ -e "$dev" ] || { echo "$dev never appeared" >&2; exit 1; }
    echo "$snap $(sha256sum "$dev" | cut -d' ' -f1)" | tee -a "$work/hashes.txt"
    "$vmobj" backup --repo "$work/repo" --key-file "$work/test.key" --source "$dev" --format raw \
      --vm waypoints --type "$type" --state-dir "$work/state" --progress none 2>&1 | tee -a "$work/backups.log"
    zfs destroy "$clone"
    type=incremental
  done
  echo "Backed up ${#snaps[@]} snapshots. Next: $0 verify $work"
}

add() {
  local snapshot=$1 work=$2 vmobj=${3:-vmobj}
  local zvol=${snapshot%@*} snap=${snapshot#*@}
  local clone="$zvol-wp" dev="/dev/zvol/$zvol-wp"
  set -e
  [ "$zvol" != "$snapshot" ] || { echo "give ZVOL@SNAPSHOT" >&2; exit 1; }
  [ -f "$work/hashes.txt" ] || { echo "$work isn't a waypoint work folder (no hashes.txt)" >&2; exit 1; }
  ! grep -q "^$snap " "$work/hashes.txt" || { echo "$snap is already in $work/hashes.txt" >&2; exit 1; }
  zfs list "$snapshot" >/dev/null || exit 1
  ! zfs list "$clone" >/dev/null 2>&1 || { echo "$clone already exists; remove it first" >&2; exit 1; }
  trap 'zfs destroy "$clone" 2>/dev/null || true' EXIT
  echo "== $snap (incremental, after $(tail -n1 "$work/hashes.txt" | cut -d' ' -f1))"
  zfs clone -o readonly=on "$snapshot" "$clone"
  for _ in $(seq 30); do [ -e "$dev" ] && break; sleep 1; done
  [ -e "$dev" ] || { echo "$dev never appeared" >&2; exit 1; }
  local hash
  hash=$(sha256sum "$dev" | cut -d' ' -f1)
  "$vmobj" backup --repo "$work/repo" --key-file "$work/test.key" --source "$dev" --format raw \
    --vm waypoints --type incremental --state-dir "$work/state" --progress none 2>&1 | tee -a "$work/backups.log"
  # Recorded only once the backup succeeded, so hashes.txt and the backups stay in step.
  echo "$snap $hash" | tee -a "$work/hashes.txt"
  zfs destroy "$clone"
}

verify() {
  local work=$1 vmobj=${2:-vmobj}
  local names want fail=0
  mapfile -t names < <("$vmobj" list --repo "$work/repo" --key-file "$work/test.key" --vm waypoints | sed -n 's/^  //p')
  mapfile -t want < "$work/hashes.txt"
  if [ "${#names[@]}" -ne "${#want[@]}" ]; then
    echo "FAIL: ${#names[@]} backups but ${#want[@]} hashes" >&2; exit 1
  fi
  # Backup names start with their UTC time, so they sort in the order
  # they were made: the same order as hashes.txt.
  for i in "${!names[@]}"; do
    local snap=${want[$i]%% *} expect=${want[$i]##* } out="$work/restore.raw" got
    rm -f "$out"
    if ! "$vmobj" restore --repo "$work/repo" --key-file "$work/test.key" --vm waypoints \
        --manifest "${names[$i]}" --output "$out" --progress none >/dev/null 2>&1; then
      echo "FAIL $snap: restore of ${names[$i]} failed"; fail=1; continue
    fi
    got=$(sha256sum "$out" | cut -d' ' -f1)
    if [ "$got" = "$expect" ]; then echo "OK   $snap  ${names[$i]}"; else echo "FAIL $snap  ${names[$i]}  got $got"; fail=1; fi
    rm -f "$out"
  done
  if [ $fail -eq 0 ]; then echo "PASS: all ${#names[@]} restores match their snapshots"; else echo "FAIL: some restores did not match"; exit 1; fi
}

case $cmd in
  backup) [ $# -ge 2 ] || usage; backup "$@" ;;
  add) [ $# -ge 2 ] || usage; add "$@" ;;
  verify) verify "$@" ;;
  *) usage ;;
esac
