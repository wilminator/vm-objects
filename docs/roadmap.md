# Roadmap

Milestones 1 to 6 are on `main`: the S3 backend, storage classes and
profiles, Object Lock and prune, qcow2 and ZFS sources, whole-VM backups,
and copying between repositories. [docs/testing](testing/README.md) has
what has been tested and how.

## Next

| Milestone | What | Done when |
| --- | --- | --- |
| **M9: snapshot history** (in progress) | Back up a disk's named snapshots as a chain, with each snapshot's name and original date in the backup, and rebuild them as real snapshots on the restoring platform, another one included. Covers backing up an existing snapshot (`zfs:pool/zvol@snap`). [Design](design/m9-snapshot-history.md). | A TrueNAS VM's history restores on the QNAP as Virtualization Station snapshots with their original dates. |
| M10: concurrent I/O | Reads and writes overlap, with `--io-workers`, so storage latency stops setting the pace. | `--metrics` shows backups and copies faster than the M6 baselines. |
| M11: LVM and Btrfs sources | Back up LVM logical volumes, through a thin snapshot so the volume holds still (`lvm:vg/lv`, `lvm:vg/lv@snap`), and Btrfs subvolumes. QNAP storage pools are LVM thin pools, and Proxmox and many plain KVM hosts keep VM disks on LVM-thin. | An LVM-thin volume backs up from a snapshot and restores in place. |

## Done

| Milestone | What |
| --- | --- |
| M7: block-level restore | `restore --mode copy` and `in-place`: update an existing file or device, writing only what differs, with `--check`. `scripts/zfs-replay.sh` rebuilds a zvol and its named snapshots from a chain of backups. |
| M8: platform modules | Everything specific to one hypervisor moves behind one interface: list VMs, describe one (disks, firmware files, definition), snapshot it, restore it. Modules: **TrueNAS** (its API, zvols, and TPM state kept under the VM's name), **QNAP** (Virtualization Station's paths), and **generic libvirt**. |

## Later

- **qcow2 output for restores**, so a restore for QNAP Virtualization
  Station (whose disk picker lists only .qcow2) needs no conversion step.
  Being built now, as step 2 of M9, because recreating QNAP snapshots
  needs it.

- **VMware and Nutanix modules**, on the M8 interface. VMware's changed
  block tracking would plug in as a faster way to find what changed; the
  repository format stays the same.
- **A faster in-place restore from S3:** per-extent hashes for full backups,
  so blocks with newer extents over them don't have to be downloaded just
  to compare.

## Phase 3: orchestration

An orchestrator (schedules, retention, many hosts) and a management portal,
likely as their own project, using `vmobj` as the engine.

## Design rules the modules keep

- **One libvirt module, many appliances.** The generic libvirt module
  takes its settings as given: the virsh program, libvirt's socket or URI,
  where TPM state and NVRAM live, and how TPM state is laid out. Appliance
  modules (QNAP, TrueNAS) are those settings plus what the appliance adds
  (TrueNAS's middleware). `vmobj platforms` lists them; `--virsh`,
  `--connect`, `--libvirt-socket` and `--swtpm-dir` override them, so an
  appliance vmobj doesn't know yet can be used through `--platform libvirt`.

- **A module is a set of capabilities.** It says what it can do (quiesce
  a guest, track changed blocks, register a restored VM) and the engine
  uses what is there.
- **The repository stays platform-neutral.** Disks are stored as plain
  block data and a VM's definition as an opaque blob, so a backup from one
  platform can restore on another; the restoring module translates what it
  can and explains the rest in `RESTORE.txt`.
