#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Runs scripts/validate-vm.sh against a made-up VM with a stand-in virsh, so
# CI checks the validation script itself without libvirt. Needs qemu-img.
#
#   scripts/validate-vm-selftest.sh [VMOBJ_BINARY]
set -euo pipefail
vmobj=$(realpath "${1:-$(command -v vmobj)}")
here=$(cd "$(dirname "$0")" && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
cd "$t"
uuid=bbbbbbbb-1111-2222-3333-444444444444

# A qcow2 disk on a raw backing file, a raw second disk, NVRAM and TPM state.
dd if=/dev/urandom of=base.raw bs=1M count=3 status=none
qemu-img create -q -f qcow2 -F raw -b base.raw vda.qcow2
qemu-io -c 'write -P 0x42 1M 256k' vda.qcow2 >/dev/null
truncate -s 2M vdb.img && dd if=/dev/urandom of=vdb.img bs=64k count=4 conv=notrunc status=none
head -c 131072 /dev/urandom > VARS.fd
mkdir -p "swtpm/$uuid/tpm2" && echo "tpm state" > "swtpm/$uuid/tpm2/tpm2-00.permall"

cat > virsh <<EOF
#!/bin/bash
[ "\$1" = -c ] && shift 2
case "\$1" in
  domstate) echo "shut off" ;;
  domuuid) echo $uuid ;;
  domname) echo selftest ;;
  domblklist) printf ' Type   Device   Target   Source\n------------------------------\n file   disk     vda      $t/vda.qcow2\n file   disk     vdb      $t/vdb.img\n file   cdrom    hdc      -\n\n' ;;
  dumpxml) cat <<XML
<domain type='kvm'>
  <name>selftest</name>
  <uuid>$uuid</uuid>
  <os><type>hvm</type><nvram>$t/VARS.fd</nvram></os>
  <devices>
    <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='$t/vda.qcow2'/><backingStore type='file'><format type='raw'/><source file='$t/base.raw'/></backingStore><target dev='vda'/></disk>
    <disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='$t/vdb.img'/><target dev='vdb'/></disk>
    <disk type='file' device='cdrom'><target dev='hdc'/><readonly/></disk>
    <tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm>
  </devices>
</domain>
XML
  ;;
  *) echo "selftest: unexpected virsh \$*" >&2; exit 1 ;;
esac
EOF
chmod +x virsh

"$here/validate-vm.sh" --domain selftest --work "$t/work" --vmobj "$vmobj" --virsh "$t/virsh" --swtpm-dir "$t/swtpm"
