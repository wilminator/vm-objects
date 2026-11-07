#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# End-to-end check of backup-vm, vms and restore-vm with a stand-in virsh,
# so it runs anywhere (CI included) without libvirt. Needs qemu-img.
#
#   scripts/vm-e2e.sh [VMOBJ_BINARY]
set -euo pipefail
vmobj=$(realpath "${1:-vmobj}")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
uuid=aaaaaaaa-1111-2222-3333-444444444444

# A VM with a qcow2 disk on a raw backing file, a raw second disk, NVRAM
# and TPM state.
dd if=/dev/urandom of=base.raw bs=1M count=3 status=none
qemu-img create -q -f qcow2 -F raw -b base.raw vda.qcow2
qemu-io -c 'write -P 0x42 1M 256k' vda.qcow2 >/dev/null
truncate -s 2M vdb.img && dd if=/dev/urandom of=vdb.img bs=64k count=4 conv=notrunc status=none
head -c 131072 /dev/urandom > VARS.fd
mkdir -p "swtpm/$uuid/tpm2" && echo "tpm state" > "swtpm/$uuid/tpm2/tpm2-00.permall"
cat > virsh <<EOF
#!/bin/bash
case "\$1" in
  domstate) echo "shut off" ;;
  dumpxml) cat <<XML
<domain type='kvm'>
  <name>e2e</name>
  <uuid>$uuid</uuid>
  <os><type>hvm</type><nvram>$work/VARS.fd</nvram></os>
  <devices>
    <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='$work/vda.qcow2'/><target dev='vda'/></disk>
    <disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='$work/vdb.img'/><target dev='vdb'/></disk>
    <disk type='file' device='cdrom'><source file='/nonexistent.iso'/><target dev='hdc'/><readonly/></disk>
    <tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm>
  </devices>
</domain>
XML
  ;;
  *) echo "vm-e2e: vmobj must not run virsh \$*" >&2; exit 1 ;;
esac
EOF
chmod +x virsh

step() { printf '\n== %s\n' "$*"; }
step "full whole-VM backup"
"$vmobj" key init --key-file k
"$vmobj" init --repo repo --key-file k --block-size 1MiB
"$vmobj" init --repo copy --key-file k --block-size 1MiB
"$vmobj" backup-vm --repo repo --key-file k --domain e2e --type full --virsh ./virsh --swtpm-dir swtpm --state-dir st --copy-to copy
qemu-img convert -O raw vda.qcow2 want-vda-full.raw
cp vdb.img want-vdb-full.raw

step "incremental whole-VM backup"
qemu-io -c 'write -P 0x99 2M 64k' vda.qcow2 >/dev/null
qemu-img convert -O raw vda.qcow2 want-vda-inc.raw
"$vmobj" backup-vm --repo repo --key-file k --domain e2e --type incremental --virsh ./virsh --swtpm-dir swtpm --state-dir st
"$vmobj" vms --repo repo --key-file k

step "restore each record and compare"
mapfile -t recs < <("$vmobj" vms --repo repo --key-file k | sed -n 's/^  \([^ ]*\).*/\1/p')
[ "${#recs[@]}" -eq 2 ] || { echo "FAIL: expected 2 records"; exit 1; }
for rec in "${recs[@]}"; do
  case "$rec" in *-full) w=full ;; *-incremental) w=inc ;; esac
  "$vmobj" restore-vm --repo repo --key-file k --vm e2e --record "$rec" --to "out-$w"
  cmp "want-vda-$w.raw" "out-$w/e2e-vda.raw"
  cmp want-vdb-full.raw "out-$w/e2e-vdb.raw"
  cmp VARS.fd "out-$w/VARS.fd"
  cmp "swtpm/$uuid/tpm2/tpm2-00.permall" "out-$w/tpm/tpm2/tpm2-00.permall"
  grep -q "<source file='$work/out-$w/e2e-vda.raw'/>" "out-$w/e2e.xml"
  grep -q "<source file='/nonexistent.iso'/>" "out-$w/e2e.xml" # the CD-ROM is untouched
  echo "OK: $rec restores disks, NVRAM, TPM and definition"
done

step "restore the full from the --copy-to copy"
"$vmobj" restore-vm --repo copy --key-file k --vm e2e --record "${recs[0]}" --to out-from-copy
cmp want-vda-full.raw out-from-copy/e2e-vda.raw
cmp "swtpm/$uuid/tpm2/tpm2-00.permall" out-from-copy/tpm/tpm2/tpm2-00.permall
echo "OK: the copy restores the whole VM"

step "restore under a new name"
"$vmobj" restore-vm --repo repo --key-file k --vm e2e --record "${recs[0]}" --to out-copy --new-name e2e-copy
grep -q "<name>e2e-copy</name>" out-copy/e2e-copy.xml
! grep -q "<uuid>" out-copy/e2e-copy.xml
echo "OK: renamed, UUID dropped"

printf '\nPASS: whole-VM end-to-end checks\n'
