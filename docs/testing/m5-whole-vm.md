# Milestone 5 testing: whole-VM backups

What this milestone adds:

- **`vmobj backup-vm --domain NAME`** backs up a whole libvirt VM:
  - **Every disk**, each as its own backup series (`<vm>.<target>`, e.g.
    `WinGames.hda`).
  - **The VM's definition** (the libvirt XML).
  - **The UEFI NVRAM store**, when the VM has one.
  - **The emulated TPM state.** This is what keeps BitLocker and Windows
    Hello PINs working after a restore.

  A sealed **VM record**, written last, ties these together.
- **How it captures the disks:**
  - **Shut-off VMs** are read directly.
  - **Running VMs on zvols** are snapshotted together in one atomic
    `zfs snapshot`. `--quiesce` first freezes the guest's filesystems through
    the QEMU guest agent, and the thaw always runs, even if the snapshot
    fails.
  - **Running VMs with disk image files are refused.** Snapshotting them
    live means changing the VM's disk chain, which vmobj won't do to a
    running VM.
- **`vmobj vms`** lists whole-VM backups.
- **`vmobj restore-vm --to DIR`** writes the disks (as raw images), an
  adjusted definition pointing at them, the NVRAM and the TPM state into a
  folder, plus a `RESTORE.txt` with the next steps. **It never changes
  libvirt.** `--new-name` renames the VM and drops its UUID, so a copy can
  be defined next to the original.
- **Definition rewrites are surgical:** only the changed attribute values
  are edited, so comments and QNAP-specific settings survive exactly.
- **`prune`** handles VM records. A kept record keeps its disks' backups.

CI runs `scripts/vm-e2e.sh`, which drives the real binary with a stand-in
`virsh`: a qcow2 disk on a raw backing file plus a raw disk, NVRAM, TPM
state and a CD-ROM (which must be left alone). It runs a full and an
incremental, then restores both byte-identical and checks the rename. Unit
tests cover the freeze → snapshot → thaw order, the thaw after a failed
snapshot, the refusal of live image files, and prune.

## On the QNAP, with WinGames shut down

From earlier: WinGames runs on machine type `pc-i440fx-4.1` with an OVMF ROM
and **no NVRAM file**, and its TPM state is at
`/QVS/var/lib/libvirt/swtpm/7240004c-9f00-4aca-8577-8987c8073490/tpm2/`.
QNAP keeps `virsh` and the TPM folder under `/QVS`, so the commands say so.

### 1. Copy the new binary over (from WSL)

```bash
cd ~/vm-objects && git pull && make build-all
```

```bash
scp bin/vmobj-linux-amd64 admin@QNAP_ADDRESS:/share/VMs/vmobj
```

### 2. Check that vmobj can see the VM

Find the VM's libvirt name (QVS may use the UUID as the name):

```bash
/QVS/usr/bin/virsh list --all
```

If that fails with a connection error, add `--connect qemu:///system` to it
and to the commands below.

### 3. Back up the whole VM

Use a fresh local repository (`init` as in milestone 4, if you deleted it):

```bash
/share/VMs/vmobj backup-vm --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key --domain DOMAIN_NAME_OR_UUID --vm WinGames --type full --virsh /QVS/usr/bin/virsh --swtpm-dir /QVS/var/lib/libvirt/swtpm --state-dir /share/VMs/vmobj-state
```

Expect "VM record: … (shut off)", one line per disk, and `Files: 1` (the
TPM state; there's no NVRAM file).

### 4. Restore it into a folder

```bash
/share/VMs/vmobj vms --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key
```

```bash
/share/VMs/vmobj restore-vm --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key --vm WinGames --record RECORD_FROM_VMS --to /share/VMs/wingames-restore
```

### 5. Compare

The TPM state must be byte-identical. This is the file whose loss cost you
the PIN:

```bash
cmp /QVS/var/lib/libvirt/swtpm/7240004c-9f00-4aca-8577-8987c8073490/tpm2/tpm2-00.permall /share/VMs/wingames-restore/tpm/tpm2/tpm2-00.permall && echo TPM IDENTICAL
```

Each disk against the image the VM uses (the paths are in the VM's
definition, and `RESTORE.txt` lists the restored ones):

```bash
qemu-img compare PATH_TO_THE_VMS_DISK.qcow2 /share/VMs/wingames-restore/WinGames-hda.raw
```

And look over the adjusted definition and the instructions:

```bash
/QVS/usr/bin/virsh dumpxml --inactive DOMAIN_NAME_OR_UUID > /tmp/vm-now.xml && diff /tmp/vm-now.xml /share/VMs/wingames-restore/WinGames.xml; rm -f /tmp/vm-now.xml
```

```bash
cat /share/VMs/wingames-restore/RESTORE.txt
```

The `diff` should show only the disk source paths, the driver type
changing to `raw`, and the `<backingStore>` chain removed. (The QNAP's
shell is BusyBox `sh`, so the current definition goes to a file first:
`<(...)` only works in bash.) Expect `RESTORE.txt` to warn that defining this XML
would replace the original VM.

**Don't `virsh define` anything on the QNAP.** QVS manages its VMs itself,
and a definition from outside could confuse it. Actually booting a restored
VM is a test for the TrueNAS box.

### 6. A live VM is refused, and the VM is untouched

Start WinGames, then run the step 3 backup again. It should stop at once
with "the VM is running and has disk image files" and write nothing. The VM
keeps running normally.

### 7. Clean up

```bash
rm -rf /share/VMs/wingames-restore
```

Keep the repository and key for milestone 6, or delete them.

## What to report back

- Step 3: the output, especially the disk lines and `Files:`.
- Step 5: `TPM IDENTICAL`, the `qemu-img compare` results, and anything
  surprising in the `diff`.
- Step 6: the exact refusal message.
