# Milestone 7 testing: block-level restore

What this milestone adds: `restore --mode copy` and `--mode in-place`, which
update an existing file or block device and write only the 4 KiB pieces
that differ, and `--check`, which reads the target back afterwards. Plus
two tools built on them: `scripts/zfs-replay.sh`, which rebuilds a zvol and
its named snapshots from a chain of backups, and `zfs-waypoints.sh add`.

CI runs the unit tests, `scripts/restore-modes-e2e.sh` (a file and a loop
device stepped through four restore points and back, with exact write
counts) and, on its ZFS runner, `scripts/zfs-replay-e2e.sh` (four snapshots
replayed into a new zvol and compared with the originals). The real test is
moving WinGames.

## 1. The new binary on TrueNAS

Have Claude Code build this branch and copy it to the NAS next to the old
binary, renaming the old one, as before.

A one-minute check that it works there, including the block-device path
(it makes and removes its own loop device):

```bash
sudo scripts/restore-modes-e2e.sh /mnt/staging/vmobj-test/vmobj
```

Expect `PASS: restore modes end to end`.

## 2. Move WinGames, snapshots included

The plan: the eight waypoint backups already in the work folder from the
waypoint test, plus one more of WinGames as it is now, are replayed into a
new zvol on `fast`. Each step writes only what changed since the step
before, has its sha256 checked against the original snapshot, and becomes
a snapshot of the same name. `WORKDIR` below is the folder you gave
`zfs-waypoints.sh backup`; it holds `repo`, `test.key` and `hashes.txt`.

WinGames must stay shut down from step 2a until it boots in step 2e.

**a. Capture WinGames as it is now**, as a ninth waypoint:

```bash
sudo zfs snapshot staging/vms/wingames@before-move
```

```bash
sudo scripts/zfs-waypoints.sh add staging/vms/wingames@before-move WORKDIR /mnt/staging/vmobj-test/vmobj
```

It reads the zvol twice (once to hash it, once to back it up) and appends
`before-move` to `hashes.txt`.

**b. Check the room on `fast`.** The new zvol needs its full size, plus
whatever the snapshots hold:

```bash
zfs get -H -o value volsize,refreservation staging/vms/wingames && zfs list -o name,used,avail fast
```

**c. Replay.** The parent dataset first, if it doesn't exist yet:

```bash
sudo zfs create -p fast/vms
```

```bash
sudo scripts/zfs-replay.sh --work WORKDIR --zvol fast/vms/wingames --like staging/vms/wingames --vmobj /mnt/staging/vmobj-test/vmobj
```

Each of the nine steps prints what it wrote, `sha256 matches the original
@…`, and the snapshot it took. It ends with `PASS` and the list of
snapshots. If it stops for any reason, fix the cause and run the same
command with `--resume` added: finished snapshots are skipped.

**d. Compare the snapshots with the originals.** The `written` column is
the data each snapshot added; the replayed ones should be close to the
originals:

```bash
zfs list -t snapshot -d 1 -o name,written -s creation staging/vms/wingames fast/vms/wingames
```

**e. Point WinGames at the new zvol and boot it.** In the TrueNAS web UI:
**Virtualization → WinGames → Devices**, open the **Disk** device, **Edit**,
choose `fast/vms/wingames` as the zvol and **Save**. Leave every other
setting as it is (VirtIO, the I/O type). Then check that only the path
changed (only the ID, path, type and I/O type are printed):

```bash
midclt call vm.device.query '[["vm","=",1],["attributes.dtype","=","DISK"]]' | jq '.[] | {id, path: .attributes.path, type: .attributes.type, iotype: .attributes.iotype}'
```

Start the VM. Check that Windows is still activated and that your PIN
still works (the TPM state didn't move, so it should).

**f. Keep the old zvol** on `staging` until you're satisfied; it's the
fallback. Switching back is the same edit, choosing `staging/vms/wingames`.

## What to report back

- Step 1: PASS, or the failing line.
- Step 2c: the nine `Written:` lines, and the final PASS.
- Step 2d: the two `written` lists side by side.
- Step 2e: whether WinGames boots, stays activated and accepts the PIN.
