# VM Objects (`vmobj`)

Block-level backups of virtual machine disks. A full backup stores the disk
as 4 MiB blocks; incremental and differential runs store only the 64 KiB
extents that changed. Everything is compressed and encrypted on the host
before it is stored, and every restore point can be restored on its own.

**Status: early development (0.1.0-dev).** It backs up raw disks to a local
folder or an S3 bucket and restores them, with per-kind storage classes,
Object Lock and pruning. It reads raw disks, qcow2 images (with backing
chains) and ZFS zvols through snapshots, and backs up whole libvirt VMs
(disks, definition, UEFI NVRAM and TPM state). Don't
rely on it as your only backup yet.

## What works today

| Command | What it does |
| --- | --- |
| `vmobj key init` | Creates a 256-bit key file (mode 400). Keep a copy offline. |
| `vmobj init` | Creates a repository in a folder or an S3 bucket. |
| `vmobj measure` | Read-only: hashes a disk and reports how much changed since the last run, at 64 KiB and 4 MiB. Needs no key or repository. |
| `vmobj backup` | `--type full`, `incremental` or `differential`. |
| `vmobj list` | Lists restore points. |
| `vmobj restore` | Writes a restore point to a raw image file (sparse) or a block device; `--mode copy` or `in-place` updates an existing one, writing only what differs. |
| `vmobj backup-vm` | Backs up a whole libvirt VM: every disk, the definition, UEFI NVRAM and TPM state. |
| `vmobj vms` | Lists whole-VM backups. |
| `vmobj restore-vm` | Writes a VM's disks, adjusted definition, NVRAM and TPM state into a folder, with instructions. Never changes libvirt. |
| `vmobj copy` | Copies backups to another repository, sending only what it lacks; the destination applies its own profile and Object Lock. |
| `vmobj profiles` | Lists the storage profiles. |
| `vmobj prune` | Removes backups past their keep-until date and data nothing else uses. Reports only, unless `--apply`. |
| `vmobj verify` | Checks a restore point's pieces are present; `--deep` also decrypts and hash-checks each one. |

Sources:

- **Raw image files and block devices.**
- **qcow2 images**, read directly with their backing chains (compressed and
  zero clusters included). No QEMU tools or root needed. Images open in a
  running VM are marked dirty and refused: shut the VM down, or back up a
  snapshot.
- **`zfs:POOL/ZVOL`** snapshots the zvol, reads the snapshot through a
  temporary read-only clone, then removes both (`--keep-snapshot` keeps the
  snapshot).

Files named `*.qcow2` or `*.qcow` are read as qcow2 and everything else as
raw; `--format` overrides. The format is never guessed from a disk's
contents, so a guest can't make the host read files of its choosing by
writing a fake qcow2 header into its own disk.

## Quick start

Create a key and a repository:

```bash
vmobj key init --key-file /root/vmobj.key
```

```bash
vmobj init --repo /mnt/backup/vmobj --key-file /root/vmobj.key
```

Back up a snapshot, then later an incremental:

```bash
vmobj backup --repo /mnt/backup/vmobj --key-file /root/vmobj.key --source /dev/zvol/tank/vms/win11@daily --vm win11 --type full
```

```bash
vmobj backup --repo /mnt/backup/vmobj --key-file /root/vmobj.key --source /dev/zvol/tank/vms/win11@daily2 --vm win11 --type incremental
```

Restore one:

```bash
vmobj list --repo /mnt/backup/vmobj --key-file /root/vmobj.key --vm win11
```

```bash
vmobj restore --repo /mnt/backup/vmobj --key-file /root/vmobj.key --vm win11 --manifest 20261030T193649Z-incremental --output /mnt/scratch/win11.raw
```

### Restore modes

`--mode` chooses how `restore` writes its target:

| Mode | What it does | Target during the restore | Space |
| --- | --- | --- | --- |
| `rename` (default) | Writes a new file under a temporary name and renames it into place. A block device is written whole. | Never part-written | A full copy, beside the target |
| `copy` | Restores to a scratch file, then updates the existing target, writing only the 4 KiB pieces that differ. The target keeps its inode, owner and permissions. | Part old, part new while it's updated | A full copy in `--scratch` |
| `in-place` | Reads the existing target, hashes it with the repository key, and fetches and writes only what differs. | Part old, part new until it finishes; run it again to finish | None |

`copy` and `in-place` need an existing target: a file (resized to the disk
if needed) or a block device such as a zvol. `--check` reads the target
back afterwards and compares every block. Rolling a disk back to last
night with `in-place` writes only what changed since, and from S3 it
downloads only that too:

```bash
vmobj restore --repo /mnt/backup/vmobj --key-file /root/vmobj.key --vm win11 --manifest 20261030T193649Z-incremental --output /dev/zvol/tank/vms/win11 --mode in-place --check
```

`scripts/zfs-replay.sh` uses this to rebuild a zvol and its named
snapshots from a chain of backups, for moving a VM disk with its history.

Measure how much a disk changes per day (run it daily):

```bash
vmobj measure --source /mnt/scratch/win11.raw --name win11
```

## Whole VMs

```bash
vmobj backup-vm --repo /mnt/backup/vmobj --key-file /root/vmobj.key --domain win11 --type full
```

- A **shut-off** VM is read directly.
- A **running** VM's zvol disks are snapshotted together, atomically.
  `--quiesce` first freezes the guest's filesystems through the QEMU guest
  agent, and the thaw always runs.
- A running VM with disk *image files* is refused: snapshotting those live
  means changing the VM's disk chain, which vmobj won't do. Shut it down
  for the backup.
- The TPM state comes from `/var/lib/libvirt/swtpm/<uuid>`. Use
  `--swtpm-dir` elsewhere; on QNAP it's `/QVS/var/lib/libvirt/swtpm`, with
  `--virsh /QVS/usr/bin/virsh`.

`restore-vm --to DIR` writes everything into a folder and explains the next
steps in `RESTORE.txt`. It never registers the VM with libvirt.
`--new-name` renames the VM and drops its UUID, so the copy can run next to
the original. Restoring the TPM state is what keeps BitLocker and Windows
Hello PINs working.

## Several targets

`--copy-to REPO` (repeatable) on `backup` and `backup-vm` copies each new
backup to more repositories as soon as it's written, so one read of the
disk keeps, say, a local copy and an S3 copy:

```bash
vmobj backup-vm --repo /mnt/backup/vmobj --key-file /root/vmobj.key --domain win11 --type incremental --copy-to 's3://my-backups/nas1?region=us-west-2'
```

`vmobj copy --from A --to B` does the same afterwards, for everything or
for one backup, and is safe to repeat. Repositories with different keys
work too: vmobj re-encrypts as it copies.

## Interrupted runs

Nothing needs cleaning up after a backup, copy or restore stops part-way
(a dropped connection, Ctrl-C, a reboot). A connection that goes quiet is
caught by the stall watchdog and retried; if it never comes back, the run
stops with an error rather than hanging. Either way, run the same command
again:

- **backup and copy** find the blocks already stored and skip them, so
  only the rest is sent. A backup (or copy) is listed only once all of it
  is stored, so a broken run never looks like a good backup.
- **restore** (the default rename mode) keeps what it wrote in a hidden
  `.NAME.vmobj-partial` file next to the output, and never puts a partial
  image under the real name. The rerun reads that file back, keeps every
  block that already matches the backup, and fetches only the rest.
  `--no-resume` starts afresh instead.
- **restore-vm** keeps a `.vmobj-restore-vm` marker naming the backup in
  its folder until it finishes. Rerun into the same folder: finished disks
  are checked in place, and the unfinished one continues. A folder holding
  another backup's unfinished restore is refused.
- **in-place and staged restores** were already safe to rerun: they
  compare before writing.

## Repositories in S3

Anywhere `--repo` takes a folder, it also takes an S3 address:

```text
s3://BUCKET[/PREFIX][?region=R][&endpoint=URL][&profile=P][&credentials=/PATH][&path-style=true|false][&ip=4|6][&stall=DURATION]
```

- `region` is required for AWS unless `AWS_REGION` is set.
- `endpoint` points at an S3-compatible service, such as QNAP QuObjects or
  MinIO. Path-style addressing is switched on automatically when it's set.
- `profile` picks a section of `~/.aws/credentials`. Credentials are never
  part of the address: vmobj reads them from the standard AWS environment
  variables, files or instance role.
- `credentials` names another credentials file (an absolute path) to read
  instead of `~/.aws/credentials`, so a job run as root or from cron finds
  its keys without any environment variable.
- `stall` (default `2m`) is how long a request may move no data before
  it is cancelled and retried. A connection whose network path
  disappears (a route withdrawn, a NAT entry dropped) often never errors
  and would otherwise wait forever. Each request is tried up to four
  times, so a brief outage costs a retry, not the whole run. A slow link
  never trips it; only a dead one does.
- `ip=4` (or `ip=6`) makes every connection use only that IP version.
  Use it when a host's IPv6 route comes and goes (a router that announces
  it unreliably, say): the system prefers IPv6 whenever it has an address,
  and a long upload or restore breaks the moment that route drops.
- A `PREFIX` lets several repositories share one bucket.

```bash
vmobj init --repo 's3://my-backups/nas1?region=us-west-2' --key-file /root/vmobj.key
```

### Storage classes and profiles

A profile picks a storage class and a retention period for each kind of
backup. `vmobj profiles` lists them:

| Profile | Fulls and differentials | Incrementals |
| --- | --- | --- |
| `disaster-recovery` (default) | Deep Archive, 180 days | Standard-IA, 30 days |
| `fast-restore` | Glacier Instant Retrieval, 90 days | Standard-IA, 30 days |
| `standard` | Standard | Standard |
| `offsite` | Standard: fulls 1 year, differentials 90 days | Standard, 90 days |
| `cold` | Deep Archive, 1 year (for monthly fulls copied with `--record`) | Deep Archive, 180 days |
| `test` | Standard, 1 day | Standard, 1 day |

Use `--profile standard` or `offsite` with self-hosted services and
Backblaze B2, which have only Standard storage. `--class` overrides the class for a single run.
Manifests and indexes always stay in Standard, so listing and planning a
restore never wait on the archive. Restoring from Glacier Flexible Retrieval
or Deep Archive is two steps: `vmobj restore ... --request-restore` starts
the retrieval and reports the expected wait, and the same restore command
run later does the restore.

### Object Lock

`vmobj init --object-lock` on a bucket created with Object Lock makes every
object write-once: each one is locked in Governance mode for as long as the
backups that use it must be kept. Each run extends locks it relies on when
they would run out too soon, and fulls upload very old blocks again rather
than leaning on them forever. vmobj never sends the governance-bypass
header, so give its credentials no `s3:BypassGovernanceRetention`
permission: then neither vmobj nor anyone holding its keys can delete a
locked backup early.

`vmobj prune` deletes backups past their keep-until date. It always keeps
each VM's newest backup, never touches objects written in the last 48
hours, and leaves locked objects for a later run.

### Setting up AWS

`scripts/aws/provision.sh` creates a bucket with Object Lock and an IAM
user that can use only that bucket, and writes its key into a credentials
profile; `scripts/aws/teardown.sh` removes them. See
[docs/aws.md](docs/aws.md), for test buckets and for real backups.

## Checking it yourself

- `scripts/hardening.sh` damages repositories, kills and stops runs, fills
  the disk, and checks vmobj refuses clearly and recovers.
- `scripts/validate-vm.sh --domain NAME --work DIR` backs up one of your
  own (shut-down) VMs, copies it, restores it twice and compares every
  disk, the NVRAM and the TPM state with the original.

[docs/testing](docs/testing/README.md) has the full test plan and what has
passed so far.

## Progress output

`measure`, `backup`, `backup-vm`, `restore` and `restore-vm` show progress
on standard error. On a terminal it is one status line rewritten in place;
in a log or a cron job, a new line every 5 seconds. Choose with
`--progress line|lines|dots|json|none`, and shape the line with a template:

```bash
vmobj restore ... --progress-format '%P %p of %S (%s), %r left'
```

```text
10.2% 1.70 GiB of 16.67 GiB (432.70 MiB/s), 35s left
```

`{unit}` fixes a unit, binary (`{MiB}`, powers of 1024, the default) or
decimal (`{MB}`, powers of 1000); the unit you write is the unit printed.
`vmobj help progress` lists every code, unit and mode.

## How it stores data

- **Content IDs** are keyed BLAKE3 hashes, so the storage owner can't tell
  which blocks match known data.
- **Objects** are zstd-compressed, then encrypted with AES-256-GCM using a
  fresh nonce; the object's name is authenticated with it.
- **Full backups** reuse any 4 MiB block already stored, so fulls after the
  first upload only blocks that changed.
- **Incremental and differential runs** pack changed extents into packs
  with a separate table of contents. A differential re-stores every extent
  changed since the full, so older incremental packs can expire.
- **Manifests are written last**, after everything they point at.
- **Host state** (`~/.local/state/vmobj/<repo>/<vm>.state`) holds keyed
  hashes from the last run so incrementals don't read back from the
  repository. Losing it only means the next run must be a full.

## Building

Requires Go 1.24 or newer. The result is one static file with no
dependencies, suitable for copying onto TrueNAS or any Linux host.

```bash
make build
```

```bash
make test
```

### Building with Docker instead

If you'd rather not install Go, the `Dockerfile` builds in a container.
This writes `bin/vmobj-linux-amd64` and `bin/vmobj-linux-arm64`, the same
files `make build-all` makes:

```bash
docker build --target export --output type=local,dest=bin .
```

(`make docker-bin` runs the same command.) The default target is a small
image holding just `vmobj`, its root certificates and an unprivileged user:

```bash
docker build -t vmobj .
```

```bash
docker run --rm vmobj version
```

### Getting the binary out of an image

The image doubles as an installer: `vmobj install` copies the binary into
a folder you mount. This puts `vmobj` in the current folder, built for the
machine you run it on:

```bash
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD":/out vmobj install /out
```

`--user` makes the file yours (the image otherwise runs as an unprivileged
user that can't write to your folder). The copy is written under a
temporary name and renamed into place with mode 0755, so it can safely
replace a vmobj that is running; installing the same version again changes
nothing.

Images made before `install` existed have no shell to run `cp`, so for
those copy the file out of a container that is created but never started:

```bash
id=$(docker create vmobj) && docker cp "$id":/vmobj ./vmobj && docker rm "$id"
```

Once releases are published, use the ready-made image instead of building
one: replace `vmobj` above with `ghcr.io/wilminator/vmobj:latest` (or
`:edge` for the newest build of `main`). The same image is also published
as `ghcr.io/wilminator/vm-objects`, after the repository.

### Running from the image

The image suits jobs that only touch files and object storage. Mount the
key file and any folders it needs, and the S3 credentials at
`/home/vmobj/.aws`. Backups of VMs and ZFS datasets call the host's
`virsh` and `zfs`, so run the binary directly on the host for those.

## License

Mozilla Public License 2.0. Copyright (c) 2026 Michael Allen Wilmes.
