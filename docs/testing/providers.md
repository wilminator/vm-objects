# Testing against several S3 services

`scripts/providers-matrix.sh` runs the same checks against every service it
is given, and copies backups between each pair. The buckets can be the ones
that hold your real backups: each run works under its own prefix (the time
it started), and its backups use the `test` profile, locked one day.

## The services

| Name | What | Object Lock | Notes |
| --- | --- | --- | --- |
| aws | AWS S3, bucket from `scripts/aws/provision.sh` | yes | |
| b2 | Backblaze B2, bucket from `scripts/b2/provision.sh` | yes | one storage class |
| quobjects | QNAP QuObjects | check | test only; retired with the QNAP |
| minio | MinIO, in CI | yes | two prefixes, `--only-pairs` |

## Running it

```bash
scripts/providers-matrix.sh --vmobj ./vmobj \
  "aws=s3://BUCKET/test?region=REGION&profile=PROFILE" \
  "b2=s3://BUCKET/test?endpoint=https://s3.REGION.backblazeb2.com&region=REGION&profile=PROFILE" \
  "quobjects=s3://BUCKET/test?endpoint=https://NAS:PORT&region=us-east-1&profile=PROFILE"
```

Add `--no-lock NAME` for a service without Object Lock. For each service it
runs `scripts/e2e.sh` plainly and with Object Lock; then for each ordered
pair (six with three services) it copies a full and an incremental, restores
both from the copy, and checks a second copy sends nothing. It ends with a
PASS/FAIL table.

## The S3 conformance tests

The engine's own S3 tests run against any bucket too (they need Go):

```bash
VMOBJ_TEST_S3='s3://BUCKET/conformance?endpoint=...&region=...&profile=...' VMOBJ_TEST_S3_LOCKS=1 go test ./internal/store/ -run 'TestS3(Conformance|Locks)$' -v
```

## Archive classes

Only AWS has them. One full backed up with `--profile cold` (Deep Archive)
proves the archive path: `vmobj restore` reports it archived, `--request-restore`
asks AWS to thaw it, and the restore works 12 to 48 hours later.
