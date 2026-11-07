# Milestone 4 testing: qcow2 and ZFS sources

What this milestone adds:

- **qcow2 disks are read directly.** vmobj reads qcow2 images (QNAP
  Virtualization Station, libvirt and Proxmox all use them) with its own
  reader, following the backing chain, with no QEMU tools, kernel modules
  or extra disk space. It handles compressed clusters (deflate and zstd),
  zero clusters, any cluster size, and qcow2 versions 2 and 3.
- **Unsafe images are refused.** Images marked dirty (open in a running VM)
  or corrupt, encrypted images, and features the reader doesn't know are
  refused with a clear message rather than read wrongly.
- **The format never comes from a disk's contents.** Files named `*.qcow2`
  are qcow2, and everything else is raw. A raw-named file containing a qcow2
  header is refused until you pass `--format`. This closes the classic hole
  where a guest writes a fake qcow2 header into its own disk to make the
  host read a file of the guest's choosing.
- **`--source zfs:pool/vms/win11`** snapshots a zvol, reads the snapshot
  through a temporary read-only clone, then removes both. Use
  `--keep-snapshot` to keep the snapshot.

CI checks the qcow2 reader against `qemu-img`'s own output for every
variant listed above, including backing chains. A ZFS job runs the real
`zfs:` flow when the runner's kernel has ZFS. The unit tests cover the
command sequence, and the undo after a failure, with a simulated `zfs`.

## A. Your real VM disks on the QNAP

This is the test that matters most: your actual WinGames images.

**The VM must be shut down.** A running VM's qcow2 is marked dirty, and
vmobj will refuse it. That refusal is the right behaviour, so trying it once
while the VM runs is a useful check too.

### 1. Build the binary in WSL and copy it to the QNAP

```bash
cd ~/vm-objects && git pull && make build-all
```

```bash
scp bin/vmobj-linux-amd64 admin@QNAP_ADDRESS:/share/VMs/vmobj
```

Everything below runs on the QNAP over SSH.

### 2. Measure the flattened image (read-only, no repository)

```bash
/share/VMs/vmobj measure --source /share/VMs/WinGames/WinGames/flattened.qcow2 --name wingames --state-dir /share/VMs/vmobj-state
```

It reads the whole 112 GiB disk and reports how much is non-zero. Note how
long it took; that's the full-backup read speed on the QNAP.

### 3. Back up and restore the flattened image

Use a local repository on a share with enough room: allow about the
"Data (non-zero)" figure from step 2, plus as much again for the restore.

```bash
/share/VMs/vmobj key init --key-file /share/VMs/vmobj-test.key
```

```bash
/share/VMs/vmobj init --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key
```

```bash
/share/VMs/vmobj backup --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key --source /share/VMs/WinGames/WinGames/flattened.qcow2 --vm wingames --type full --state-dir /share/VMs/vmobj-state
```

```bash
/share/VMs/vmobj list --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key
```

```bash
/share/VMs/vmobj restore --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key --vm wingames --manifest NAME_FROM_LIST --output /share/VMs/wingames-restored.raw
```

Then let QEMU itself compare the original with the restore (the same tool
you used for the flatten):

```bash
qemu-img compare /share/VMs/WinGames/WinGames/flattened.qcow2 /share/VMs/wingames-restored.raw
```

Expect "Images are identical." The restored raw file is sparse, so it uses
much less space than 112 GiB.

### 4. The live chain (base + 3 layers)

Point `measure` at the **top** layer of the chain the VM actually uses (the
file named in the VM's disk settings):

```bash
/share/VMs/vmobj measure --source PATH_TO_TOP_LAYER.qcow2 --name wingames-chain --state-dir /share/VMs/vmobj-state
```

- If it works, the non-zero figure should be close to step 2's (same disk).
- If it says a backing file "has no recorded format", QNAP made the chain
  without recording the backing format. **Don't change anything yet.**
  Send me the message: the fix it prints (`qemu-img rebase -u -F ...`) only
  rewrites header metadata, but it's your VM, so we'll decide together.

### 5. Clean up

```bash
rm -rf /share/VMs/vmobj-repo /share/VMs/wingames-restored.raw /share/VMs/vmobj-state
```

Keep `vmobj-test.key` if you want to reuse the repository; otherwise
delete it too.

## B. ZFS (later, on the new NAS)

The QNAP's QTS doesn't use ZFS, so this waits for TrueNAS. As root, on any
pool with 64 MiB free:

```bash
scripts/zfs-e2e.sh POOL_OR_PARENT_DATASET bin/vmobj
```

It creates a test zvol, backs it up from snapshots (full, then incremental),
restores both byte-identical, checks vmobj left no snapshots or clones
behind, and deletes the test zvol.

## What to report back

- Step 2: the size, the non-zero data, and how long it took.
- Step 3: whether `qemu-img compare` said "Images are identical", and how
  long the backup and restore took.
- Step 4: worked or not, and the exact message if not.
