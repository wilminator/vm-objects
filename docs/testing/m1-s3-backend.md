# Milestone 1 testing: S3 backend

What this milestone adds: `--repo` accepts an `s3://` address, so every
command (`init`, `backup`, `list`, `restore`, `verify`) can use an S3 bucket
on AWS or an S3-compatible service such as QNAP QuObjects or MinIO.

CI already runs every check below against MinIO. What CI can't tell us is
whether *your* S3 services behave the same, so these steps repeat the checks
against QuObjects on the QNAP, with a MinIO container as the fallback, and
optionally against real AWS.

Run everything from your **WSL** shell. Keep the work in your WSL home folder
(not under `/mnt/c`), since the Windows-mounted drives are much slower.

## 1. Get the code and Go

Install Go 1.24 or newer, if `go version` doesn't already show it:

```bash
cd ~
curl -LO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc && source ~/.bashrc
go version
```

Clone the repository and build it:

```bash
git clone https://github.com/wilminator/vm-objects.git ~/vm-objects
cd ~/vm-objects
make build && make test
```

`make test` should end with every package `ok`.

## 2. Set up an S3 service to test against

### Option A: QuObjects on the QNAP

1. In QTS, open **App Center**, search for **QuObjects**, and install it.
2. Open QuObjects. Create a **user**, then an **access key** for that user.
   Keep the secret key on screen; you'll paste it into a file in step 3.
3. Create a bucket named `vmobj-test`.
4. Note the S3 endpoint: the NAS address plus the port QuObjects shows for
   its S3 service (on its overview or settings page). It looks like
   `http://192.168.x.x:PORT`, or `https://` if you enabled HTTPS.

### Option B: MinIO on your computer

If QuObjects isn't available, run MinIO in WSL, the same way CI does. MinIO
stopped publishing ready-made images and binaries in October 2025, so build
its last release from source (this takes a few minutes the first time):

```bash
GOBIN="$HOME/minio-bin" go install github.com/minio/minio@RELEASE.2025-10-15T17-29-55Z
```

Start it in a second WSL window and leave it running:

```bash
mkdir -p ~/minio-data && MINIO_ROOT_USER=vmobj-test MINIO_ROOT_PASSWORD=vmobj-test-local-only ~/minio-bin/minio server ~/minio-data --address 127.0.0.1:9000
```

Its endpoint is `http://127.0.0.1:9000`, the access key is `vmobj-test`
and the secret is `vmobj-test-local-only`. Create the bucket in step 3, once
the credentials are in place.

## 3. Store the credentials in a file

vmobj reads credentials only from the standard AWS files and environment,
never from the command line. Create a profile named `vmtest`:

```bash
mkdir -p ~/.aws && chmod 700 ~/.aws
nano ~/.aws/credentials
```

Add this section, with your access key and secret:

```ini
[vmtest]
aws_access_key_id = YOUR_ACCESS_KEY
aws_secret_access_key = YOUR_SECRET_KEY
```

Save the file, then lock it down:

```bash
chmod 600 ~/.aws/credentials
```

For MinIO (Option B) only, create the bucket now, with Object Lock turned on
(milestone 3 uses it). The repository includes a small helper for this, so
the AWS CLI isn't needed:

```bash
go run ./scripts/mkbucket 's3://vmobj-test?endpoint=http://127.0.0.1:9000&profile=vmtest'
```

Set a variable holding the connection options, replacing the endpoint with
yours (`http://127.0.0.1:9000` for MinIO):

```bash
export S3_OPTS='endpoint=http://NAS_ADDRESS:PORT&profile=vmtest'
```

## 4. Run the checks

**Conformance tests.** These run the same tests that the local folder backend
and the in-memory fake pass. Each run uses a fresh prefix and cleans up after
itself.

```bash
VMOBJ_TEST_S3="s3://vmobj-test?$S3_OPTS" go test -count=1 -v -run Conformance ./internal/store/
```

Expect `--- PASS: TestS3Conformance` with every subtest passing.

**End-to-end with the real binary.** This runs a full, an incremental and a
differential backup of a 40 MiB test disk, then restores and verifies each one
byte-for-byte:

```bash
scripts/e2e.sh "s3://vmobj-test/e2e-$(date +%s)?$S3_OPTS" bin/vmobj
```

It should end with `PASS: all end-to-end checks`. If it doesn't, the output
names the step that failed, and that is what I need to see.

## 5. Optional: real AWS

Only do this if you want to spend a few cents to confirm AWS itself.

**The quick way:** `scripts/aws/provision.sh` creates the bucket, an IAM user
with exactly the permissions milestones 1 to 3 need, and its credentials
profile, and `scripts/aws/teardown.sh` removes them afterwards. See
[Setting up AWS for vmobj](../aws.md#a-test-bucket). Use the bucket and
profile it prints in place of `BUCKET` and `vmobj-aws` below, and skip to the
two test commands.

**By hand:** create a bucket (for example `vmobj-test-<something unique>`)
in your usual region, and an IAM user whose only permission is this policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::BUCKET/*"
    },
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::BUCKET"
    }
  ]
}
```

Put that user's key in `~/.aws/credentials` as a profile named `vmobj-aws`,
then run:

```bash
VMOBJ_TEST_S3='s3://BUCKET?region=us-west-2&profile=vmobj-aws' go test -count=1 -v -run Conformance ./internal/store/
```

```bash
scripts/e2e.sh "s3://BUCKET/e2e-$(date +%s)?region=us-west-2&profile=vmobj-aws" bin/vmobj
```

Delete the bucket's contents afterwards (`scripts/aws/teardown.sh` does it for a provisioned bucket).

## What to report back

- Which service you used (QuObjects version, or MinIO).
- PASS, or the failing test's name and its last 20 lines of output.
- Never paste the contents of `~/.aws/credentials` or a key file.
