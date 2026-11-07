# Milestone 6 testing: several targets and `vmobj copy`

What this milestone adds:

- **`vmobj copy --from REPO --to REPO`** copies backups between
  repositories:
  - everything (`--vm` limits it to one VM);
  - one disk backup (`--vm win11.vda --manifest NAME`);
  - or one whole-VM backup (`--vm win11 --record NAME`).

  Only what the destination lacks is sent, so run it as often as you like.
- **Same key on both sides:** blocks travel as stored, encrypted, and each
  is checked on the way. **Different keys:** each piece is decrypted,
  re-hashed and sealed again for the destination. This is the migration
  path between keys.
- **The destination applies its own policy.** `--profile` sets retention
  and class, counted from when the backup was originally made, and the
  destination's Object Lock is applied. A backup that would already be past
  its retention at the destination is skipped, with a note.
- **`--copy-to REPO`** (repeatable) on `backup` and `backup-vm` copies the
  new backup straight away: one read of the disk feeds every target. The
  backup counts as done even if a copy fails; the error tells you to retry
  with `vmobj copy`.

CI covers all of this. The engine tests check:

- same key: incremental transfer, byte-identical blocks, and a second run
  that writes nothing;
- different keys: no source names reused, every restore identical, deep
  verify passes;
- a locked destination: a profile is required, and the locks are long
  enough;
- skipping expired backups;
- refusing mismatched block sizes, a self-copy, and archived source data;
- whole-VM records.

The e2e scripts copy local-to-local, S3-to-local and `backup-vm --copy-to`,
and restore from every copy.

## On the QNAP: one backup, two places

This uses the repository and key from milestones 4 and 5, plus the
QuObjects bucket from milestone 1. The `vmtest` credentials must be in
`~/.aws/credentials` on the QNAP for the user you SSH in as; copy the file
over from WSL with `scp` if needed.

### 1. Copy the new binary over (from WSL)

```bash
cd ~/vm-objects && git fetch && git checkout m6-copy && make build-all
```

```bash
scp bin/vmobj-linux-amd64 admin@QNAP_ADDRESS:/share/VMs/vmobj
```

### 2. Make a second repository on QuObjects, with the same key

```bash
export S3_OPTS='endpoint=http://127.0.0.1:PORT&profile=vmtest'
```

```bash
/share/VMs/vmobj init --repo "s3://vmobj-test/qnap-copy?$S3_OPTS" --key-file /share/VMs/vmobj-test.key
```

### 3. Copy what's already in the local repository

```bash
/share/VMs/vmobj copy --from /share/VMs/vmobj-repo --to "s3://vmobj-test/qnap-copy?$S3_OPTS" --key-file /share/VMs/vmobj-test.key --profile standard
```

Expect a "Copied:" line covering every backup and VM record from the
earlier milestones. Run the same command again: it should say
`Copied: 0 backups`, and finish much faster.

### 4. A new whole-VM backup that lands in both places

With WinGames shut down:

```bash
/share/VMs/vmobj backup-vm --repo /share/VMs/vmobj-repo --key-file /share/VMs/vmobj-test.key --domain DOMAIN_NAME_OR_UUID --vm WinGames --type full --profile standard --virsh /QVS/usr/bin/virsh --swtpm-dir /QVS/var/lib/libvirt/swtpm --state-dir /share/VMs/vmobj-state --copy-to "s3://vmobj-test/qnap-copy?$S3_OPTS"
```

Expect "Copied to: s3://…" at the end. Since this full follows the one from
milestone 5, it should write little: only the blocks that changed.

### 5. Restore from the copy

```bash
/share/VMs/vmobj vms --repo "s3://vmobj-test/qnap-copy?$S3_OPTS" --key-file /share/VMs/vmobj-test.key
```

```bash
/share/VMs/vmobj restore-vm --repo "s3://vmobj-test/qnap-copy?$S3_OPTS" --key-file /share/VMs/vmobj-test.key --vm WinGames --record NEWEST_RECORD --to /share/VMs/wingames-from-copy
```

Then compare the restore with the VM's own files, as in milestone 5:

```bash
cmp /QVS/var/lib/libvirt/swtpm/7240004c-9f00-4aca-8577-8987c8073490/tpm2/tpm2-00.permall /share/VMs/wingames-from-copy/tpm/tpm2/tpm2-00.permall && echo TPM IDENTICAL
```

```bash
qemu-img compare PATH_TO_THE_VMS_DISK.qcow2 /share/VMs/wingames-from-copy/WinGames-hda.raw
```

### 6. Optional: a copy to real AWS

Repeat step 2 with the AWS bucket from milestone 1, then step 3 against it.
Use `--profile standard` unless you want to pay Deep Archive's minimums on
test data.

### 7. Clean up

```bash
rm -rf /share/VMs/wingames-from-copy
```

Delete the `qnap-copy` prefix in QuObjects when you're done.

## What to report back

- Step 3: the "Copied:" lines from both runs, and roughly how long each took.
- Step 4: the "Written:" and "Copied to:" lines.
- Step 5: `TPM IDENTICAL` and the `qemu-img compare` result.
