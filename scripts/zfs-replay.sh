#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Rebuilds a zvol, snapshot history included, from vmobj backups. Run with
# --help for the details.
set -uo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/zfs-replay.sh --work WORKDIR --zvol POOL/NEW --like POOL/OLD [options]

Rebuilds a zvol and its snapshots from a chain of vmobj backups, such as
the ones scripts/zfs-waypoints.sh makes: one backup per snapshot, oldest
first. For each backup, in order, it restores into the new zvol with
"vmobj restore --mode in-place --check", which writes only what changed
since the previous one, checks the result against the snapshot's sha256
(when known), and takes a snapshot of the same name. So each new snapshot
holds only what really changed, like the original's.

Use it to move a VM disk to another pool, or another machine, with vmobj
and keep its named snapshots. The snapshots get today's date: ZFS sets the
creation time.

Options:
  --work DIR        a zfs-waypoints.sh work folder: its repository (repo),
                    key (test.key), backups of VM "waypoints" and
                    hashes.txt (snapshot names and sha256s, in order).
                    A repository with just one series uses it, whatever
                    its name; --vm NAME picks one otherwise.
  --repo, --key-file, --vm, --names "a b c"
                    instead of --work: any repository, the disk's backup
                    series, and one snapshot name per backup, oldest first
                    (no sha256 check then)
  --zvol POOL/NEW   the zvol to create (must not exist, unless --resume)
  --like POOL/OLD   copy volsize, volblocksize, compression and whether
                    it's sparse from this zvol
  --volsize SIZE, --volblocksize SIZE
                    instead of --like
  --sparse          create it sparse (no reservation)
  --resume          continue a replay that stopped: skip the snapshots
                    the new zvol already has
  --vmobj PATH      vmobj binary (default: vmobj on PATH)
  -h, --help        this text

Run as root. Each step reads the whole zvol three times (compare, check,
sha256), so a 100 GiB disk takes a few minutes per snapshot on SSDs.
EOF
}

work="" repo="" key="" vm="" names_arg="" zvol="" like="" volsize="" volblock="" sparse=false resume=false vmobj=vmobj
while [ $# -gt 0 ]; do
  case $1 in
    --work) work=$2; shift 2 ;;
    --repo) repo=$2; shift 2 ;;
    --key-file) key=$2; shift 2 ;;
    --vm) vm=$2; shift 2 ;;
    --names) names_arg=$2; shift 2 ;;
    --zvol) zvol=$2; shift 2 ;;
    --like) like=$2; shift 2 ;;
    --volsize) volsize=$2; shift 2 ;;
    --volblocksize) volblock=$2; shift 2 ;;
    --sparse) sparse=true; shift ;;
    --resume) resume=true; shift ;;
    --vmobj) vmobj=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

die() { echo "zfs-replay: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

[ -n "$zvol" ] || die "--zvol is required (see --help)"
command -v "$vmobj" >/dev/null || die "vmobj not found: $vmobj"
vmobj=$(command -v "$vmobj")
compression=""

# ---- the backups and the snapshot names they become --------------------------

snaps=() hashes=()
if [ -n "$work" ]; then
  repo="$work/repo" key="$work/test.key"
  if [ -z "$vm" ]; then
    # The repository's only series, whatever it's called; else "waypoints".
    mapfile -t found < <("$vmobj" list --repo "$repo" --key-file "$key" | sed -n 's/^\([^ ].*\):$/\1/p')
    if [ ${#found[@]} -eq 1 ]; then vm=${found[0]}; else vm=waypoints; fi
  fi
  echo "backup series: $vm"
  [ -f "$work/hashes.txt" ] || die "$work/hashes.txt not found; is this a zfs-waypoints.sh work folder?"
  while read -r s h; do snaps+=("$s"); hashes+=("$h"); done < "$work/hashes.txt"
else
  if [ -z "$repo" ] || [ -z "$key" ] || [ -z "$vm" ] || [ -z "$names_arg" ]; then die "give --work, or all of --repo, --key-file, --vm and --names"; fi
  read -ra snaps <<<"$names_arg"
fi
mapfile -t manifests < <("$vmobj" list --repo "$repo" --key-file "$key" --vm "$vm" | sed -n 's/^  //p')
[ ${#manifests[@]} -eq ${#snaps[@]} ] || die "${#manifests[@]} backups of $vm but ${#snaps[@]} snapshot names"
step "plan"
for i in "${!snaps[@]}"; do printf '   %-40s -> %s@%s\n' "${manifests[$i]}" "$zvol" "${snaps[$i]}"; done

# ---- the new zvol ---------------------------------------------------------------

dev="/dev/zvol/$zvol"
if zfs list "$zvol" >/dev/null 2>&1; then
  $resume || die "$zvol already exists. Pick another name, or add --resume to continue an earlier replay into it."
  echo "   resuming into $zvol"
else
  if [ -n "$like" ]; then
    zfs list "$like" >/dev/null 2>&1 || die "--like $like: no such zvol"
    volsize=$(zfs get -Hp -o value volsize "$like")
    volblock=$(zfs get -Hp -o value volblocksize "$like")
    compression=$(zfs get -H -o value compression "$like")
    [ "$(zfs get -H -o value refreservation "$like")" = none ] && sparse=true
  fi
  [ -n "$volsize" ] || die "give --like, or --volsize"
  args=(create -V "$volsize")
  $sparse && args+=(-s)
  [ -n "$volblock" ] && args+=(-o "volblocksize=$volblock")
  [ -n "$compression" ] && args+=(-o "compression=$compression")
  step "creating $zvol"
  echo "   zfs ${args[*]} $zvol"
  zfs "${args[@]}" "$zvol" || die "zfs create failed"
fi
for _ in $(seq 100); do [ -e "$dev" ] && break; sleep 0.2; done
[ -e "$dev" ] || die "$dev never appeared"

# ---- replay -------------------------------------------------------------------

for i in "${!snaps[@]}"; do
  snap=${snaps[$i]} name=${manifests[$i]}
  if zfs list "$zvol@$snap" >/dev/null 2>&1; then
    $resume || die "$zvol@$snap already exists"
    echo "   $snap: already there, skipped"
    continue
  fi
  step "$snap ($name)"
  "$vmobj" restore --repo "$repo" --key-file "$key" --vm "$vm" --manifest "$name" --output "$dev" \
    --mode in-place --check --progress lines | sed 's/^/   /'
  [ "${PIPESTATUS[0]}" -eq 0 ] || die "the restore of $name failed; fix the cause, then run again with --resume"
  if [ ${#hashes[@]} -gt 0 ]; then
    got=$(sha256sum "$dev" | cut -d' ' -f1)
    [ "$got" = "${hashes[$i]}" ] || die "$zvol after $name has sha256 $got, but the snapshot $snap had ${hashes[$i]}. Nothing was snapshotted; the zvol is left as it is."
    echo "   sha256 matches the original @$snap"
  fi
  zfs snapshot "$zvol@$snap" || die "zfs snapshot $zvol@$snap failed"
  echo "   snapshot $zvol@$snap taken"
done

step "result"
zfs list -t all -o name,used,refer -s creation -r "$zvol" | sed 's/^/   /'
echo
echo "PASS: $zvol holds all ${#snaps[@]} snapshots. Point the VM's disk at $dev when you're ready."
