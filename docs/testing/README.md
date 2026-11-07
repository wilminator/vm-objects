# Testing vmobj

CI runs the unit tests, and MinIO and ZFS end-to-end tests, on every pull
request. The pages here are the tests CI can't do: real disks, real VMs,
real S3 services, and what happens when things go wrong.

Run them from a clone of `main`:

```bash
git clone https://github.com/wilminator/vm-objects.git ~/vm-objects && cd ~/vm-objects && make build
```

## Where things stand

| Area | Page | Status |
| --- | --- | --- |
| qcow2 and ZFS sources | [m4-vm-sources.md](m4-vm-sources.md), [waypoints.md](waypoints.md) | Passed on the QNAP and TrueNAS, Nov 2026: byte-identical restores, 8 ZFS waypoints |
| Whole VMs | [m5-whole-vm.md](m5-whole-vm.md) | Passed on the QNAP: disk and TPM state identical |
| Copy and `--copy-to` | [m6-copy.md](m6-copy.md) | Passed on the QNAP: restore from the copy identical; a repeat copy sends nothing |
| Failure safety | [hardening.md](hardening.md) | `scripts/hardening.sh` passes, and runs in CI; still to run on the NAS |
| A real VM, end to end | [hardening.md](hardening.md#a-real-vm-scriptsvalidate-vmsh) | `scripts/validate-vm.sh`; its self-test runs in CI. Still to run on TrueNAS |
| S3 backend | [m1-s3-backend.md](m1-s3-backend.md) | CI only (MinIO). To do: QuObjects, then AWS |
| Storage classes | [m2-storage-classes.md](m2-storage-classes.md) | CI only. To do: AWS archive restore |
| Object Lock and prune | [m3-object-lock.md](m3-object-lock.md) | CI only. To do: QuObjects, then AWS |

## Suggested order

1. **[Failure safety](hardening.md)**, on the NAS: about ten minutes,
   no setup.
2. **`validate-vm.sh` on a real VM**, then the manual checks on the
   hardening page: a restore on a different host, running from the Docker
   image, and timing baselines.
3. **QuObjects**: milestones 1 and 3, part A. Free, on the QNAP.
4. **AWS**: set up a test bucket with [the AWS scripts](../aws.md), run
   milestones 1 to 3, part B, then tear it down.
5. **Real backups to AWS**: the same scripts with `--purpose backups`
   ([how](../aws.md#the-bucket-for-real-backups)).

The milestone pages were written while each milestone was on its own
branch; they now build from `main`.
