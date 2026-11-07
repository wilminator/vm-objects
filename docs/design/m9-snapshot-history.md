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

## Fast replay (built first)

Replaying a history used to read the whole disk three times per step
(compare, `--check`, `sha256sum`). Two changes, in `vmobj restore`:

- `--base NAME` (in-place): the target already holds backup NAME (the
  previous step, just checked), so blocks the same in NAME and the backup
  being restored are skipped without reading the target. Only the blocks
  that differ are read, compared and written.
- `--check` computes the whole disk's SHA-256 while it reads back, and
  `--expect-sha256 HEX` fails the restore when it differs; `--check-written`
  reads back only the blocks written.

`zfs-replay.sh` uses them: `--verify every` (default) is one full read per
step; `--verify end` reads back only what each step wrote and checks the
whole disk once, after the last.

A plain restore also became one pass: each block is written once, with its
newer extents merged in memory, and a full block that newer extents cover
entirely isn't fetched.

## Staged restores (built second)

`restore --mode staged` updates an existing file or device without
touching it until the result is proven:

1. The target is opened read-only. The blocks that differ (all of them, or
   with `--base` only those that differ between the two backups) are
   compared, and the 4 KiB pieces to change go to a journal, each record
   sealed with the repository's key (compressed, encrypted, and bound to
   its place in the journal).
2. Every block of target-plus-journal is checked against the backup, and
   the whole disk's SHA-256 computed (`--expect-sha256` compares it).
   Blocks that are wrong, for example because the target wasn't what
   `--base` said, are fetched whole into the journal and checked again.
3. Only then is the journal committed (synced, with a commit record),
   applied, read back and removed.

Interrupted while applying, the same command finishes from the committed
journal. An uncommitted journal is discarded: the target was never
touched. A journal for a different restore is refused, and a damaged
journal fails its seal. With `--base`, the room the journal may need is
checked before anything is fetched; the journal's filesystem keeps 256 MiB
free and a restore stops cleanly, target untouched, if it runs low.

Changes are written twice (journal, then target); the target is read once
to compare (only the differing blocks with `--base`) and once to check.
`zfs-replay.sh --staged DIR` uses it.

Later: reuse an uncommitted journal's fetched pieces on a rerun (they are
discarded today), and keep pieces as fetched (still compressed) rather
than re-compressing.

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
