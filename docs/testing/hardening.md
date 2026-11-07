# Failure-safety testing

The milestone tests show that backups restore correctly when everything
goes right. These show what happens when something goes wrong: damaged
data, a run killed or stopped part-way, a full disk. In every case vmobj
should refuse clearly, keep earlier backups intact, and pick up again on
the next run.

## 1. The automated tests

`scripts/hardening.sh` runs eleven tests on throwaway local repositories,
each in its own temporary folder. It needs no S3 service, no VM and no
root (except the optional disk-full test), and takes a few minutes.

Run it on the NAS, where backups will really run. From a clone of `main`,
with the binary you'll actually use:

```bash
scripts/hardening.sh /mnt/fast/apps/vmobj/vmobj
```

Or with the one you just built:

```bash
scripts/hardening.sh bin/vmobj
```

It ends with a summary like `11 passed, 0 failed, 0 skipped`.

| Test | What it does | What should happen |
| --- | --- | --- |
| `corrupt-block` | Flips one byte in a stored block | `verify --deep` and `restore` refuse; nothing is written under the output name and no temporary file is left |
| `corrupt-pack` | Flips one byte in an incremental's pack | The incremental refuses to restore; the full, which doesn't use that pack, still restores |
| `corrupt-manifest` | Flips one byte in a manifest | That backup refuses to restore; the others still do |
| `missing-block` | Deletes a stored block | `verify` (without `--deep`) reports it |
| `kill-full` | `kill -9` during a full, then runs it again | No backup is recorded for the killed run; the rerun finishes and restores identical; `prune --apply` afterwards removes nothing the backup needs |
| `kill-incremental` | `kill -9` during an incremental, then two more | Both later incrementals restore identical, so the host's state file survived the kill |
| `stop-backup` | One SIGTERM during a full (vmobj treats it like Ctrl+C) | Exit code 130, both messages printed, no backup recorded, the rerun works |
| `stop-copy` | One SIGTERM during a copy, then copies again | Exit 130; the destination lists nothing until the second copy, which restores identical |
| `quit-copy` | Two SIGTERMs during a copy (quit at once) | Exit 143, or 130 if the clean stop won the race; the next copy completes |
| `long-chain` | A full and 20 incrementals | The oldest, a middle and the newest incremental and the full all restore identical |
| `install` | `vmobj install` twice, and with `--name` | Identical file, mode 755; the second install leaves it untouched |

The disk-full test mounts a 64 MiB `tmpfs` with `sudo`, so it only runs
when named:

```bash
scripts/hardening.sh bin/vmobj diskfull
```

It should fail the large backup with a "no space left on device" message
and leave the earlier backup verifying and restoring.

**SKIP** means a backup or copy finished before the script could stop it.
That happens on very fast storage. Rerun that test with a bigger disk:

```bash
HARDENING_DISK_MIB=2048 scripts/hardening.sh bin/vmobj kill-full stop-copy
```

**Leftovers of a killed run** stay in the repository until a prune at least
an hour later removes them: prune never touches objects younger than its
`--grace` period, which can't be set below an hour. That's deliberate: a
backup still running elsewhere must never lose its pieces.

## 2. Checks to do by hand

### A real VM: `scripts/validate-vm.sh`

This runs the checks from the milestone 4 to 6 pages against one of your
own VMs, in one go: a whole-VM full backup with `--copy-to`, an
incremental, `vmobj copy` (twice: the second must send nothing), and a
restore from each repository. Every disk is compared with the VM's own
disk, the NVRAM and TPM state byte for byte, and the rewritten definition
is checked. The VM is only read, and must be shut off for the whole run.

On TrueNAS, with WinGames shut down:

```bash
sudo scripts/validate-vm.sh --domain 1_WinGames --work /mnt/fast/validate-test --vmobj /mnt/fast/apps/vmobj/vmobj --connect 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock'
```

On the QNAP (Virtualization Station):

```bash
scripts/validate-vm.sh --domain DOMAIN_NAME_OR_UUID --work /share/VMs/validate-test --vmobj /share/VMs/vmobj --virsh /QVS/usr/bin/virsh --swtpm-dir /QVS/var/lib/libvirt/swtpm
```

It ends with `PASS: vmobj backs up and restores … exactly` and deletes its
test folder, or lists each FAIL and keeps the folder. `--help` lists the
options. CI runs the same script against a made-up VM
(`scripts/validate-vm-selftest.sh`).

### Restore on a different host

A backup made on one host must restore on another. Back up a shut-down VM
on TrueNAS into a repository the QNAP can reach (a share, or S3):

```bash
sudo vmobj backup-vm --repo /mnt/fast/vmobj-xhost --key-file /root/vmobj.key --domain 1_WinGames --type full --connect 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock'
```

On the QNAP, restore it under a new name, so it can never be mistaken for
the original:

```bash
/share/VMs/vmobj restore-vm --repo /PATH/TO/vmobj-xhost --key-file /share/VMs/vmobj-test.key --vm 1_WinGames --to /share/VMs/xhost-restore --new-name WinGames-xhost
```

Compare each restored disk's `sha256sum` with the zvol's on TrueNAS
(`sudo sha256sum /dev/zvol/...`), and read `RESTORE.txt`. If you boot the
copy, keep its network disconnected: two copies of an activated Windows
online at once can get its activation flagged.

Then restore one disk **onto a new zvol** on TrueNAS, which tests writing to
a block device. Make the zvol the same size as the original (`zfs get -H -o
value volsize`); `vmobj list` shows the disk series (such as
`1_WinGames.vda`) and the manifest name:

```bash
sudo zfs create -s -V SIZE fast/restore-test-disk
```

```bash
sudo vmobj restore --repo /mnt/fast/vmobj-xhost --key-file /root/vmobj.key --vm VM_DISK_SERIES --manifest NAME --output /dev/zvol/fast/restore-test-disk
```

Compare it with `cmp`, then remove it with `zfs destroy fast/restore-test-disk`.

### Running from the Docker image

The image runs as an unprivileged user (uid 65532). Check that it can make
and restore a backup of a plain file, with the folder mounted:

```bash
mkdir -p ~/img-test && head -c 64M /dev/urandom > ~/img-test/disk.raw && chmod -R a+rwX ~/img-test
```

```bash
docker run --rm -v ~/img-test:/work ghcr.io/wilminator/vmobj:edge key init --key-file /work/k
```

```bash
docker run --rm -v ~/img-test:/work ghcr.io/wilminator/vmobj:edge init --repo /work/repo --key-file /work/k
```

```bash
docker run --rm -v ~/img-test:/work ghcr.io/wilminator/vmobj:edge backup --repo /work/repo --key-file /work/k --source /work/disk.raw --vm img --type full --state-dir /work/state
```

Restore the backup the same way (`list`, then `restore ... --output
/work/out.raw`) and compare it with `cmp ~/img-test/disk.raw
~/img-test/out.raw`. Backups of zvols and VMs need the host's `zfs` and
`virsh`, so they run with the binary on the host, not in the image.

### Timing baselines, before M7

Save these outputs. Milestone 7 (concurrent I/O) will be measured against
them. Use a real VM disk on TrueNAS:

```bash
sudo vmobj backup --repo /mnt/fast/vmobj-test --key-file /root/vmobj.key --source zfs:fast/vms/wingames --vm baseline --type full --metrics
```

```bash
sudo vmobj copy --from /mnt/fast/vmobj-test --to /mnt/staging/vmobj-copy-test --key-file /root/vmobj.key --profile standard --metrics
```

## What to report back

- The summary lines from `hardening.sh`, and the full output of any test
  that FAILs.
- `validate-vm.sh`: the summary, and every FAIL line.
- For the manual checks: whether each restore matched, and the two
  `--metrics` tables.
