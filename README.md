# VM Objects (`vmobj`)

Block-level backups of virtual machine disks. A full backup stores the disk
as 4 MiB blocks; incremental and differential runs store only the 64 KiB
extents that changed. Everything is compressed and encrypted on the host
before it is stored, and every restore point can be restored on its own.

**Status: early development (0.1.0-dev).** It backs up raw disks to a local
folder and restores them. S3, storage classes, Object Lock, host plugins
(ZFS, QEMU/libvirt) and VM definitions/TPM state come next. Don't rely on
it as your only backup yet.

## What works today

| Command | What it does |
| --- | --- |
| `vmobj key init` | Creates a 256-bit key file (mode 400). Keep a copy offline. |
| `vmobj init` | Creates a repository in a folder. |
| `vmobj measure` | Read-only: hashes a disk and reports how much changed since the last run, at 64 KiB and 4 MiB. Needs no key or repository. |
| `vmobj backup` | `--type full`, `incremental` or `differential`. |
| `vmobj list` | Lists restore points. |
| `vmobj restore` | Writes a restore point to a raw image file (sparse) or a block device. |
| `vmobj verify` | Checks a restore point's pieces are present; `--deep` also decrypts and hash-checks each one. |

Sources are raw image files or block devices, for example a ZFS zvol
snapshot (`/dev/zvol/tank/vms/win11@snap`). For qcow2, expose the disk as
raw first (`qemu-nbd`) or convert it with `qemu-img convert -O raw`.

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

Measure how much a disk changes per day (run it daily):

```bash
vmobj measure --source /mnt/scratch/win11.raw --name win11
```

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

The image doubles as an installer. It has no shell, so copy the file out
with `docker cp` from a container that is created but never started. This
puts `vmobj` in the current folder, built for the machine you run it on:

```bash
id=$(docker create vmobj) && docker cp "$id":/vmobj ./vmobj && docker rm "$id"
```

Once releases are published, use the ready-made image instead of building
one: replace `vmobj` above with `ghcr.io/wilminator/vm-objects:latest` (or
`:edge` for the newest build of `main`).

### Running from the image

The image suits jobs that only touch files and object storage. Mount the
key file and any folders it needs, and the S3 credentials at
`/home/vmobj/.aws`. Backups of VMs and ZFS datasets call the host's
`virsh` and `zfs`, so run the binary directly on the host for those.

## License

Mozilla Public License 2.0. Copyright (c) 2026 Michael Allen Wilmes.
