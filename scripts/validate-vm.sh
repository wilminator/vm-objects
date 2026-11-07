#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Validates vmobj against one of your own VMs, end to end. Run with --help.
set -uo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/validate-vm.sh --domain NAME --work DIR [options]

Checks that vmobj backs up and restores a real libvirt VM exactly, using
throwaway repositories in DIR. Nothing about the VM is changed: it is only
read, and must be shut off for the whole run so its disks hold still.

  1. a whole-VM full backup, with --copy-to a second repository
  2. an incremental backup (the VM hasn't changed, so it stores almost
     nothing), copied with "vmobj copy"; a second copy must send nothing
  3. the full restored from the first repository: every disk compared with
     the VM's own disk, NVRAM and TPM state compared byte for byte, and the
     rewritten definition checked
  4. the incremental restored from the copy, compared the same way
  5. a different key refused

These are the checks behind the milestone 4 to 6 test pages, run in one go.
Restores are compared and deleted one at a time, so DIR needs room for the
repositories (the VM's used space, compressed, twice) plus the largest disk.

Options:
  --domain NAME     libvirt domain name or UUID (required)
  --work DIR        empty or new folder for the test (required)
  --vmobj PATH      vmobj binary (default: vmobj on PATH)
  --virsh PATH      virsh binary (QNAP: /QVS/usr/bin/virsh)
  --connect URI     libvirt URI, e.g. qemu:///system, or on TrueNAS
                    'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock'
  --swtpm-dir DIR   where libvirt keeps TPM state (default
                    /var/lib/libvirt/swtpm; QNAP /QVS/var/lib/libvirt/swtpm)
  --keep            keep DIR afterwards (default: delete it when all pass)
  -h, --help        this text

Needs qemu-img for qcow2 disks. Usually run as root, to read the disks and
the TPM state.
EOF
}

domain="" work="" vmobj=vmobj virsh=virsh connect="" swtpm=/var/lib/libvirt/swtpm keep=false
while [ $# -gt 0 ]; do
  case $1 in
    --domain) domain=$2; shift 2 ;;
    --work) work=$2; shift 2 ;;
    --vmobj) vmobj=$2; shift 2 ;;
    --virsh) virsh=$2; shift 2 ;;
    --connect) connect=$2; shift 2 ;;
    --swtpm-dir) swtpm=$2; shift 2 ;;
    --keep) keep=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

die() { echo "validate-vm: $*" >&2; exit 2; }
if [ -z "$domain" ] || [ -z "$work" ]; then die "--domain and --work are required (see --help)"; fi
command -v "$vmobj" >/dev/null || die "vmobj not found: $vmobj"
vmobj=$(command -v "$vmobj")
command -v "$virsh" >/dev/null || die "virsh not found: $virsh"
if [ -e "$work" ] && [ -n "$(ls -A "$work" 2>/dev/null)" ]; then die "$work is not empty"; fi
mkdir -p "$work" && work=$(cd "$work" && pwd)

vsh=("$virsh") vmo_virsh=(--virsh "$virsh" --swtpm-dir "$swtpm")
if [ -n "$connect" ]; then vsh+=(-c "$connect"); vmo_virsh+=(--connect "$connect"); fi

pass=0 fail=0
step() { printf '\n== %s\n' "$*"; }
ok() { printf '   ok: %s\n' "$*"; pass=$((pass + 1)); }
bad() { printf '   FAIL: %s\n' "$*"; fail=$((fail + 1)); }

# ---- what the VM looks like -----------------------------------------------------

step "the VM"
state=$("${vsh[@]}" domstate "$domain" 2>&1) || die "virsh can't see $domain: $state"
[ "$state" = "shut off" ] || die "$domain is '$state'. Shut it down first: its disks must not change during the test."
uuid=$("${vsh[@]}" domuuid "$domain" | tr -d '[:space:]')
xml=$("${vsh[@]}" dumpxml --inactive "$domain")
# The name from the definition: "virsh domname" takes only an ID or UUID.
name=$(sed -n 's:.*<name>\(.*\)</name>.*:\1:p' <<<"$xml" | head -n1)
[ -n "$name" ] || die "couldn't read the name of $domain from its definition"
echo "   $name ($uuid), shut off"

# Disks: "file disk vda /path" or "block disk vdb /dev/...". CD-ROMs are skipped.
mapfile -t disks < <("${vsh[@]}" domblklist --details "$domain" | awk '$2 == "disk" && $4 != "" && $4 != "-" {print $3 " " $4}')
[ ${#disks[@]} -gt 0 ] || die "$domain has no disks"
for d in "${disks[@]}"; do echo "   disk ${d%% *}: ${d#* }"; done
nvram=$(sed -n 's:.*<nvram[^>]*>\(.*\)</nvram>.*:\1:p' <<<"$xml" | head -n1)
[ -n "$nvram" ] && echo "   NVRAM: $nvram"
tpmdir=""
if grep -q "<backend type='emulator'" <<<"$xml"; then
  # A definition can name its TPM folder (TrueNAS's do); otherwise it is
  # libvirt's swtpm folder, under the UUID.
  tpmdir=$(sed -n "/<tpm/,/<\/tpm>/s:.*<source type=.dir. path=.\([^'\"]*\).*:\1:p" <<<"$xml" | head -n1)
  [ -n "$tpmdir" ] || tpmdir="$swtpm/$uuid"
  echo "   TPM state: $tpmdir"
fi

# ---- backups -------------------------------------------------------------------

key="$work/test.key" repo="$work/repo" copy="$work/copy" state="$work/state"
step "repositories"
if ! { "$vmobj" key init --key-file "$key" && "$vmobj" init --repo "$repo" --key-file "$key" \
        && "$vmobj" init --repo "$copy" --key-file "$key"; } >/dev/null; then
  die "couldn't create the test repositories"
fi
ok "a key and two repositories in $work"

step "full backup, with --copy-to"
if "$vmobj" backup-vm --repo "$repo" --key-file "$key" --domain "$domain" --vm "$name" --type full \
    "${vmo_virsh[@]}" --state-dir "$state" --copy-to "$copy" --progress lines 2>&1 | tee "$work/full.log"; then
  ok "full backup and copy"
else
  bad "the full backup failed (see above)"; echo; echo "validate-vm: can't go on without a backup"; exit 1
fi

step "incremental backup, then vmobj copy"
if "$vmobj" backup-vm --repo "$repo" --key-file "$key" --domain "$domain" --vm "$name" --type incremental \
    "${vmo_virsh[@]}" --state-dir "$state" --progress none 2>&1 | tee "$work/inc.log"; then
  ok "incremental backup"
else
  bad "the incremental backup failed"
fi
if "$vmobj" copy --from "$repo" --to "$copy" --key-file "$key" --profile standard --progress none | tee "$work/copy1.log"; then
  ok "copy"
else
  bad "the copy failed"
fi
"$vmobj" copy --from "$repo" --to "$copy" --key-file "$key" --profile standard --progress none > "$work/copy2.log" 2>&1
if grep -q "Copied:    0 backups" "$work/copy2.log"; then ok "a second copy sends nothing"; else bad "a second copy copied again: $(head -n3 "$work/copy2.log" | paste -sd ' ')"; fi

mapfile -t recs < <("$vmobj" vms --repo "$repo" --key-file "$key" --vm "$name" | sed -n 's/^  \([^ ]*\).*/\1/p')
[ ${#recs[@]} -eq 2 ] || { bad "expected 2 VM records, found ${#recs[@]}"; exit 1; }

# ---- restores ------------------------------------------------------------------

# Restore record $2 from repository $1 into $work/restore and compare everything.
check_restore() {
  local from=$1 rec=$2 out="$work/restore"
  step "restore $rec from ${from#"$work"/}"
  if ! "$vmobj" restore-vm --repo "$from" --key-file "$key" --vm "$name" --record "$rec" --to "$out" --progress lines; then
    bad "restore-vm failed"; return
  fi
  local d target src got
  for d in "${disks[@]}"; do
    # Restored disks are named <VM>-<target>.raw (.img for QNAP); the VM's name there is
    # the platform's (TrueNAS's WinGames, not libvirt's 1_WinGames).
    target=${d%% *} src=${d#* }
    got=$(find "$out" -maxdepth 1 \( -name "*-$target.raw" -o -name "*-$target.img" \) | head -n1)
    if [ -z "$got" ] || [ ! -f "$got" ]; then bad "disk $target wasn't restored"; continue; fi
    case $src in
      *.qcow2|*.qcow)
        if qemu-img compare -q -f qcow2 -F raw "$src" "$got"; then ok "disk $target matches $src"; else bad "disk $target differs from $src"; fi ;;
      *)
        if cmp -s "$src" "$got"; then ok "disk $target matches $src"; else bad "disk $target differs from $src"; fi ;;
    esac
    rm -f "$got"
  done
  if [ -n "$nvram" ]; then
    if cmp -s "$nvram" "$out/$(basename "$nvram")"; then ok "NVRAM identical"; else bad "NVRAM differs or is missing"; fi
  fi
  if [ -n "$tpmdir" ]; then
    if diff -r "$tpmdir" "$out/tpm" >/dev/null; then ok "TPM state identical ($(find "$tpmdir" -type f | wc -l) files)"; else bad "TPM state differs or is missing"; fi
  fi
  local def
  def=$(find "$out" -maxdepth 1 -name '*.xml' | head -n1)
  if [ -n "$def" ]; then
    local bad_def=0
    for d in "${disks[@]}"; do
      grep -Eq "$out/[^/'\"]*-${d%% *}\.(raw|img)" "$def" || { bad "the definition doesn't point disk ${d%% *} at its restored file"; bad_def=1; }
    done
    if grep -q "<backingStore" "$def"; then bad "the definition still has a <backingStore> chain"; bad_def=1; fi
    [ $bad_def -eq 0 ] && ok "definition points every disk at its restored file, with no backing chain"
  else
    bad "no definition was written"
  fi
  if [ -s "$out/RESTORE.txt" ]; then ok "RESTORE.txt written"; else bad "RESTORE.txt missing"; fi
  rm -rf "$out"
}

check_restore "$repo" "${recs[0]}"
check_restore "$copy" "${recs[1]}"

step "a different key is refused"
"$vmobj" key init --key-file "$work/other.key" >/dev/null
if "$vmobj" vms --repo "$repo" --key-file "$work/other.key" >/dev/null 2>&1; then bad "another key opened the repository"; else ok "refused"; fi

# ---- summary -------------------------------------------------------------------

step "summary"
printf '   %d checks passed, %d failed\n' "$pass" "$fail"
if [ "$fail" -eq 0 ]; then
  printf '\nPASS: vmobj backs up and restores %s exactly\n' "$name"
  $keep || rm -rf "$work"
  exit 0
fi
printf '\nFAIL: see the FAIL lines above; the test folder %s is kept\n' "$work"
exit 1
