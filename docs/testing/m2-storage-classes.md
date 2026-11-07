# Milestone 2 testing: storage classes and profiles

What this milestone adds:

- **Profiles** (`vmobj profiles`) assign a storage class and a retention
  period to each kind of backup. The default, `disaster-recovery`, sends
  fulls and differentials to Deep Archive and incrementals to Standard-IA.
- `backup --profile NAME` and `backup --class CLASS` (a one-run override).
- Manifests, pack indexes and the config always stay in Standard, so
  `list`, `verify` and restore planning never wait on the archive.
- `restore` checks first whether anything it needs is archived. If so, it
  writes nothing, shows the wait per retrieval tier, and with
  `--request-restore` starts the retrieval. Run it again once the retrieval
  finishes and it restores normally.
- `list --long` shows each backup's class and keep-until date.

CI covers all of this against the in-memory fake S3, and the Standard-class
path against MinIO. Self-hosted services (QuObjects, MinIO) don't have the
archive classes, so **the real archive behaviour can only be tested on AWS**.
Part B costs well under $1.

Run from **WSL**, in the clone from milestone 1:

```bash
cd ~/vm-objects && git pull && make build && make test
```

## A. QuObjects or MinIO (free)

Use the same bucket and `S3_OPTS` as milestone 1.

1. The end-to-end script with the Standard profile:

   ```bash
   E2E_BACKUP_ARGS="--profile standard" scripts/e2e.sh "s3://vmobj-test/m2-$(date +%s)?$S3_OPTS" bin/vmobj
   ```

   Expect `PASS`, with every backup reporting `Storage: S3 Standard`.

2. Check that the default profile fails with a clear explanation rather
   than an obscure error. Make a small disk and a repository:

   ```bash
   mkdir -p ~/vmobj-m2 && cd ~/vmobj-m2
   ~/vm-objects/bin/vmobj key init --key-file m2.key
   truncate -s 8M disk.raw && dd if=/dev/urandom of=disk.raw bs=1M count=2 conv=notrunc status=none
   export REPO="s3://vmobj-test/m2-default-$(date +%s)?$S3_OPTS"
   ~/vm-objects/bin/vmobj init --repo "$REPO" --key-file m2.key
   ```

   ```bash
   ~/vm-objects/bin/vmobj backup --repo "$REPO" --key-file m2.key --source disk.raw --vm m2 --type full
   ```

   Expect a failure whose message says the service does not support
   `DEEP_ARCHIVE` and suggests `--profile standard`. If QuObjects instead
   *accepts* Deep Archive, please tell me, because that would be surprising.

## B. Real AWS: archive, restore request, restore (under $1)

Use the `vmobj-aws` profile and bucket from milestone 1. A bucket and user
made by `scripts/aws/provision.sh` already have everything this part needs.
For a user you set up by hand, the IAM policy needs one more permission,
`s3:RestoreObject`, so the first statement's Action becomes:

```json
"Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject", "s3:RestoreObject"]
```

Glacier Flexible Retrieval is used here because its Expedited tier finishes
in minutes. Deep Archive's fastest tier takes 12 hours, which comes in part C.

1. Make a repository and a full backup in Flexible Retrieval:

   ```bash
   cd ~/vmobj-m2
   export REPO="s3://BUCKET/m2-$(date +%s)?region=us-west-2&profile=vmobj-aws"
   ~/vm-objects/bin/vmobj init --repo "$REPO" --key-file m2.key --block-size 1MiB
   ```

   ```bash
   ~/vm-objects/bin/vmobj backup --repo "$REPO" --key-file m2.key --source disk.raw --vm m2 --type full --class flexible --state-dir ./state
   ```

   Expect `Storage: S3 Glacier Flexible Retrieval`. In the AWS console, the
   objects under `blocks/` should show storage class *Glacier Flexible
   Retrieval*, and the ones under `manifests/` should show *Standard*.

2. Listing and a quick verify work at once, with no restore needed:

   ```bash
   ~/vm-objects/bin/vmobj list --repo "$REPO" --key-file m2.key --long
   ```

   ```bash
   ~/vm-objects/bin/vmobj verify --repo "$REPO" --key-file m2.key --vm m2 --manifest NAME_FROM_LIST --deep
   ```

   The deep verify should say the pieces are archived and were checked for
   presence only, then `OK`.

3. A restore refuses to start, shows the plan, and writes nothing:

   ```bash
   ~/vm-objects/bin/vmobj restore --repo "$REPO" --key-file m2.key --vm m2 --manifest NAME_FROM_LIST --output restored.raw
   ```

   Expect "This backup is in archive storage. Nothing was written." with the
   wait for each tier, and no `restored.raw` file afterwards.

4. Request an Expedited restore for one day:

   ```bash
   ~/vm-objects/bin/vmobj restore --repo "$REPO" --key-file m2.key --vm m2 --manifest NAME_FROM_LIST --output restored.raw --request-restore --tier expedited --days 1
   ```

5. Wait 5 to 10 minutes, then run the plain restore from step 3 again. It
   should now restore. Compare the result with the original:

   ```bash
   cmp disk.raw restored.raw && echo IDENTICAL
   ```

## C. Optional: Deep Archive (needs a day)

Repeat part B with `--class deep-archive`, using `--tier standard` in step 4
(Deep Archive has no Expedited tier; asking for it should give a clear
error). Check back in about 12 hours. The test data is a few MB, but Deep
Archive bills a minimum of 180 days, so this costs a fraction of a cent
that you can't get back by deleting early.

## Clean up

Delete the `m2-*` prefixes from the buckets when you're done. On AWS the
Glacier objects bill their minimum days regardless, at a fraction of a cent.

## What to report back

- A: PASS for the e2e script, and the exact error from the default-profile
  backup.
- B: the output of steps 3 and 4, and whether step 5 printed `IDENTICAL`.
- C, if you ran it: whether the Expedited request was refused clearly, and
  whether the restore worked after the wait.
