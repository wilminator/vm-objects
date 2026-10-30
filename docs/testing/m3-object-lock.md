# Milestone 3 testing: Object Lock, retention and prune

What this milestone adds:

- `vmobj init --object-lock` makes a repository where every object is
  written with an S3 **Governance-mode lock**. The setting is fixed at init
  and protected by the config's MAC, so it can't be switched off later by
  editing the stored config. Init refuses buckets without Object Lock, and
  local folders.
- **The lock rule.** Every backup makes sure each object it relies on is
  locked at least until its own keep-until date plus one day. Locks that
  fall short are extended. During a full, blocks older than the profile's
  refresh age (180 days by default) are uploaded fresh instead.
- **vmobj never sends the governance-bypass header.** No vmobj command, and
  no bug in one, can delete a locked backup or shorten its lock.
- `vmobj prune` removes backups past their keep-until date, and the data
  only they used. It is a dry run unless `--apply` is given. It always keeps
  each VM's newest backup, skips anything written in the last 48 hours, and
  skips anything still locked, reporting when it unlocks.

CI checks all of this against the fake S3, which models versions and locks,
and against MinIO built from source: the lock tests, plus a full end-to-end
run on a locked repository.

> **Heads-up before you start.** Anything written to a locked repository
> stays locked for its retention period plus a month: about four months
> with the `standard` profile. On the QNAP or on AWS, deleting it sooner
> needs an administrator account with the bypass permission. The tests
> below write only a few MB, so this costs nothing that matters. Just use
> a separate test bucket.

Run from **WSL**:

```bash
cd ~/vm-objects && git fetch && git checkout m3-object-lock && make build && make test
```

## A. QuObjects or MinIO

### 1. Make a bucket with Object Lock

Object Lock can only be turned on when a bucket is created. Use a new
bucket named `vmobj-locked`.

- **QuObjects:** create the bucket in the QuObjects web page and look for an
  Object Lock (or WORM) option when creating it. If the page has no such
  option, try the helper below against the QuObjects endpoint instead.
- **MinIO, or QuObjects without the option in its page:**

  ```bash
  go run ./scripts/mkbucket "s3://vmobj-locked?$S3_OPTS"
  ```

### 2. The lock tests

These use real locks lasting a few seconds, so they leave nothing locked
behind:

```bash
VMOBJ_TEST_S3="s3://vmobj-locked?$S3_OPTS" VMOBJ_TEST_S3_LOCKS=1 go test -count=1 -v -run 'Locks' ./internal/store/
```

Expect `--- PASS: TestS3Locks`. If QuObjects fails here, the first failing
line tells us which part of Object Lock it doesn't support, and that is the
most useful thing you can send me.

### 3. End-to-end on a locked repository

```bash
E2E_BACKUP_ARGS="--profile standard" E2E_INIT_ARGS="--object-lock" scripts/e2e.sh "s3://vmobj-locked/e2e-$(date +%s)?$S3_OPTS" bin/vmobj
```

Expect `PASS`. The init step should print "Object Lock is on".

### 4. Locks really hold

Pick one of the objects the e2e run just wrote and try to delete it from
the QuObjects web page (or with any S3 tool), using the `vmtest`
credentials. It should be refused. A delete in a versioned bucket may
instead *appear* to work by adding a "delete marker". If so, check that the
object's earlier version is still listed when the page shows versions.

## B. Real AWS (pennies)

1. Create a **new** bucket, `vmobj-locked-<something unique>`, in your usual
   region. In the console, under *Advanced settings*, turn **Object Lock**
   on (this also turns on versioning). Leave "default retention" **off**:
   vmobj sets every lock itself.

2. Give the test IAM user these permissions on that bucket, and **not**
   `s3:BypassGovernanceRetention`:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": [
           "s3:PutObject", "s3:GetObject", "s3:GetObjectVersion", "s3:DeleteObject",
           "s3:DeleteObjectVersion", "s3:RestoreObject", "s3:PutObjectRetention",
           "s3:GetObjectRetention"
         ],
         "Resource": "arn:aws:s3:::BUCKET/*"
       },
       {
         "Effect": "Allow",
         "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketObjectLockConfiguration"],
         "Resource": "arn:aws:s3:::BUCKET"
       }
     ]
   }
   ```

3. The lock tests, which leave nothing locked:

   ```bash
   VMOBJ_TEST_S3='s3://BUCKET?region=us-west-2&profile=vmobj-aws' VMOBJ_TEST_S3_LOCKS=1 go test -count=1 -v -run 'Locks' ./internal/store/
   ```

4. End-to-end on a locked repository:

   ```bash
   E2E_BACKUP_ARGS="--profile standard" E2E_INIT_ARGS="--object-lock" scripts/e2e.sh "s3://BUCKET/e2e-$(date +%s)?region=us-west-2&profile=vmobj-aws" bin/vmobj
   ```

5. In the S3 console, open one of the objects under the new `e2e-…/blocks/`
   prefix. Its **Object Lock** panel should show *Governance* mode, with a
   retain-until date about four months out. Under `manifests/`, the date
   should be about three months out.

## C. Prune (any of the services above)

On the repository from step A3 or B4, a dry run should find nothing to do:

```bash
~/vm-objects/bin/vmobj prune --repo "REPO_ADDRESS_FROM_THE_E2E_RUN" --key-file PATH_TO_THE_KEY
```

The e2e script deletes its key at the end, so for this step make a
repository by hand. `init --object-lock`, a `full` and an `incremental`
backup with `--profile standard` are enough. Then run the prune above, and
again with `--apply`. Expect "Past keep-until: 0" and nothing deleted:
everything is still within retention, which is the point.

## What to report back

- Which services passed the lock tests (A2, B3), and for any failure, its
  first failing line.
- PASS or FAIL for the locked e2e runs (A3, B4).
- Whether the delete in A4 was refused (or only added a delete marker).
- The lock dates you saw in B5.
