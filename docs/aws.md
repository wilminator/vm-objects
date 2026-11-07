# Setting up AWS for vmobj

Two scripts set up and remove everything vmobj needs in AWS:

- `scripts/aws/provision.sh` creates a bucket, an IAM user that can use
  only that bucket, and an access key written straight into a credentials
  file.
- `scripts/aws/teardown.sh` removes all of it again.

The same scripts serve a throwaway **test** bucket and the bucket for your
**real backups**. The only difference is `--purpose`, which decides how
hard teardown makes it to delete the bucket.

## What you need once

- **The AWS CLI v2 and `jq`** on the machine you run the scripts from (WSL
  is fine):

  ```bash
  sudo apt install -y jq unzip && curl -sSLo /tmp/awscliv2.zip https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip && unzip -q /tmp/awscliv2.zip -d /tmp && sudo /tmp/aws/install
  ```

- **Administrator credentials for the AWS CLI**, as a named profile. IAM
  Identity Center (SSO) is the best choice; an IAM user with
  `AdministratorAccess` works too. Don't use the account's root keys.

  ```bash
  aws configure sso --profile admin
  ```

  Check it with:

  ```bash
  aws sts get-caller-identity --profile admin
  ```

These administrator credentials only run the scripts. vmobj never gets
them, and they should never be copied to a NAS.

## What the scripts create

| Piece | Settings |
| --- | --- |
| Bucket | Object Lock on (so versioning too), public access blocked, AES-256 default encryption, tagged `vmobj:purpose`. No default retention: vmobj sets each object's lock itself. |
| Lifecycle rule | Clears multipart uploads unfinished after 7 days and delete markers with nothing behind them. It never removes data a backup uses. |
| IAM user | `vmobj-BUCKET` by default, with one inline policy: object read, write, delete, restore and retention calls on this bucket only. |
| Explicit deny | `s3:BypassGovernanceRetention`, and changes to the bucket's lock, policy, lifecycle and versioning, and deleting it. A leaked vmobj key can't remove a locked backup early or loosen the bucket. |
| Access key | Written to `~/.aws/credentials` (or `--credentials-file`) as profile `vmobj-BUCKET`. The secret is never printed. |

Every option is listed by `--help`. `--dry-run` shows the steps and changes
nothing.

## A test bucket

Bucket names are global across AWS, so add something of your own:

```bash
scripts/aws/provision.sh --admin-profile admin --region us-west-2 --bucket vmobj-test-yourname
```

It ends by printing the repository address, for example
`s3://vmobj-test-yourname?region=us-west-2&profile=vmobj-vmobj-test-yourname`.
That's what the milestone 1 to 3 test pages call `BUCKET` with its
`region` and `profile`.

When the tests are done, remove everything:

```bash
scripts/aws/teardown.sh --admin-profile admin --region us-west-2 --bucket vmobj-test-yourname
```

Locked test objects are deleted with the governance bypass, which your
administrator profile has. Glacier and Deep Archive objects still bill
their minimum storage days (90 and 180) after deletion: a fraction of a
cent for test data.

## The bucket for real backups

```bash
scripts/aws/provision.sh --admin-profile admin --region us-west-2 --bucket vmobj-backups-yourname --purpose backups --credentials-file ./vmobj-nas.credentials
```

`--credentials-file` writes the key into its own file here, rather than
into your own `~/.aws/credentials`, so you can carry it to the NAS.

**On the NAS** (as root, since vmobj runs as root there for zvols and
`virsh`), put the file in place and delete the copy you carried over:

```bash
sudo install -d -m 700 /root/.aws && sudo install -m 600 vmobj-nas.credentials /root/.aws/credentials && rm vmobj-nas.credentials
```

If `/root/.aws/credentials` already exists, append the file's contents
instead of replacing it.

Make the repository. Use a prefix per host, so one bucket can hold several:

```bash
sudo vmobj init --object-lock --repo 's3://vmobj-backups-yourname/truenas?region=us-west-2&profile=vmobj-vmobj-backups-yourname' --key-file /root/vmobj.key
```

Then back up to the local repository and copy each new backup to S3 in the
same run:

```bash
sudo vmobj backup-vm --repo /mnt/fast/vmobj --key-file /root/vmobj.key --domain win11 --type incremental --copy-to 's3://vmobj-backups-yourname/truenas?region=us-west-2&profile=vmobj-vmobj-backups-yourname'
```

The S3 copy uses the `disaster-recovery` profile unless you choose another:
fulls and differentials in Deep Archive for 180 days, incrementals in
Standard-IA for 30. `vmobj profiles` lists the others.

### Before you rely on it

- **Keep the vmobj key file somewhere other than the NAS** (a password
  manager, a printed copy). The S3 copy is useless without it.
- **Restore from S3 once**, onto spare space, before you need to. Deep
  Archive restores take 12 to 48 hours, so do it with a small VM, or with a
  backup copied using `--profile standard`.
- **Who can delete what.** vmobj's key can't delete a locked backup early.
  Your administrator credentials can (that's what Governance mode means), so
  keep them off the NAS and protected with MFA.

Deleting the backups bucket needs its name twice, on purpose:

```bash
scripts/aws/teardown.sh --admin-profile admin --region us-west-2 --bucket vmobj-backups-yourname --destroy-backups vmobj-backups-yourname
```
