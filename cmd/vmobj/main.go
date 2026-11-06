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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wilminator/vm-objects/internal/engine"
	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/libvirt"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/measure"
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
                    [--request-restore [--tier bulk|standard|expedited] [--days N]]
  vmobj backup-vm   --repo REPO --key-file PATH --domain NAME --type full|incremental|differential
                    [--vm NAME] [--profile NAME] [--quiesce] [--virsh PATH] [--connect URI] [--swtpm-dir DIR]
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
  vmobj version

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
	// Ctrl-C cancels the context; work stops cleanly between blocks, and
	// no manifest is written for an unfinished run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	case "version":
		fmt.Println("vmobj", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
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
	return repo.Open(ctx, b, k)
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

// meter prints how far a long job has got every few seconds: percent,
// bytes and average speed. Go note: atomic.Int64 can be updated from the
// working goroutine and read from the printing goroutine without a lock.
type meter struct {
	done, total atomic.Int64
	stop        chan struct{}
	stopped     sync.Once
}

func startMeter(total int64) *meter {
	m := &meter{stop: make(chan struct{})}
	m.total.Store(total)
	start := time.Now()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-t.C:
				n, tot := m.done.Load(), m.total.Load()
				pct := 0.0
				if tot > 0 {
					pct = 100 * float64(n) / float64(tot)
				}
				rate := float64(n) / time.Since(start).Seconds()
				fmt.Fprintf(os.Stderr, "  %5.1f%%  %s of %s  (%s/s)\n",
					pct, units.Format(n), units.Format(tot), units.Format(int64(rate)))
			}
		}
	}()
	return m
}

// Stop ends the printing; safe to call more than once.
func (m *meter) Stop() { m.stopped.Do(func() { close(m.stop) }) }

// progress wraps a source and counts what a scan has read.
type progress struct {
	source.Source
	m *meter
}

func (p *progress) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.Source.ReadAt(b, off)
	p.m.done.Add(int64(n))
	return n, err
}

func withProgress(src source.Source) (*progress, func()) {
	m := startMeter(src.Size())
	return &progress{Source: src, m: m}, m.Stop
}

// restoreProgress prints what a restore is doing: a line when it starts
// checking the backup, writing the disk and applying later changes
// (naming the disk, if given), and the meter while it writes. Call the returned func when the
// restore ends.
func restoreProgress(dest string) (func(target string, p engine.Phase, done, total int64), func()) {
	var (
		mu      sync.Mutex
		m       *meter
		current string
		phase   engine.Phase
	)
	finish := func() {
		if m != nil {
			m.Stop()
			m = nil
		}
	}
	report := func(target string, p engine.Phase, done, total int64) {
		mu.Lock()
		defer mu.Unlock()
		label := dest
		if target != "" {
			label = "disk " + target
		}
		switch {
		case p == engine.PhaseChecking:
			finish()
			fmt.Fprintf(os.Stderr, "Checking that the backup for %s is complete...\n", label)
		case p != phase || target != current || m == nil:
			finish()
			phase, current = p, target
			if p == engine.PhaseChanges {
				fmt.Fprintf(os.Stderr, "Applying %s of later changes to %s...\n", units.Format(total), label)
			} else {
				fmt.Fprintf(os.Stderr, "Writing %s to %s...\n", units.Format(total), label)
			}
			m = startMeter(total)
			m.done.Store(done)
		default:
			m.done.Store(done)
		}
	}
	return report, func() { mu.Lock(); finish(); mu.Unlock() }
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
	fs.Parse(args)
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
	p, stopProgress := withProgress(disk)
	fmt.Fprintf(os.Stderr, "Measuring %s (%s)...\n", *src, units.Format(disk.Size()))
	start := time.Now()
	cur, err := measure.Run(ctx, p, scan.Options{BlockSize: extent.Length(bs), ExtentSize: extent.Length(es), Workers: *workers})
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
	fs.Parse(args)
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
	p, stopProgress := withProgress(disk)
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
	force := fs.Bool("force", false, "replace an existing output file")
	request := fs.Bool("request-restore", false, "ask S3 to restore archived objects this restore needs")
	tierFlag := fs.String("tier", "bulk", "retrieval speed for --request-restore: bulk, standard or expedited")
	days := fs.Int("days", 7, "days the restored copies stay readable")
	fs.Parse(args)
	if err := require(fs, "repo", "key-file", "vm", "manifest", "output"); err != nil {
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
	report, stopReport := restoreProgress(*out)
	st, err := engine.Restore(ctx, engine.RestoreOptions{Repo: r, VM: *vm, Manifest: *name, Output: *out, Force: *force,
		Progress: func(p engine.Phase, done, total int64) { report("", p, done, total) }})
	stopReport()
	var nr *engine.NotReadyError
	if errors.As(err, &nr) {
		return explainNotReady(ctx, r, nr.Readiness, *request, tier, *days)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Restored %s to %s in %s: %d blocks, %d newer extents, %d zero blocks.\n",
		units.Format(st.DiskBytes), *out, time.Since(start).Round(time.Second), st.BlocksRead, st.ExtentsRead, st.ZeroBlocks)
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	vm := fs.String("vm", "", "VM name")
	name := fs.String("manifest", "", "manifest to check")
	deep := fs.Bool("deep", false, "download and check every piece")
	fs.Parse(args)
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
	swtpmDir := fs.String("swtpm-dir", engine.DefaultSwtpmDir, "where libvirt keeps TPM state (QNAP: /QVS/var/lib/libvirt/swtpm)")
	virshPath := fs.String("virsh", "virsh", "virsh program (QNAP: /QVS/usr/bin/virsh)")
	connect := fs.String("connect", "", "libvirt URI, e.g. qemu:///system")
	var copyTo repeated
	fs.Var(&copyTo, "copy-to", "also copy the new backup to this repository (repeatable; same key file)")
	fs.Parse(args)
	if err := require(fs, "repo", "key-file", "domain", "type"); err != nil {
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
	start := time.Now()
	st, err := engine.BackupVM(ctx, engine.BackupVMOptions{
		Repo: r, Domain: *domain, VM: *vm, Kind: k, Profile: &prof, Class: class, Workers: *workers,
		StateDir: *stateDir, Quiesce: *quiesce, SwtpmDir: *swtpmDir,
		Virsh: libvirt.Virsh{Path: *virshPath, Connect: *connect},
		Progress: func(target string, s source.Source) (source.Source, func()) {
			fmt.Fprintf(os.Stderr, "Disk %s (%s)...\n", target, units.Format(s.Size()))
			return withProgress(s)
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
	fmt.Printf("Files:     %d (NVRAM and TPM state, inside the record)\n", st.Files)
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
	request := fs.Bool("request-restore", false, "ask S3 to restore archived objects this restore needs")
	tierFlag := fs.String("tier", "bulk", "retrieval speed for --request-restore: bulk, standard or expedited")
	days := fs.Int("days", 7, "days the restored copies stay readable")
	fs.Parse(args)
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
	report, stopReport := restoreProgress(*to)
	st, err := engine.RestoreVM(ctx, engine.RestoreVMOptions{Repo: r, VM: *vm, Record: *record, Dir: *to, NewName: *newName, Progress: report})
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
			if err = copyOne(engine.CopyOptions{From: from, To: to, Profile: prof, Class: class}, cs); err == nil {
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
	fs.Parse(args)
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
	o := engine.CopyOptions{From: src, To: dst, Profile: &prof, Class: class}
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
