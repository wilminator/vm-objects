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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wilminator/vm-objects/internal/engine"
	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/measure"
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
  vmobj init        --repo REPO --key-file PATH [--block-size 4MiB] [--extent-size 64KiB]
  vmobj measure     --source DISK [--name NAME] [--state-dir DIR]
  vmobj backup      --repo REPO --key-file PATH --source DISK --vm NAME --type full|incremental|differential
  vmobj list        --repo REPO --key-file PATH [--vm NAME]
  vmobj restore     --repo REPO --key-file PATH --vm NAME --manifest NAME --output PATH [--force]
  vmobj verify      --repo REPO --key-file PATH --vm NAME --manifest NAME [--deep]
  vmobj version

REPO is a folder path, or an S3 bucket:
  s3://BUCKET[/PREFIX][?region=R][&endpoint=URL][&profile=P][&path-style=true|false]
  endpoint= points at an S3-compatible service such as QNAP QuObjects or MinIO.
  Credentials come from the AWS environment variables or ~/.aws/credentials
  (choose a section with profile=), never from the address.

DISK is a raw image file or a block device (for example a zvol snapshot).
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

// progress wraps a source and prints how far a scan has got every few
// seconds. Go note: atomic.Int64 can be updated from the scanning
// goroutine and read from the printing goroutine without a lock.
type progress struct {
	source.Source
	read atomic.Int64
}

func (p *progress) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.Source.ReadAt(b, off)
	p.read.Add(int64(n))
	return n, err
}

func withProgress(src source.Source) (*progress, func()) {
	p := &progress{Source: src}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				n := p.read.Load()
				rate := float64(n) / time.Since(start).Seconds()
				fmt.Fprintf(os.Stderr, "  %5.1f%%  %s of %s  (%s/s)\n",
					100*float64(n)/float64(src.Size()), units.Format(n), units.Format(src.Size()), units.Format(int64(rate)))
			}
		}
	}()
	return p, func() { close(done) }
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
	fs.Parse(args)
	if err := require(fs, "repo", "key-file"); err != nil {
		return err
	}
	k, err := keys.Load(*keyFile)
	if err != nil {
		return err
	}
	cfg := repo.DefaultConfig()
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
		*name = filepath.Base(*src)
	}
	if err := manifest.CheckVMName(*name); err != nil {
		return fmt.Errorf("--name: %w", err)
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(defaultStateDir(), "measure")
	}
	path := filepath.Join(*stateDir, *name+".hashes")

	disk, err := source.OpenRaw(*src)
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

func cmdBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	src := fs.String("source", "", "raw disk image or block device (use a snapshot)")
	vm := fs.String("vm", "", "VM name")
	kind := fs.String("type", "", "full, incremental or differential")
	stateDir := fs.String("state-dir", "", "where this host keeps per-VM state")
	workers := fs.Int("workers", 0, "blocks hashed at once (default: one per CPU)")
	fs.Parse(args)
	if err := require(fs, "repo", "key-file", "source", "vm", "type"); err != nil {
		return err
	}
	k, err := manifest.ParseKind(*kind)
	if err != nil {
		return err
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(defaultStateDir(), r.Config.RepoID)
	}
	disk, err := source.OpenRaw(*src)
	if err != nil {
		return err
	}
	defer disk.Close()
	p, stopProgress := withProgress(disk)
	fmt.Fprintf(os.Stderr, "%s backup of %s (%s)...\n", k, *vm, units.Format(disk.Size()))
	start := time.Now()
	st, err := engine.Backup(ctx, engine.BackupOptions{
		Repo: r, Source: p, VM: *vm, Kind: k, Workers: *workers,
		StatePath: filepath.Join(*stateDir, *vm+".state"),
	})
	stopProgress()
	if err != nil {
		return err
	}
	fmt.Printf("Manifest:  %s\n", st.Manifest)
	fmt.Printf("Took:      %s\n", time.Since(start).Round(time.Second))
	if k == manifest.Full {
		fmt.Printf("Blocks:    %d stored, %d reused, %d all zeros\n", st.BlocksStored, st.BlocksReused, st.ZeroBlocks)
	} else {
		fmt.Printf("Extents:   %d differ from the full; %d stored, %d reused\n", st.ExtentsChanged, st.ExtentsStored, st.ExtentsReused)
		fmt.Printf("Packs:     %d\n", st.Packs)
	}
	fmt.Printf("Written:   %s (compressed and encrypted)\n", units.Format(st.BytesWritten))
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
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
			fmt.Printf("  %s\n", n)
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
	fs.Parse(args)
	if err := require(fs, "repo", "key-file", "vm", "manifest", "output"); err != nil {
		return err
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	start := time.Now()
	st, err := engine.Restore(ctx, engine.RestoreOptions{Repo: r, VM: *vm, Manifest: *name, Output: *out, Force: *force})
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
	if len(vs.Missing) == 0 {
		fmt.Printf("OK: %d blocks and %d extents are %s.\n", vs.Blocks, vs.Extents, how)
		return nil
	}
	for _, m := range vs.Missing {
		fmt.Println("  problem:", m)
	}
	return fmt.Errorf("%d problems found", len(vs.Missing))
}
