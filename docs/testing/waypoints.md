# Real-data test: a zvol's snapshots as waypoints

This test runs a chain of incremental backups over real changes, which the
milestone tests can't. Take a zvol that already has snapshots, oldest
first: the script backs up the first as a full and each later one as an
incremental, then restores every backup and checks that each one is
byte-identical to its snapshot.

It was first run on 2026-11-06, on eight snapshots of the 250 GiB WinGames
disk. Between those snapshots came activation, a CPU change, a Windows 11
feature update, a driver update, a disk-controller change and a chipset
change. All eight restores matched, including one waypoint with no
changes at all, which backed up to 0 B.

## Run it

Run it as root on the ZFS host. The work folder must be new. Put it on a
different pool from the zvol if you can, because reading and writing one
pool at once is slower.

```bash
sudo scripts/zfs-waypoints.sh backup POOL/vms/myvm /mnt/OTHERPOOL/waypoints bin/vmobj
```

```bash
sudo scripts/zfs-waypoints.sh verify /mnt/OTHERPOOL/waypoints bin/vmobj
```

`backup` prints each snapshot's hash as it goes and a summary per backup.
The summaries are also saved in `backups.log`. `verify` prints one
`OK` or `FAIL` line per snapshot, then `PASS` or `FAIL`.

The script reads each snapshot through a read-only clone named
`ZVOL-wp`, and destroys that clone afterwards; the snapshots themselves are
never changed. Each snapshot's disk is read twice: once for the hash and
once for the backup.

## What to look at

- **`Written:`** for each incremental: how much each real change cost.
- **The "reused" counts:** extents already stored by an earlier backup.
  These show deduplication working across the chain.
- **Snapshots with no changes:** they should write 0 B.

## Clean up

The work folder holds a full repository, so it is as large as the
backups. Remove it when you're done:

```bash
sudo rm -rf /mnt/OTHERPOOL/waypoints
```
