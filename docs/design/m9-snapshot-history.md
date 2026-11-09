# M9: snapshot history

Status: parts A and B built (PR #35); part C (QNAP) built, waiting on the test on real hardware. Builds on M8 (`internal/platform`).

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
| QNAP (Virtualization Station) | external qcow2 overlays, one file per snapshot (see below) | its record file `.<uuid>.meta/snapshot/<epoch>.xml`: libvirt snapshot XML plus `<displayname>`, `<creationLocaltime>`, `<lockSnapshot>`, `<createdBy>` (see below) |
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

## How Virtualization Station stores snapshots

Found by a read-only probe: one snapshot taken in the Virtualization
Station (VS) UI of a shut-off VM. VS makes an **external overlay**.

- The live disk becomes a new qcow2 file, `<disk>.<epoch>` (for example
  `vm-vda.img` gains `vm-vda.1794103287`), whose backing file is the
  previous image. The previous image is read-only from then on: it must
  never be changed or renamed while an overlay points at it.
- The domain definition (`.<uuid>.meta/<uuid>.xml`, the file
  `--definition` reads) is repointed at the overlay, with a
  `<backingStore>` for the image below it.
- libvirt knows the snapshot by the epoch alone (`virsh snapshot-list`
  shows `1794103287`, with its creation time). The name typed in the UI
  isn't a libvirt snapshot name. VS keeps a record per snapshot at
  `.<uuid>.meta/snapshot/<epoch>.xml`, and **lists a VM's snapshots from
  those files**: there's no snapshot table in its database. A record is
  the libvirt `<domainsnapshot>` (named and dated by the epoch, with a
  `<parent>` naming the previous snapshot's epoch, the overlay each disk
  started, and a copy of the domain), followed on one line by VS's own
  elements:

  ```
  <displayname>before update</displayname><creationLocaltime>1794074487</creationLocaltime><lockSnapshot>unlock</lockSnapshot><createdBy>user</createdBy>
  ```

  `creationLocaltime` is the same moment as local wall-clock time, stored
  as if it were UTC (the epoch minus 28800 in PST). Scheduled snapshots
  say `createdBy` `scheduling`, carry a `<description>`, and have no
  `lockSnapshot`.
- VS's database (Django, SQLite) keeps each disk's current top file in
  `vms_disk.path` and its bottom image in `vms_disk.root_path`. vmobj
  never writes there: the VM is created in the VS UI on the restored top
  layer, so VS records it itself.
- QNAP's virsh (8.6) has no `snapshot-list --details`, and the libvirt
  domain name is the VM's UUID, not its display name.

So, for the QNAP module:

- **Listing:** the snapshots are the chain under the disk `domblklist`
  reports, top to bottom. Each overlay's epoch is its date, and its name
  comes from VS's record.
- **Backing one up:** opening the chain down to overlay *N* gives the disk
  as it was at that snapshot. `internal/source`'s qcow2 reader already
  follows backing chains, so this needs no QEMU tools. The current state
  is the top overlay.
- **Recreating** (`restore-vm --history --platform qnap`): each disk is
  written as the chain VS would have made:

  ```
  NAME-vda.img          the first snapshot, a standalone qcow2 image
  NAME-vda.<epoch1>     overlay: the second snapshot (only what changed)
  ...
  NAME-vda.qcow2        overlay: the current state, the top layer
  ```

  The top layer ends in `.qcow2` because the VS disk picker lists only
  `.qcow2` files. Each snapshot gets one epoch for all disks (its creation
  time, a second later if two share a second), and its record names the
  overlay started on top of it. The records are left as a plan
  (`qnap-snapshots.json`). Once the VM has been created in the VS UI on
  the top layers, `vmobj qnap-snapshots` checks that the VM is shut off,
  that its disks are those top layers, and that no record exists yet, and
  then writes the records with a copy of the VM's new definition.

  `restore-vm --create --qvs-credentials FILE --os-type TYPE [--bridge BR]`
  does that step itself: it creates the VM through Virtualization
  Station's API (`POST /qvs/v2/vms`) on the top layers, using the backed-up
  definition's memory, vCPUs, firmware and disk buses, never sets a VNC
  password and never starts the VM, then writes the snapshot records as
  `qnap-snapshots` would. Without `--history` it creates the VM on a
  plain `NAME-vda.qcow2`. RESTORE.txt says which of these happened.

## Build order

1. **Part A, ZFS and TrueNAS** (no QNAP needed): snapshot fields in the
   manifest, `--source zfs:pool/zvol@snap`, the `Snapshotter` interface
   and its TrueNAS implementation, and `backup-vm --history` /
   `restore-vm --history`.
2. **qcow2 writer:** a restore can write qcow2, including an overlay on a
   given backing file holding only the clusters that changed. Checked
   with `qemu-img check` and `qemu-img compare` in CI.
3. **Part B, QNAP:** list, back up and recreate VS snapshots, on 1 and 2
   plus probe 2's findings.
4. **The test on real hardware:** a TrueNAS history restores on the QNAP
   as VS snapshots, and the VM boots from each.

## Done when

- A ZFS history backs up with `backup-vm --history` and rebuilds with
  `restore-vm --history`, matching what `zfs-replay.sh` gives today, with
  `vmobj:created` set.
- A TrueNAS history restores on the QNAP as Virtualization Station
  snapshots carrying their original dates, and the VM boots from each.
- `zfs-waypoints.sh` and `zfs-replay.sh` stay, as thin wrappers or moved to
  `scripts/legacy/` once `--history` covers them.
