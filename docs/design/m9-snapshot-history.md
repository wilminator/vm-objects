# M9: snapshot history

Status: design, not built. Builds on M8 (`internal/platform`).

## What it does

Backs up a VM disk's named snapshots, oldest first, as one chain of
backups, and rebuilds them as real snapshots on the restoring platform,
which can be a different one. Today `scripts/zfs-waypoints.sh` and
`scripts/zfs-replay.sh` do this for ZFS alone, with the snapshot names and
hashes kept beside the repository in `hashes.txt`. M9 moves it into
`vmobj` and the platform modules, and keeps everything in the repository.

## Pieces

### 1. Backing up an existing snapshot

`--source zfs:pool/zvol@snap` (and, through the modules, a VM snapshot by
name) reads the snapshot directly: a ZFS snapshot is read through a
temporary read-only clone, as `zfs-waypoints.sh` does now. This replaces
the old "M10: existing snapshots" item.

### 2. Snapshot details in the backup

Each backup made from a snapshot records, inside its encrypted manifest:

| Field | Example |
| --- | --- |
| `snapshot.name` | `before-move` |
| `snapshot.created` | `2026-09-14T03:12:44Z` (the original creation time) |
| `snapshot.description` | free text, when the platform has one |
| `snapshot.platform` | `truenas`, `qnap`, `libvirt` |
| `snapshot.sha256` | of the whole disk at that snapshot (optional, `--hash`) |

So the history travels with the backups and needs no side file.

### 3. The module interface

An optional capability, like `Quiescer`:

```go
type Snapshotter interface {
    // Snapshots lists a disk's snapshots, oldest first.
    Snapshots(vm *VM, disk string) ([]Snapshot, error)
    // Open returns a reader for one snapshot's contents.
    Open(vm *VM, disk string, snap Snapshot) (io.ReadCloser, int64, error)
    // Recreate turns the current state of a restored disk into a
    // snapshot named and dated as snap, as closely as the platform allows.
    Recreate(target string, snap Snapshot) error
}
```

`vmobj backup-vm --history` backs up every snapshot not yet in the
repository, then the current state. `vmobj restore-vm --history` restores
the oldest in full and each later one with `--mode in-place`, calling
`Recreate` after each.

### 4. Per platform

| Platform | Snapshots are | Original date kept as |
| --- | --- | --- |
| TrueNAS / ZFS | zvol snapshots | a `vmobj:created` user property (ZFS sets `creation` itself) |
| QNAP (Virtualization Station) | **to be found out** (see below) | libvirt snapshot metadata: `<creationTime>` and `<description>`, loaded with `virsh snapshot-create --redefine`, which keeps them as given |
| generic libvirt | qcow2 snapshots | as QNAP |
| VMware, Nutanix (later) | their snapshot APIs | their own fields where they exist |

When a platform can't recreate a snapshot (a plain raw file, say), the
restore writes the newest state and lists the history in `RESTORE.txt`.

## First step: how Virtualization Station stores snapshots

QNAP stores VM snapshots as deltas. Which kind decides how `Recreate`
works on the QNAP:

- **external overlays** (a qcow2 file per snapshot, each on top of the
  last): rebuild by creating each overlay and writing that step's changes
  into it;
- **internal qcow2 snapshots** (all in one file): `qemu-img snapshot -c`
  after each step;
- **QTS volume snapshots** (below the VM): out of reach from libvirt; the
  history would be listed but not recreated.

To find out: take one snapshot of a test VM in Virtualization Station,
then read, without changing anything, `virsh snapshot-list --tree`,
`virsh snapshot-dumpxml`, `qemu-img info --backing-chain` on the disk, and
the files next to it.

## Done when

- A ZFS history backs up with `backup-vm --history` and rebuilds with
  `restore-vm --history`, matching what `zfs-replay.sh` gives today, with
  `vmobj:created` set.
- A TrueNAS history restores on the QNAP as Virtualization Station
  snapshots carrying their original dates, and the VM boots from each.
- `zfs-waypoints.sh` and `zfs-replay.sh` stay, as thin wrappers or moved to
  `scripts/legacy/` once `--history` covers them.
