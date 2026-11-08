// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Command vmobj backs up virtual machine disks block by block.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wilminator/vm-objects/internal/engine"
	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/measure"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/scan"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/units"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `vmobj - block-level backups of virtual machine disks

Usage:
  vmobj key init    --key-file PATH
  vmobj init        --repo REPO --key-file PATH [--block-size 4MiB] [--extent-size 64KiB] [--object-lock]
  vmobj measure     --source DISK [--name NAME] [--state-dir DIR]
  vmobj backup      --repo REPO --key-file PATH --source DISK --vm NAME --type full|incremental|differential
                    [--profile NAME] [--class CLASS] [--copy-to REPO ...]
  vmobj list        --repo REPO --key-file PATH [--vm NAME] [--long]
  vmobj restore     --repo REPO --key-file PATH --vm NAME --manifest NAME --output PATH [--force]
                    [--mode rename|copy|in-place|staged] [--scratch DIR] [--journal PATH]
                    [--check [--check-written]] [--expect-sha256 HEX] [--base NAME]
                    [--request-restore [--tier bulk|standard|expedited] [--days N]]
  vmobj backup-vm   --repo REPO --key-file PATH --domain NAME --type full|incremental|differential
                    [--vm NAME] [--profile NAME] [--quiesce] [--platform NAME] [--virsh PATH] [--connect URI]
                    [--libvirt-socket PATH] [--swtpm-dir DIR] [--definition FILE]
                    [--copy-to REPO ...]
  vmobj vms         --repo REPO --key-file PATH [--vm NAME]
  vmobj restore-vm  --repo REPO --key-file PATH --vm NAME --record NAME --to DIR [--new-name NAME]
                    [--request-restore [--tier bulk|standard|expedited] [--days N]]
  vmobj copy        --from REPO --to REPO --key-file PATH [--to-key-file PATH] [--vm NAME]
                    [--manifest NAME | --record NAME] [--profile NAME] [--class CLASS]
  vmobj prune       --repo REPO --key-file PATH [--apply] [--keep-last 1] [--grace 48h]
  vmobj profiles
  vmobj verify      --repo REPO --key-file PATH --vm NAME --manifest NAME [--deep]
  vmobj install     [--name vmobj] DIR
  vmobj platforms   (the platform modules and the libvirt settings each uses)
  vmobj version
  vmobj help [progress]

measure, backup, backup-vm, restore, restore-vm and copy also take --progress,
--progress-format and related options; see "vmobj help progress".

REPO is a folder path, or an S3 bucket:
  s3://BUCKET[/PREFIX][?region=R][&endpoint=URL][&profile=P][&path-style=true|false]
  endpoint= points at an S3-compatible service such as QNAP QuObjects or MinIO.
  Credentials come from the AWS environment variables or ~/.aws/credentials
  (choose a section with profile=), never from the address.

DISK is a disk image file (raw or qcow2, with its backing chain), a block
device, or zfs:POOL/ZVOL to snapshot a zvol and back up the snapshot. Files
named *.qcow2 are read as qcow2 and anything else as raw; --format raw|qcow2
overrides that. vmobj never decides the format from a disk's contents.

backup-vm backs up a whole libvirt VM: every disk, its definition, its UEFI
NVRAM and its TPM state. A running VM's zvol disks are snapshotted together
(--quiesce freezes the guest's filesystems through the guest agent first);
a running VM with disk image files is refused, so shut it down first.
restore-vm writes the disks, an adjusted definition and the NVRAM/TPM files
into a folder, with instructions; it never changes libvirt itself.

install copies this vmobj binary into a folder, for example out of the
container image: docker run --rm --user "$(id -u):$(id -g)" -v "$PWD":/out
ghcr.io/wilminator/vmobj install /out

copy moves backups between repositories, sending only what the destination
lacks; run it again any time. The destination applies its own --profile
(retention and class, counted from when the backup was made) and its own
Object Lock. --copy-to on backup and backup-vm copies the new backup to
more repositories straight away, so one read of the disk feeds them all.

Profiles pick a storage class and retention for each kind of backup (see
"vmobj profiles"). The default is disaster-recovery. Self-hosted S3 services
usually support only Standard storage: use --profile standard with them.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// The first Ctrl-C (or SIGTERM) cancels the context: work stops cleanly
	// at the next block, and no manifest is written for an unfinished run.
	// A second one quits at once, as if vmobj had not caught the first.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Fprintln(os.Stderr, "\nStopping after the current block; press Ctrl+C again to quit now.")
		stop()
		signal.Reset(os.Interrupt, syscall.SIGTERM)
	}()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "key":
		err = cmdKey(args)
	case "init":
		err = cmdInit(ctx, args)
	case "measure":
		err = cmdMeasure(ctx, args)
	case "backup":
		err = cmdBackup(ctx, args)
	case "list":
		err = cmdList(ctx, args)
	case "restore":
		err = cmdRestore(ctx, args)
	case "verify":
		err = cmdVerify(ctx, args)
	case "profiles":
		err = cmdProfiles()
	case "prune":
		err = cmdPrune(ctx, args)
	case "backup-vm":
		err = cmdBackupVM(ctx, args)
	case "restore-vm":
		err = cmdRestoreVM(ctx, args)
	case "vms":
		err = cmdVMs(ctx, args)
	case "copy":
		err = cmdCopy(ctx, args)
	case "install":
		err = cmdInstall(args)
	case "platforms":
		err = cmdPlatforms()
	case "version":
		fmt.Println("vmobj", version)
	case "help", "-h", "--help":
		if len(args) > 0 && args[0] == "progress" {
			os.Stdout.WriteString(progressHelp)
		} else {
			fmt.Print(usage)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	// Report the timings even when the command failed: a slow or stuck
	// stage is often why.
	runMetrics.Report(os.Stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "vmobj: stopped before finishing. Nothing was recorded for the unfinished run; any pieces it already stored are reused next time or removed by prune.")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "vmobj:", err)
		os.Exit(1)
	}
}

// ---- shared helpers ----

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "vmobj")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".vmobj-state"
	}
	return filepath.Join(home, ".local", "state", "vmobj")
}

func require(fs *flag.FlagSet, names ...string) error {
	for _, n := range names {
		if fs.Lookup(n).Value.String() == "" {
			return fmt.Errorf("%s: --%s is required", fs.Name(), n)
		}
	}
	return nil
}

func openRepo(ctx context.Context, dir, keyFile string) (*repo.Repo, error) {
	k, err := keys.Load(keyFile)
	if err != nil {
		return nil, err
	}
	b, err := store.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	r, err := repo.Open(ctx, b, k)
	if r != nil {
		r.Metrics = runMetrics
	}
	return r, err
}

// openSource opens --source: zfs:POOL/ZVOL snapshots a zvol, anything
// else is a file or device in the given (or name-derived) format.
func openSource(ctx context.Context, spec, format string, keepSnapshot bool) (source.Source, error) {
	if ds, ok := strings.CutPrefix(spec, "zfs:"); ok {
		if format != "" && format != "raw" {
			return nil, errors.New("--format does not apply to zfs: sources (zvols are raw)")
		}
		return source.OpenZFS(ctx, ds, source.ZFSOptions{KeepSnapshot: keepSnapshot})
	}
	return source.Open(spec, format)
}

// copyUploads is --uploads: how many blocks a copy sends at once.
var copyUploads = engine.DefaultUploads

func addUploadsFlag(fs *flag.FlagSet) {
	fs.IntVar(&copyUploads, "uploads", engine.DefaultUploads, "copies: blocks sent at once (1-64); more helps over the internet, where each upload waits on the network")
}

// ---- commands ----

func cmdKey(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New("usage: vmobj key init --key-file PATH")
	}
	fs := flag.NewFlagSet("key init", flag.ExitOnError)
	keyFile := fs.String("key-file", "", "where to create the key file")
	fs.Parse(args[1:])
	if err := require(fs, "key-file"); err != nil {
		return err
	}
	if err := keys.Generate(*keyFile); err != nil {
		return err
	}
	fmt.Printf("Created %s (mode 400).\n", *keyFile)
	fmt.Println("Back it up somewhere safe and offline: without it, no backup made with it can be restored.")
	return nil
}

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	blockSize := fs.String("block-size", "4MiB", "base block size")
	extentSize := fs.String("extent-size", "64KiB", "incremental extent size")
	packSize := fs.String("pack-size", "128MiB", "target pack size")
	objectLock := fs.Bool("object-lock", false, "lock every object (S3 bucket created with Object Lock)")
	fs.Parse(args)
	if err := require(fs, "repo", "key-file"); err != nil {
		return err
	}
	k, err := keys.Load(*keyFile)
	if err != nil {
		return err
	}
	cfg := repo.DefaultConfig()
	cfg.ObjectLock = *objectLock
	for _, s := range []struct {
		in  string
		out *int64
	}{{*blockSize, (*int64)(&cfg.BlockSize)}, {*extentSize, (*int64)(&cfg.ExtentSize)}, {*packSize, &cfg.PackTarget}} {
		v, err := units.ParseSize(s.in)
		if err != nil {
			return err
		}
		*s.out = v
	}
	b, err := store.Open(ctx, *dir)
	if err != nil {
		return err
	}
	r, err := repo.Init(ctx, b, k, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("Created repository %s in %s (blocks %s, extents %s).\n",
		r.Config.RepoID, *dir, units.Format(int64(cfg.BlockSize)), units.Format(int64(cfg.ExtentSize)))
	if cfg.ObjectLock {
		fmt.Println("Object Lock is on: every backup is locked in Governance mode for its retention period.")
	}
	return nil
}

func cmdMeasure(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("measure", flag.ExitOnError)
	src := fs.String("source", "", "raw disk image or block device")
	name := fs.String("name", "", "name for the saved results (default: the source file name)")
	stateDir := fs.String("state-dir", "", "where results are kept between runs")
	blockSize := fs.String("block-size", "4MiB", "large block size to report")
	extentSize := fs.String("extent-size", "64KiB", "small extent size to report")
	workers := fs.Int("workers", 0, "blocks hashed at once (default: one per CPU)")
	format := fs.String("format", "", "disk format: raw or qcow2 (default: from the file name)")
	pf := addProgressFlags(fs)
	metricsOn := addMetricsFlag(fs)
	fs.Parse(args)
	startMetrics(*metricsOn)
	pc, perr := pf.config()
	if perr != nil {
		return perr
	}
	if err := require(fs, "source"); err != nil {
		return err
	}
	bs, err := units.ParseSize(*blockSize)
	if err != nil {
		return err
	}
	es, err := units.ParseSize(*extentSize)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(filepath.Base(strings.TrimPrefix(*src, "zfs:")))
	}
	if err := manifest.CheckVMName(*name); err != nil {
		return fmt.Errorf("--name: %w", err)
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(defaultStateDir(), "measure")
	}
	path := filepath.Join(*stateDir, *name+".hashes")

	disk, err := openSource(ctx, *src, *format, false)
	if err != nil {
		return err
	}
	defer disk.Close()
	p, stopProgress := withProgress(pc, *src, disk)
	fmt.Fprintf(os.Stderr, "Measuring %s (%s)...\n", *src, units.Format(disk.Size()))
	start := time.Now()
	cur, err := measure.Run(ctx, p, scan.Options{BlockSize: extent.Length(bs), ExtentSize: extent.Length(es), Workers: *workers, Metrics: runMetrics})
	stopProgress()
	if err != nil {
		return err
	}
	took := time.Since(start).Round(time.Second)

	s := cur.Summarise()
	fmt.Printf("Disk:            %s, read in %s\n", units.Format(cur.DiskSize), took)
	fmt.Printf("Data (non-zero): %s in %d of %d extents of %s\n",
		units.Format(s.DataBytes), s.Extents-s.ZeroExtents, s.Extents, units.Format(es))

	prev, err := measure.Load(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Println("First run for this disk; run again later to see what changed.")
	case err != nil:
		return err
	default:
		ch, err := measure.Compare(prev, cur)
		if err != nil {
			fmt.Printf("Not compared with the previous run: %v\n", err)
		} else {
			fmt.Printf("Changed since %s (%s ago):\n", ch.Since.Local().Format("2006-01-02 15:04"), time.Since(ch.Since).Round(time.Minute))
			fmt.Printf("  at %-6s %8d extents  %s\n", units.Format(es), ch.ChangedExtents, units.Format(ch.ChangedExtentBytes))
			fmt.Printf("  at %-6s %8d blocks   %s\n", units.Format(bs), ch.ChangedBlocks, units.Format(ch.ChangedBlockBytes))
		}
		if err := os.Rename(path, path+".prev"); err != nil {
			return err
		}
	}
	return cur.Save(path)
}

func cmdBackup(ctx context.Context, args []string) (err error) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	src := fs.String("source", "", "raw disk image or block device (use a snapshot)")
	vm := fs.String("vm", "", "VM name")
	kind := fs.String("type", "", "full, incremental or differential")
	stateDir := fs.String("state-dir", "", "where this host keeps per-VM state")
	workers := fs.Int("workers", 0, "blocks hashed at once (default: one per CPU)")
	profileName := fs.String("profile", policy.DefaultProfile, "storage profile (see vmobj profiles)")
	classFlag := fs.String("class", "", "storage class for this run's data, overriding the profile")
	format := fs.String("format", "", "disk format: raw or qcow2 (default: from the file name)")
	keepSnap := fs.Bool("keep-snapshot", false, "with zfs:, keep the snapshot the backup was read from")
	var copyTo repeated
	fs.Var(&copyTo, "copy-to", "also copy the new backup to this repository (repeatable; same key file)")
	addUploadsFlag(fs)
	pf := addProgressFlags(fs)
	metricsOn := addMetricsFlag(fs)
	fs.Parse(args)
	startMetrics(*metricsOn)
	pc, perr := pf.config()
	if perr != nil {
		return perr
	}
	if err := require(fs, "repo", "key-file", "source", "vm", "type"); err != nil {
		return err
	}
	k, err := manifest.ParseKind(*kind)
	if err != nil {
		return err
	}
	prof, err := policy.Lookup(*profileName)
	if err != nil {
		return err
	}
	var class policy.Class
	if *classFlag != "" {
		if class, err = policy.ParseClass(*classFlag); err != nil {
			return err
		}
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(defaultStateDir(), r.Config.RepoID)
	}
	disk, err := openSource(ctx, *src, *format, *keepSnap)
	if err != nil {
		return err
	}
	defer disk.Close()
	if z, ok := disk.(*source.ZFS); ok {
		fmt.Fprintf(os.Stderr, "Reading from snapshot %s\n", z.Snapshot())
	}
	p, stopProgress := withProgress(pc, *src, disk)
	fmt.Fprintf(os.Stderr, "%s backup of %s (%s)...\n", k, *vm, units.Format(disk.Size()))
	start := time.Now()
	st, err := engine.Backup(ctx, engine.BackupOptions{
		Repo: r, Source: p, VM: *vm, Kind: k, Workers: *workers,
		StatePath: filepath.Join(*stateDir, *vm+".state"),
		Profile:   &prof, Class: class,
	})
	stopProgress()
	if err != nil {
		return err
	}
	fmt.Printf("Manifest:  %s\n", st.Manifest)
	if m, err := r.GetManifest(ctx, *vm, st.Manifest); err == nil {
		fmt.Printf("Storage:   %s (profile %s), keep until %s\n", className(m.DataClass), m.Profile, m.RetainUntil.Local().Format("2006-01-02"))
	}
	fmt.Printf("Took:      %s\n", time.Since(start).Round(time.Second))
	if k == manifest.Full {
		fmt.Printf("Blocks:    %d stored, %d reused, %d all zeros\n", st.BlocksStored, st.BlocksReused, st.ZeroBlocks)
	} else {
		fmt.Printf("Extents:   %d differ from the full; %d stored, %d reused\n", st.ExtentsChanged, st.ExtentsStored, st.ExtentsReused)
		fmt.Printf("Packs:     %d\n", st.Packs)
	}
	fmt.Printf("Written:   %s (compressed and encrypted)\n", units.Format(st.BytesWritten))
	defer func() {
		if err == nil {
			err = copyAfterBackup(ctx, r, copyTo, *keyFile, &prof, class, func(o engine.CopyOptions, cs *engine.CopyStats) error {
				return engine.CopyManifest(ctx, o, *vm, st.Manifest, cs)
			})
		}
	}()
	if st.LocksExtended > 0 || st.BlocksRefresh > 0 {
		fmt.Printf("Locks:     %d extended, %d old blocks uploaded again\n", st.LocksExtended, st.BlocksRefresh)
	}
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	vm := fs.String("vm", "", "only this VM")
	long := fs.Bool("long", false, "also show each backup's storage class and retention")
	fs.Parse(args)
	if err := require(fs, "repo", "key-file"); err != nil {
		return err
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	vms := []string{*vm}
	if *vm == "" {
		if vms, err = r.ListVMs(ctx); err != nil {
			return err
		}
	}
	for _, v := range vms {
		names, err := r.ListManifests(ctx, v)
		if err != nil {
			return err
		}
		fmt.Printf("%s:\n", v)
		for _, n := range names {
			if !*long {
				fmt.Printf("  %s\n", n)
				continue
			}
			m, err := r.GetManifest(ctx, v, n)
			if err != nil {
				return err
			}
			keep := "-"
			if !m.RetainUntil.IsZero() {
				keep = m.RetainUntil.Local().Format("2006-01-02")
			}
			fmt.Printf("  %-40s %-14s keep until %s  %s\n", n, className(m.DataClass), keep, units.Format(m.DiskSize))
		}
	}
	return nil
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	vm := fs.String("vm", "", "VM name")
	name := fs.String("manifest", "", "manifest to restore (see vmobj list)")
	out := fs.String("output", "", "raw image file or block device to write")
	force := fs.Bool("force", false, "replace an existing output file (rename mode)")
	modeFlag := fs.String("mode", "rename", "rename: write a new file and rename it into place; copy: restore to scratch space, then update the existing target, writing only what differs; in-place: update the existing target directly, fetching and writing only what differs; staged: like in-place, but the changes wait in a journal until the whole result is checked, and only then go into the target")
	journal := fs.String("journal", "", "staged: the journal file (default: next to a file target, as NAME.vmobj-journal; a block device needs one, on another filesystem)")
	scratch := fs.String("scratch", "", "copy mode: folder for the temporary full restore (default: the target's folder)")
	check := fs.Bool("check", false, "copy and in-place: read the target back afterwards and compare it; also prints the whole disk's SHA-256")
	base := fs.String("base", "", "in-place or staged: a backup the target already holds (e.g. the previous step of a replay, just checked); blocks the same in both are skipped without reading the target")
	checkWritten := fs.Bool("check-written", false, "with --check: read back only the blocks this restore wrote (faster; no SHA-256)")
	expectSHA := fs.String("expect-sha256", "", "with a full --check: fail unless the restored disk's SHA-256 is this")
	request := fs.Bool("request-restore", false, "ask S3 to restore archived objects this restore needs")
	tierFlag := fs.String("tier", "bulk", "retrieval speed for --request-restore: bulk, standard or expedited")
	days := fs.Int("days", 7, "days the restored copies stay readable")
	pf := addProgressFlags(fs)
	metricsOn := addMetricsFlag(fs)
	fs.Parse(args)
	startMetrics(*metricsOn)
	pc, perr := pf.config()
	if perr != nil {
		return perr
	}
	if err := require(fs, "repo", "key-file", "vm", "manifest", "output"); err != nil {
		return err
	}
	tier, err := policy.ParseTier(*tierFlag)
	if err != nil {
		return err
	}
	mode, err := engine.ParseMode(*modeFlag)
	if err != nil {
		return err
	}
	if *scratch != "" && mode != engine.ModeCopy {
		return errors.New("restore: --scratch only applies to --mode copy")
	}
	if *check && mode == engine.ModeRename {
		return errors.New("restore: --check works with --mode copy or in-place")
	}
	if *base != "" && mode != engine.ModeInPlace && mode != engine.ModeStaged {
		return errors.New("restore: --base works with --mode in-place or staged")
	}
	if *journal != "" && mode != engine.ModeStaged {
		return errors.New("restore: --journal only applies to --mode staged")
	}
	if mode == engine.ModeStaged && *checkWritten {
		return errors.New("restore: staged mode always checks the whole result; --check-written doesn't apply")
	}
	if *checkWritten && !*check {
		return errors.New("restore: --check-written needs --check")
	}
	if *expectSHA != "" && mode != engine.ModeStaged && (!*check || *checkWritten) {
		return errors.New("restore: --expect-sha256 needs --check (without --check-written)")
	}
	if mode != engine.ModeRename && *force {
		return errors.New("restore: --force only applies to --mode rename; copy and in-place always update the existing target")
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	start := time.Now()
	scratchLabel := ""
	if mode == engine.ModeCopy {
		scratchLabel = "a scratch copy"
	}
	if mode == engine.ModeInPlace {
		fmt.Fprintf(os.Stderr, "In-place restore: %s is changed as the restore runs. If it stops part-way, the target is part old, part new; run the same command again to finish it.\n", *out)
	}
	report, stopReport := restoreProgress(pc, *out, scratchLabel)
	st, err := engine.Restore(ctx, engine.RestoreOptions{Repo: r, VM: *vm, Manifest: *name, Output: *out, Force: *force,
		Mode: mode, Scratch: *scratch, Check: *check, Base: *base, CheckWritten: *checkWritten, ExpectSHA256: *expectSHA, Journal: *journal,
		Progress: func(p engine.Phase, done, total int64) { report("", p, done, total) }})
	stopReport()
	var nr *engine.NotReadyError
	if errors.As(err, &nr) {
		return explainNotReady(ctx, r, nr.Readiness, *request, tier, *days)
	}
	if err != nil {
		return err
	}
	took := time.Since(start).Round(time.Second)
	if mode == engine.ModeRename {
		fmt.Printf("Restored %s to %s in %s: %d blocks, %d newer extents, %d zero blocks.\n",
			units.Format(st.DiskBytes), *out, took, st.BlocksRead, st.ExtentsRead, st.ZeroBlocks)
		return nil
	}
	if mode == engine.ModeStaged {
		printStagedSummary(*out, st, took)
		return nil
	}
	printUpdateSummary(*out, mode, st, took)
	return nil
}

// printStagedSummary reports a staged restore.
func printStagedSummary(out string, st *engine.RestoreStats, took time.Duration) {
	if st.Finished {
		fmt.Printf("Finished an interrupted staged restore of %s (%s) in %s.\n", out, units.Format(st.DiskBytes), took)
		fmt.Printf("  Applied:   %s from the journal; read back, it matches\n", units.Format(st.Applied))
		fmt.Printf("  SHA-256:   %s (checked before applying)\n", st.SHA256)
		return
	}
	fmt.Printf("Updated %s (staged, %s) in %s.\n", out, units.Format(st.DiskBytes), took)
	if st.Discarded {
		fmt.Println("  Note:      an unfinished journal from an earlier run was discarded (it was never applied)")
	}
	if st.BlocksSkipped > 0 {
		fmt.Printf("  Skipped:   %s (%d blocks the same in --base, not compared)\n", units.Format(st.SkippedBytes), st.BlocksSkipped)
	}
	fmt.Printf("  Compared:  %s; %d blocks were already right\n", units.Format(st.Compared), st.BlocksSame)
	fmt.Printf("  Journal:   %s of changes (%s on disk), %d pieces of 4 KiB\n", units.Format(st.JournalBytes), units.Format(st.JournalFile), st.PiecesWritten)
	if st.Repaired > 0 {
		fmt.Printf("  Repaired:  %d blocks didn't hold what --base said; fetched whole before anything was written\n", st.Repaired)
	}
	fmt.Printf("  Checked:   every block of the result, before writing\n")
	fmt.Printf("  SHA-256:   %s\n", st.SHA256)
	fmt.Printf("  Applied:   %s in %d writes; read back, it matches\n", units.Format(st.Applied), st.Writes)
	fmt.Printf("  Fetched:   %d blocks and %d newer extents\n", st.BlocksRead, st.ExtentsRead)
}

// printUpdateSummary reports a copy or in-place restore.
func printUpdateSummary(out string, mode engine.Mode, st *engine.RestoreStats, took time.Duration) {
	fmt.Printf("Updated %s (%s, %s) in %s.\n", out, mode, units.Format(st.DiskBytes), took)
	if st.BlocksSkipped > 0 {
		fmt.Printf("  Skipped:   %s (%d blocks the same in --base, not read)\n", units.Format(st.SkippedBytes), st.BlocksSkipped)
	}
	fmt.Printf("  Compared:  %s; %d blocks were already right\n", units.Format(st.Compared), st.BlocksSame)
	fmt.Printf("  Written:   %s (%d pieces of 4 KiB, in %d writes)\n", units.Format(st.Written), st.PiecesWritten, st.Writes)
	if mode == engine.ModeInPlace {
		fmt.Printf("  Fetched:   %d blocks and %d newer extents (%d newer extents were already in place)\n", st.BlocksRead, st.ExtentsRead, st.ExtentsSame)
	}
	if st.Resized {
		fmt.Printf("  Resized:   the file was %s; now %s\n", units.Format(st.ResizedFrom), units.Format(st.DiskBytes))
	}
	if st.TargetExtra > 0 {
		fmt.Printf("  Note:      the device is %s larger than the disk; that part was left as it was\n", units.Format(st.TargetExtra))
	}
	if st.Checked {
		if st.SHA256 != "" {
			fmt.Println("  Checked:   read back, every block matches")
			fmt.Printf("  SHA-256:   %s\n", st.SHA256)
		} else {
			fmt.Printf("  Checked:   read back the %s written, every block matches\n", units.Format(st.CheckedBytes))
		}
	}
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	vm := fs.String("vm", "", "VM name")
	name := fs.String("manifest", "", "manifest to check")
	deep := fs.Bool("deep", false, "download and check every piece")
	metricsOn := addMetricsFlag(fs)
	fs.Parse(args)
	startMetrics(*metricsOn)
	if err := require(fs, "repo", "key-file", "vm", "manifest"); err != nil {
		return err
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	vs, err := engine.Verify(ctx, r, *vm, *name, *deep)
	if err != nil {
		return err
	}
	how := "present"
	if vs.Deep {
		how = "present and intact"
	}
	if vs.Archived > 0 {
		fmt.Printf("Note: %d pieces are archived and were checked for presence only; restore them to check contents.\n", vs.Archived)
	}
	if len(vs.Missing) == 0 {
		fmt.Printf("OK: %d blocks and %d extents are %s.\n", vs.Blocks, vs.Extents, how)
		return nil
	}
	for _, m := range vs.Missing {
		fmt.Println("  problem:", m)
	}
	return fmt.Errorf("%d problems found", len(vs.Missing))
}

// className shows a storage class in friendly form.
func className(c string) string {
	if c == "" {
		return "default class"
	}
	if ci, ok := policy.Info(policy.Class(c)); ok {
		return ci.Name
	}
	return c
}

// explainNotReady reports what a restore is waiting for and, if asked,
// requests the restores. Nothing has been written to the output.
func explainNotReady(ctx context.Context, r *repo.Repo, rd *engine.Readiness, request bool, tier policy.Tier, days int) error {
	fmt.Printf("This backup is in archive storage. Nothing was written.\n")
	fmt.Printf("  objects needed:        %d\n", rd.Objects)
	fmt.Printf("  readable now:          %d\n", rd.Ready)
	fmt.Printf("  restore in progress:   %d\n", rd.Restoring)
	fmt.Printf("  not yet requested:     %d (%s)\n", len(rd.Pending), units.Format(rd.PendingBytes))
	for _, t := range []policy.Tier{policy.Bulk, policy.StandardT, policy.Expedited} {
		fmt.Printf("  wait at %-10s     %s\n", strings.ToLower(string(t))+":", rd.Wait(t))
	}
	if len(rd.Pending) == 0 {
		fmt.Println("All needed restores are requested; run this command again once they finish.")
		return errors.New("waiting for archive restores")
	}
	if !request {
		fmt.Println("Run again with --request-restore (and optionally --tier, --days) to start the restores.")
		fmt.Println("Restores are billed per GB and per request; bulk is the cheapest tier.")
		return errors.New("archived objects need a restore request")
	}
	n, err := engine.RequestRestores(ctx, r, rd, days, tier)
	if err != nil {
		return err
	}
	fmt.Printf("Requested %d restores at %s tier, readable for %d days once done (%s).\n",
		n, strings.ToLower(string(tier)), days, rd.Wait(tier))
	fmt.Println("Run the same restore command again later; it starts as soon as everything is readable.")
	return nil
}

func cmdProfiles() error {
	for _, name := range policy.Names() {
		p, _ := policy.Lookup(name)
		def := ""
		if name == policy.DefaultProfile {
			def = " (default)"
		}
		fmt.Printf("%s%s\n  %s\n", name, def, p.Description)
		for _, k := range []manifest.Kind{manifest.Full, manifest.Differential, manifest.Incremental} {
			rule := p.Rule(k)
			ci, _ := policy.Info(rule.Class)
			fmt.Printf("  %-13s %-32s keep %3d days, about every %d days\n",
				k, ci.Name, int(rule.Retain/policy.Day), int(rule.Every/policy.Day))
		}
		fmt.Println()
	}
	return nil
}

func cmdPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://...")
	keyFile := fs.String("key-file", "", "key file")
	apply := fs.Bool("apply", false, "delete for real (without it, only report)")
	keepLast := fs.Int("keep-last", 1, "always keep this many newest backups of each VM")
	grace := fs.Duration("grace", engine.DefaultGrace, "never delete objects written more recently than this")
	fs.Parse(args)
	if err := require(fs, "repo", "key-file"); err != nil {
		return err
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	rep, err := engine.Prune(ctx, engine.PruneOptions{Repo: r, Now: time.Now(), Apply: *apply, KeepLast: *keepLast, Grace: *grace})
	if err != nil {
		return err
	}
	verb := "Would delete"
	if rep.Applied {
		verb = "Deleted"
	}
	fmt.Printf("Backups examined:     %d\n", rep.Manifests)
	fmt.Printf("Past keep-until:      %d\n", len(rep.ExpiredManifests))
	for _, m := range rep.ExpiredManifests {
		fmt.Printf("  %s\n", m)
	}
	for _, m := range rep.KeptExpired {
		fmt.Printf("  kept (newest of its VM): %s\n", m)
	}
	fmt.Printf("Unreferenced objects: %d (%s)\n", rep.Unreferenced, units.Format(rep.UnreferencedSize))
	if rep.OldVersions > 0 {
		fmt.Printf("Superseded versions:  %d\n", rep.OldVersions)
	}
	if rep.Recent > 0 {
		fmt.Printf("Too recent to touch:  %d (written within %s)\n", rep.Recent, *grace)
	}
	if len(rep.Locked) > 0 {
		var latest time.Time
		for _, t := range rep.Locked {
			if t.After(latest) {
				latest = t
			}
		}
		fmt.Printf("Still locked:         %d (all unlocked by %s; a later prune removes them)\n", len(rep.Locked), latest.Local().Format("2006-01-02"))
	}
	if rep.Applied {
		fmt.Printf("%s:              %d object versions (%s)\n", verb, rep.Deleted, units.Format(rep.DeletedSize))
	} else {
		fmt.Println("Nothing was deleted. Run again with --apply to delete.")
	}
	return nil
}

// newPlatform builds the platform module for --platform, with any
// --virsh, --connect and --swtpm-dir overrides.
func newPlatform(name, virshPath, connect, socket, swtpmDir string) (platform.Platform, error) {
	return platform.New(name, platform.LibvirtConfig{Virsh: virshPath, URI: connect, Socket: socket, SwtpmDir: swtpmDir})
}

func cmdBackupVM(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup-vm", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://...")
	keyFile := fs.String("key-file", "", "key file")
	domain := fs.String("domain", "", "libvirt domain name or UUID")
	vm := fs.String("vm", "", "name in the repository (default: the domain's name)")
	kind := fs.String("type", "", "full, incremental or differential")
	profileName := fs.String("profile", policy.DefaultProfile, "storage profile (see vmobj profiles)")
	classFlag := fs.String("class", "", "storage class for this run's data, overriding the profile")
	stateDir := fs.String("state-dir", "", "where this host keeps per-disk state")
	workers := fs.Int("workers", 0, "blocks hashed at once (default: one per CPU)")
	quiesce := fs.Bool("quiesce", false, "freeze the guest's filesystems (QEMU guest agent) while a running VM is snapshotted")
	platformName := fs.String("platform", "auto", "what runs the VM: auto (detect), libvirt, qnap or truenas")
	swtpmDir := fs.String("swtpm-dir", "", "where libvirt keeps TPM state (default: the platform's; see vmobj platforms)")
	virshPath := fs.String("virsh", "", "virsh program (default: the platform's; see vmobj platforms)")
	connect := fs.String("connect", "", "libvirt URI, e.g. qemu:///system (default: the platform's)")
	defFile := fs.String("definition", "", "read the VM's libvirt definition from this file instead of from libvirt, for a shut-off VM the platform defines only while it runs (QNAP Virtualization Station: <disk folder>/.<UUID>.meta/<UUID>.xml)")
	socket := fs.String("libvirt-socket", "", "libvirt's socket, when it isn't the default one (TrueNAS: "+platform.TrueNASLibvirtSocket+"); --connect wins")
	var copyTo repeated
	fs.Var(&copyTo, "copy-to", "also copy the new backup to this repository (repeatable; same key file)")
	addUploadsFlag(fs)
	pf := addProgressFlags(fs)
	metricsOn := addMetricsFlag(fs)
	fs.Parse(args)
	startMetrics(*metricsOn)
	pc, perr := pf.config()
	if perr != nil {
		return perr
	}
	if err := require(fs, "repo", "key-file", "type"); err != nil {
		return err
	}
	if *domain == "" && *defFile == "" {
		return errors.New("backup-vm: give --domain, or --definition for a VM libvirt doesn't hold while it's off")
	}
	k, err := manifest.ParseKind(*kind)
	if err != nil {
		return err
	}
	plat, err := newPlatform(*platformName, *virshPath, *connect, *socket, *swtpmDir)
	if err != nil {
		return err
	}
	if *defFile != "" {
		lv, ok := plat.(*platform.Libvirt)
		if !ok {
			return fmt.Errorf("backup-vm: --definition works with the libvirt and qnap platforms, not %s", plat.Name())
		}
		if *quiesce {
			return errors.New("backup-vm: --quiesce needs a running VM; --definition is for a shut-off one")
		}
		lv.DefinitionFile = *defFile
	}
	prof, err := policy.Lookup(*profileName)
	if err != nil {
		return err
	}
	var class policy.Class
	if *classFlag != "" {
		if class, err = policy.ParseClass(*classFlag); err != nil {
			return err
		}
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(defaultStateDir(), r.Config.RepoID)
	}
	start := time.Now()
	st, err := engine.BackupVM(ctx, engine.BackupVMOptions{
		Repo: r, Domain: *domain, VM: *vm, Kind: k, Profile: &prof, Class: class, Workers: *workers,
		StateDir: *stateDir, Quiesce: *quiesce, Platform: plat,
		Progress: func(target string, s source.Source) (source.Source, func()) {
			fmt.Fprintf(os.Stderr, "Disk %s (%s)...\n", target, units.Format(s.Size()))
			return withProgress(pc, "disk "+target, s)
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("VM record: %s (%s)\n", st.Record, st.Consistency)
	var targets []string
	for t := range st.Disks {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		d := st.Disks[t]
		fmt.Printf("  %-6s %s, %s written\n", t, d.Manifest, units.Format(d.BytesWritten))
	}
	files := describeVMFiles(st.NVRAMFiles, st.TPMFiles)
	for _, c := range st.ConfigFiles {
		files += ", settings " + c
	}
	fmt.Printf("Files:     %s (inside the record)\n", files)
	for _, s := range st.Skipped {
		fmt.Printf("Skipped:   %s\n", s)
	}
	fmt.Printf("Took:      %s\n", time.Since(start).Round(time.Second))
	return copyAfterBackup(ctx, r, copyTo, *keyFile, &prof, class, func(o engine.CopyOptions, cs *engine.CopyStats) error {
		vmName := *vm
		if vmName == "" {
			rec, err := findRecord(ctx, r, st.Record)
			if err != nil {
				return err
			}
			vmName = rec
		}
		return engine.CopyRecord(ctx, o, vmName, st.Record, cs)
	})
}

// findRecord finds which VM a just-written record belongs to.
func findRecord(ctx context.Context, r *repo.Repo, record string) (string, error) {
	vms, err := r.ListRecordVMs(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range vms {
		if ok, _ := r.Backend.Exists(ctx, repo.VMRecordKey(v, record)); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("VM record %s not found", record)
}

func cmdVMs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("vms", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://...")
	keyFile := fs.String("key-file", "", "key file")
	vm := fs.String("vm", "", "only this VM")
	fs.Parse(args)
	if err := require(fs, "repo", "key-file"); err != nil {
		return err
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	vms := []string{*vm}
	if *vm == "" {
		if vms, err = r.ListRecordVMs(ctx); err != nil {
			return err
		}
	}
	for _, v := range vms {
		names, err := r.ListVMRecords(ctx, v)
		if err != nil {
			return err
		}
		fmt.Printf("%s:\n", v)
		for _, n := range names {
			rec, err := r.GetVMRecord(ctx, v, n)
			if err != nil {
				return err
			}
			var disks []string
			for _, d := range rec.Disks {
				disks = append(disks, d.Target)
			}
			keep := "-"
			if !rec.RetainUntil.IsZero() {
				keep = rec.RetainUntil.Local().Format("2006-01-02")
			}
			fmt.Printf("  %-36s %-18s disks %-12s keep until %s\n", n, rec.Consistency, strings.Join(disks, ","), keep)
		}
	}
	return nil
}

func cmdRestoreVM(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore-vm", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://...")
	keyFile := fs.String("key-file", "", "key file")
	vm := fs.String("vm", "", "VM name (see vmobj vms)")
	record := fs.String("record", "", "VM record to restore (see vmobj vms)")
	to := fs.String("to", "", "folder to restore into (created; must be empty)")
	newName := fs.String("new-name", "", "rename the VM and drop its UUID, to run it next to the original")
	targetFlag := fs.String("platform", "auto", "where the VM will run next, for RESTORE.txt: auto (this host), libvirt, qnap or truenas")
	request := fs.Bool("request-restore", false, "ask S3 to restore archived objects this restore needs")
	tierFlag := fs.String("tier", "bulk", "retrieval speed for --request-restore: bulk, standard or expedited")
	days := fs.Int("days", 7, "days the restored copies stay readable")
	pf := addProgressFlags(fs)
	metricsOn := addMetricsFlag(fs)
	fs.Parse(args)
	target, err := platform.Check(*targetFlag)
	if err != nil {
		return err
	}
	startMetrics(*metricsOn)
	pc, perr := pf.config()
	if perr != nil {
		return perr
	}
	if err := require(fs, "repo", "key-file", "vm", "record", "to"); err != nil {
		return err
	}
	tier, err := policy.ParseTier(*tierFlag)
	if err != nil {
		return err
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	start := time.Now()
	report, stopReport := restoreProgress(pc, *to, "")
	st, err := engine.RestoreVM(ctx, engine.RestoreVMOptions{Target: target, Repo: r, VM: *vm, Record: *record, Dir: *to, NewName: *newName, Progress: report})
	stopReport()
	var nr *engine.NotReadyError
	if errors.As(err, &nr) {
		return explainNotReady(ctx, r, nr.Readiness, *request, tier, *days)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Restored %s (%s) into %s in %s.\n", st.Record.DomainName, st.Record.Name, *to, time.Since(start).Round(time.Second))
	fmt.Printf("Definition: %s\n", st.Definition)
	fmt.Println("Nothing was registered with libvirt. Read RESTORE.txt in that folder for the next steps.")
	return nil
}

// repeated is a flag that may be given several times.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// copyAfterBackup copies a just-written backup to each --copy-to target.
// The backup itself has already succeeded; a failed copy is reported as
// an error so scripts notice, and can be retried with "vmobj copy".
func copyAfterBackup(ctx context.Context, from *repo.Repo, targets []string, keyFile string, prof *policy.Profile, class policy.Class,
	copyOne func(engine.CopyOptions, *engine.CopyStats) error) error {
	var failed []string
	for _, t := range targets {
		to, err := openRepo(ctx, t, keyFile)
		if err == nil {
			cs := &engine.CopyStats{}
			if err = copyOne(engine.CopyOptions{From: from, To: to, Profile: prof, Class: class, Uploads: copyUploads}, cs); err == nil {
				fmt.Printf("Copied to: %s (%s written)\n", t, units.Format(cs.BytesWritten))
				continue
			}
		}
		fmt.Fprintf(os.Stderr, "Copy to %s failed: %v\n", t, err)
		failed = append(failed, t)
	}
	if len(failed) > 0 {
		return fmt.Errorf("the backup succeeded, but copying it to %s failed; retry with vmobj copy", strings.Join(failed, ", "))
	}
	return nil
}

func cmdCopy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("copy", flag.ExitOnError)
	from := fs.String("from", "", "source repository")
	to := fs.String("to", "", "destination repository")
	keyFile := fs.String("key-file", "", "key file for the source (and the destination, unless --to-key-file)")
	toKeyFile := fs.String("to-key-file", "", "key file for the destination, if different")
	vm := fs.String("vm", "", "only this VM (with --manifest: the disk series, e.g. win11.vda)")
	name := fs.String("manifest", "", "copy only this backup of --vm")
	record := fs.String("record", "", "copy only this whole-VM backup of --vm")
	profileName := fs.String("profile", policy.DefaultProfile, "storage profile at the destination")
	classFlag := fs.String("class", "", "storage class at the destination, overriding the profile")
	addUploadsFlag(fs)
	pf := addProgressFlags(fs)
	metricsOn := addMetricsFlag(fs)
	fs.Parse(args)
	startMetrics(*metricsOn)
	pc, perr := pf.config()
	if perr != nil {
		return perr
	}
	if err := require(fs, "from", "to", "key-file"); err != nil {
		return err
	}
	if (*name != "" || *record != "") && *vm == "" {
		return errors.New("copy: --manifest and --record need --vm")
	}
	if *name != "" && *record != "" {
		return errors.New("copy: give --manifest or --record, not both")
	}
	prof, err := policy.Lookup(*profileName)
	if err != nil {
		return err
	}
	var class policy.Class
	if *classFlag != "" {
		if class, err = policy.ParseClass(*classFlag); err != nil {
			return err
		}
	}
	src, err := openRepo(ctx, *from, *keyFile)
	if err != nil {
		return err
	}
	if *toKeyFile == "" {
		*toKeyFile = *keyFile
	}
	dst, err := openRepo(ctx, *to, *toKeyFile)
	if err != nil {
		return err
	}
	report, stopReport := copyProgress(pc)
	defer stopReport()
	o := engine.CopyOptions{From: src, To: dst, Profile: &prof, Class: class, Progress: report, Uploads: copyUploads}
	start := time.Now()
	st := &engine.CopyStats{}
	switch {
	case *name != "":
		err = engine.CopyManifest(ctx, o, *vm, *name, st)
	case *record != "":
		err = engine.CopyRecord(ctx, o, *vm, *record, st)
	default:
		st, err = engine.CopyAll(ctx, o, *vm)
	}
	stopReport()
	var nr *engine.NotReadyError
	if errors.As(err, &nr) {
		fmt.Println("The source backup is in archive storage; restore it there first (vmobj restore --request-restore).")
		return err
	}
	if err != nil {
		return err
	}
	fmt.Printf("Copied:    %d backups, %d VM records in %s\n", st.Manifests, st.Records, time.Since(start).Round(time.Second))
	fmt.Printf("Blocks:    %d copied, %d already there\n", st.BlocksCopied, st.BlocksPresent)
	fmt.Printf("Extents:   %d copied, %d already there\n", st.ExtentsCopied, st.ExtentsPresent)
	fmt.Printf("Written:   %s\n", units.Format(st.BytesWritten))
	if st.LocksExtended > 0 {
		fmt.Printf("Locks:     %d extended\n", st.LocksExtended)
	}
	for _, s := range st.Skipped {
		fmt.Printf("Skipped:   %s\n", s)
	}
	return nil
}

// describeVMFiles names the files a whole-VM backup kept, for example
// "TPM state (2 files), no NVRAM".
func describeVMFiles(nvram, tpm int) string {
	plural := func(n int) string {
		if n == 1 {
			return "1 file"
		}
		return fmt.Sprintf("%d files", n)
	}
	var parts []string
	if nvram > 0 {
		parts = append(parts, "NVRAM ("+plural(nvram)+")")
	}
	if tpm > 0 {
		parts = append(parts, "TPM state ("+plural(tpm)+")")
	}
	switch {
	case nvram == 0 && tpm == 0:
		return "none (no NVRAM or TPM state)"
	case nvram == 0:
		parts = append(parts, "no NVRAM")
	case tpm == 0:
		parts = append(parts, "no TPM")
	}
	return strings.Join(parts, ", ")
}

// cmdPlatforms lists the platform modules and the libvirt settings each
// supplies; backup-vm's --virsh, --connect, --libvirt-socket and
// --swtpm-dir override them.
func cmdPlatforms() error {
	fmt.Printf("This host: %s\n\n", platform.Detect())
	for _, n := range platform.Names {
		c := platform.Appliances[n]
		fmt.Printf("%s\n", n)
		fmt.Printf("  virsh:      %s\n", c.Virsh)
		conn := c.ConnectURI()
		if conn == "" {
			conn = "(virsh's default)"
		}
		fmt.Printf("  connect:    %s\n", conn)
		fmt.Printf("  TPM state:  %s (layout %s)\n", c.SwtpmDir, c.TPMLayout)
		nv := c.NVRAMDir
		if nv == "" {
			nv = "(as each VM's definition says)"
		}
		fmt.Printf("  NVRAM:      %s\n", nv)
		if n == "truenas" {
			fmt.Printf("  also:       %s (VM settings)\n", platform.TrueNASMidclt)
		}
		fmt.Println()
	}
	return nil
}
