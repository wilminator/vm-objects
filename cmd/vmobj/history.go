// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/engine"
	"github.com/wilminator/vm-objects/internal/libvirt"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/progress"
	"github.com/wilminator/vm-objects/internal/qvs"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/target"
	"github.com/wilminator/vm-objects/internal/units"
)

// Snapshot history from the command line (milestone 9): backup --history
// and restore-history, for ZFS zvols. The VM-level commands build on the
// same engine calls.

// existingSnapshot splits a zfs:POOL/ZVOL@SNAP source into its zvol and
// snapshot; isSnap is false for a plain zfs:POOL/ZVOL or a file.
func existingSnapshot(spec string) (full string, isSnap bool) {
	ds, ok := strings.CutPrefix(spec, "zfs:")
	if !ok || !strings.Contains(ds, "@") {
		return "", false
	}
	return ds, true
}

// zfsSnapshotInfo looks up one of a zvol's snapshots for its manifest
// record: name, original date and description.
func zfsSnapshotInfo(ctx context.Context, full string) (*manifest.Snapshot, error) {
	zvol, name, _ := strings.Cut(full, "@")
	snaps, err := source.ListZFSSnapshots(ctx, nil, zvol)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if s.Name == name {
			return zfsRecord(s), nil
		}
	}
	return nil, fmt.Errorf("%s: no such snapshot", full)
}

func zfsRecord(s source.ZFSSnapshot) *manifest.Snapshot {
	return &manifest.Snapshot{Name: s.Name, Created: s.Created, Description: s.Description, Platform: "zfs"}
}

// backupHistory backs up a zvol's named snapshots that the repository
// doesn't have yet, oldest first, and copies each new backup to copyTo.
func backupHistory(ctx context.Context, base engine.BackupOptions, spec, format string, hash bool,
	pc progress.Config, copyTo []string, keyFile string) error {
	zvol, ok := strings.CutPrefix(spec, "zfs:")
	if !ok || strings.Contains(zvol, "@") {
		return errors.New("--history needs --source zfs:POOL/ZVOL (the zvol, not one of its snapshots)")
	}
	if format != "" && format != "raw" {
		return errors.New("--format does not apply to zfs: sources (zvols are raw)")
	}
	snaps, err := source.ListZFSSnapshots(ctx, nil, zvol)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Fprintf(os.Stderr, "%s has no named snapshots.\n", zvol)
		return nil
	}
	var list []engine.SnapshotSource
	for _, s := range snaps {
		full := zvol + "@" + s.Name
		list = append(list, engine.SnapshotSource{
			Info: *zfsRecord(s),
			Open: func(ctx context.Context) (source.Source, error) {
				return source.OpenZFS(ctx, zvol, source.ZFSOptions{Existing: full})
			},
		})
	}
	fmt.Fprintf(os.Stderr, "%d named snapshots of %s, oldest first.\n", len(list), zvol)
	start := time.Now()
	steps, err := engine.BackupHistory(ctx, engine.HistoryOptions{
		BackupOptions: base, Snapshots: list, Hash: hash,
		Step: func(i, n int, s manifest.Snapshot, skipped bool) {
			if skipped {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s (%s): already backed up\n", i+1, n, s.Name, s.Created.Local().Format("2006-01-02 15:04"))
			} else {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s (%s): backing up\n", i+1, n, s.Name, s.Created.Local().Format("2006-01-02 15:04"))
			}
		},
		Wrap: func(src source.Source) (source.Source, func()) { return withProgress(pc, zvol, src) },
	})
	added := 0
	for _, st := range steps {
		if st.Skipped {
			continue
		}
		added++
		fmt.Printf("Snapshot:  %-30s -> %s (%s written)\n", st.Snapshot.Name, st.Manifest, units.Format(st.Stats.BytesWritten))
		if len(copyTo) > 0 {
			name := st.Manifest
			if cerr := copyAfterBackup(ctx, base.Repo, copyTo, keyFile, base.Profile, base.Class, func(o engine.CopyOptions, cs *engine.CopyStats) error {
				return engine.CopyManifest(ctx, o, base.VM, name, cs)
			}); cerr != nil && err == nil {
				err = cerr
			}
		}
	}
	fmt.Printf("History:   %d snapshots backed up, %d already there, took %s\n", added, len(steps)-added, time.Since(start).Round(time.Second))
	return err
}

func cmdRestoreHistory(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore-history", flag.ExitOnError)
	dir := fs.String("repo", "", "repository: a folder path or s3://BUCKET[/PREFIX]?region=...")
	keyFile := fs.String("key-file", "", "key file")
	vm := fs.String("vm", "", "the disk's backup series")
	zvol := fs.String("zvol", "", "the zvol to rebuild the history in (POOL/NAME); created unless it exists and --resume is given")
	like := fs.String("like", "", "create the zvol like this one: size, volblocksize, compression, sparse")
	volsize := fs.String("volsize", "", "size of the new zvol, instead of --like (e.g. 250G; default: the largest disk in the history)")
	volblock := fs.String("volblocksize", "", "volblocksize of the new zvol, instead of --like")
	sparse := fs.Bool("sparse", false, "create the zvol sparse (no reservation)")
	resume := fs.Bool("resume", false, "continue an earlier replay into an existing zvol: skip the snapshots it has")
	staged := fs.String("staged", "", "folder (on another dataset than the zvol) for staged restores: each step is proven before the zvol changes")
	verify := fs.String("verify", "every", "every: check the whole zvol after each step; end: check what each step wrote, and the whole zvol after the last")
	list := fs.Bool("list", false, "only list the history that would be restored")
	pf := addProgressFlags(fs)
	fs.Parse(args)
	pc, perr := pf.config()
	if perr != nil {
		return perr
	}
	if err := require(fs, "repo", "key-file", "vm"); err != nil {
		return err
	}
	if *verify != "every" && *verify != "end" {
		return errors.New("--verify every or end")
	}
	r, err := openRepo(ctx, *dir, *keyFile)
	if err != nil {
		return err
	}
	ms, err := engine.HistoryManifests(ctx, r, *vm)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return engine.ErrNoHistory
	}
	fmt.Printf("History of %s: %d snapshots, oldest first\n", *vm, len(ms))
	var largest int64
	for _, m := range ms {
		hash := "no sha256"
		if m.SHA256 != "" {
			hash = "sha256 " + m.SHA256[:12] + "…"
		}
		fmt.Printf("  %-30s %s  %-36s %s, %s\n", m.Snapshot.Name, m.Snapshot.Created.Local().Format("2006-01-02 15:04"), m.Name, units.Format(m.DiskSize), hash)
		largest = max(largest, m.DiskSize)
	}
	if *list {
		return nil
	}
	if *zvol == "" {
		return errors.New("--zvol is required (or --list)")
	}
	t := &target.ZFS{Zvol: *zvol}
	if t.Exists(ctx) {
		if !*resume {
			return fmt.Errorf("%s already exists; pick a new name, or add --resume to continue an earlier replay into it", *zvol)
		}
		if err := t.WaitDevice(ctx, 30*time.Second); err != nil {
			return err
		}
	} else {
		spec := target.ZvolSpec{Size: largest, BlockSize: *volblock, Sparse: *sparse}
		if *like != "" {
			if spec, err = target.LikeZvol(ctx, nil, *like); err != nil {
				return err
			}
			if spec.Size < largest {
				return fmt.Errorf("--like %s is %s, smaller than the history's largest disk (%s)", *like, units.Format(spec.Size), units.Format(largest))
			}
		}
		if *volsize != "" {
			if spec.Size, err = units.ParseSize(*volsize); err != nil {
				return fmt.Errorf("--volsize: %w", err)
			}
		}
		fmt.Fprintf(os.Stderr, "Creating %s (%s)\n", *zvol, units.Format(spec.Size))
		if err := t.Create(ctx, spec); err != nil {
			return err
		}
	}
	report, stopProgress := restoreProgress(pc, t.Output(), "")
	defer stopProgress()
	start := time.Now()
	steps, err := engine.Replay(ctx, engine.ReplayOptions{
		Repo: r, VM: *vm, Target: t, Staged: *staged, WrittenOnly: *verify == "end",
		Step: func(i, n int, m *manifest.Manifest, skipped bool) {
			if skipped {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s: already in %s, skipped\n", i+1, n, m.Snapshot.Name, *zvol)
			} else {
				fmt.Fprintf(os.Stderr, "[%d/%d] %s: restoring %s\n", i+1, n, m.Snapshot.Name, m.Name)
			}
		},
		Progress: func(p engine.Phase, done, total int64) { report(t.Output(), p, done, total) },
	})
	stopProgress()
	for _, s := range steps {
		switch {
		case s.Skipped:
			fmt.Printf("  %-30s skipped (already there)\n", s.Snapshot.Name)
		case s.Proven:
			fmt.Printf("  %-30s %s written, sha256 matches the original\n", s.Snapshot.Name, units.Format(s.Stats.Written))
		default:
			fmt.Printf("  %-30s %s written, checked against the backup\n", s.Snapshot.Name, units.Format(s.Stats.Written))
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("Done: %s holds all %d snapshots (took %s). Point the VM's disk at %s when ready.\n",
		*zvol, len(steps), time.Since(start).Round(time.Second), t.Output())
	return nil
}

type restoreQCOW2Args struct {
	repo, keyFile, vm, manifest, out, backing, backingFormat, base, expect, mode string
	check, request, others                                                       bool
	tier                                                                         policy.Tier
	days                                                                         int
	pc                                                                           progress.Config
}

// restoreQCOW2 is restore --output-format qcow2: a new image, standalone
// or as an overlay on --backing.
func restoreQCOW2(ctx context.Context, a restoreQCOW2Args) error {
	if a.mode != "rename" {
		return errors.New("restore: --output-format qcow2 always writes a new image; --mode doesn't apply")
	}
	if a.others {
		return errors.New("restore: --force, --journal, --scratch and --check-written don't apply to --output-format qcow2")
	}
	if a.base != "" && a.backing == "" {
		return errors.New("restore: --base with --output-format qcow2 needs --backing (the image that holds the base)")
	}
	if a.expect != "" && !a.check {
		return errors.New("restore: --expect-sha256 needs --check")
	}
	r, err := openRepo(ctx, a.repo, a.keyFile)
	if err != nil {
		return err
	}
	start := time.Now()
	report, stop := restoreProgress(a.pc, a.out, "")
	o := engine.QCOW2Options{Repo: r, VM: a.vm, Manifest: a.manifest, Output: a.out, Base: a.base,
		Check: a.check, ExpectSHA256: a.expect,
		Progress: func(p engine.Phase, done, total int64) { report("", p, done, total) }}
	if a.backing != "" {
		o.Backing, o.BackingFormat = a.backing, a.backingFormat
	}
	st, err := engine.RestoreQCOW2(ctx, o)
	stop()
	var nr *engine.NotReadyError
	if errors.As(err, &nr) {
		return explainNotReady(ctx, r, nr.Readiness, a.request, a.tier, a.days)
	}
	if err != nil {
		return err
	}
	what := "qcow2 image"
	if a.backing != "" {
		what = "qcow2 overlay on " + a.backing
	}
	fmt.Printf("Restored %s to %s (%s) in %s.\n", units.Format(st.DiskBytes), a.out, what, time.Since(start).Round(time.Second))
	if a.backing != "" {
		if st.BlocksSkipped > 0 {
			fmt.Printf("  Skipped:   %d blocks the same in --base, not read\n", st.BlocksSkipped)
		}
		fmt.Printf("  Compared:  %s with the backing file; %s written into the overlay\n", units.Format(st.Compared), units.Format(st.Written))
	}
	fmt.Printf("  Fetched:   %d blocks and %d newer extents\n", st.BlocksRead, st.ExtentsRead)
	if st.Checked {
		fmt.Println("  Checked:   read back through the whole chain, every block matches")
		fmt.Printf("  SHA-256:   %s\n", st.SHA256)
	}
	return nil
}

// cmdQNAPSnapshots registers the snapshots restore-vm --history prepared,
// for a VM created in Virtualization Station on the restored top layers.
func cmdQNAPSnapshots(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("qnap-snapshots", flag.ExitOnError)
	planPath := fs.String("plan", "", "the "+engine.QNAPPlanFile+" restore-vm --history wrote")
	definition := fs.String("definition", "", "the new VM's definition: .<UUID>.meta/<UUID>.xml in its folder")
	apply := fs.Bool("apply", false, "write the records; without it, only check and list what would be written")
	virsh := fs.String("virsh", platform.QNAPVirsh, "virsh used to check that the VM is shut off (empty: don't check)")
	fs.Parse(args)
	if err := require(fs, "plan", "definition"); err != nil {
		return err
	}
	plan, err := engine.ReadQNAPPlan(*planPath)
	if err != nil {
		return err
	}
	var runner libvirt.Runner
	if *virsh != "" {
		if _, err := os.Stat(*virsh); err == nil {
			runner = libvirt.Virsh{Path: *virsh}
		}
	}
	paths, err := engine.RegisterQNAPSnapshots(ctx, plan, *definition, runner, *apply)
	if err != nil {
		return err
	}
	verb := "Would write"
	if *apply {
		verb = "Wrote"
	}
	fmt.Printf("%s %d snapshot records for %s (backup %s):\n", verb, len(paths), plan.VM, plan.Record)
	for i, p := range paths {
		r := plan.Records[i]
		fmt.Printf("  %s  %q, taken %s\n", p, r.DisplayName, time.Unix(r.Epoch, 0).Local().Format("2006-01-02 15:04"))
	}
	if !*apply {
		fmt.Println("Nothing was written. Run again with --apply to write them.")
	} else {
		fmt.Println("Open the VM in Virtualization Station: its Snapshots tab lists them.")
	}
	return nil
}

// cmdQVSCheck logs in to Virtualization Station's API and lists what it
// reports, changing nothing: to confirm the routes and the account's
// rights on a live system.
func cmdQVSCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("qvs-check", flag.ExitOnError)
	creds := fs.String("qvs-credentials", "", "credentials file ([qvs] url, username, password; mode 600)")
	vm := fs.String("vm", "", "also list this VM's state and snapshots (name or UUID)")
	fs.Parse(args)
	if err := require(fs, "qvs-credentials"); err != nil {
		return err
	}
	cfg, err := qvs.LoadConfig(*creds)
	if err != nil {
		return err
	}
	c, err := qvs.New(cfg)
	if err != nil {
		return err
	}
	if err := c.Login(ctx); err != nil {
		return err
	}
	defer c.Logout(context.WithoutCancel(ctx))
	fmt.Printf("Logged in to %s as %s.\n", cfg.URL, cfg.Username)
	vms, err := c.ListVMs(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d VMs:\n", len(vms))
	for _, v := range vms {
		fmt.Printf("  %-4d %-24s %-38s %s\n", v.ID, v.Name, v.UUID, qvs.NormalState(v.PowerState))
	}
	if *vm == "" {
		return nil
	}
	// A UUID first; failing that, a name.
	v, err := c.FindVM(ctx, *vm, "")
	if err != nil {
		if v, err = c.FindVM(ctx, "", *vm); err != nil {
			return err
		}
	}
	st, err := c.State(ctx, v.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s (id %d): %s\n", v.Name, v.ID, st)
	snaps, err := c.ListSnapshots(ctx, v.ID)
	if err != nil {
		return err
	}
	fmt.Printf("%d snapshots:\n", len(snaps))
	for _, s := range snaps {
		lock := ""
		if s.Locked {
			lock = " (locked)"
		}
		fmt.Printf("  %-12d %-28s %s%s\n", s.ID, s.Name, s.Created.Local().Format("2006-01-02 15:04"), lock)
	}
	return nil
}

// cmdQVSCleanup removes snapshots vmobj took for backups and left behind
// (a run that was killed, or an old build): only those named
// engine.SnapshotNamePrefix + time. Anything else is never touched.
func cmdQVSCleanup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("qvs-cleanup", flag.ExitOnError)
	creds := fs.String("qvs-credentials", "", "credentials file ([qvs] url, username, password; mode 600)")
	vm := fs.String("vm", "", "the VM (name or UUID)")
	apply := fs.Bool("apply", false, "delete them; without it, only list them")
	fs.Parse(args)
	if err := require(fs, "qvs-credentials", "vm"); err != nil {
		return err
	}
	cfg, err := qvs.LoadConfig(*creds)
	if err != nil {
		return err
	}
	c, err := qvs.New(cfg)
	if err != nil {
		return err
	}
	if err := c.Login(ctx); err != nil {
		return err
	}
	defer c.Logout(context.WithoutCancel(ctx))
	v, err := c.FindVM(ctx, *vm, "")
	if err != nil {
		if v, err = c.FindVM(ctx, "", *vm); err != nil {
			return err
		}
	}
	snaps, err := c.ListSnapshots(ctx, v.ID)
	if err != nil {
		return err
	}
	var ours []qvs.Snapshot
	for _, s := range snaps {
		if strings.HasPrefix(s.Name, engine.SnapshotNamePrefix) {
			ours = append(ours, s)
		}
	}
	if len(ours) == 0 {
		fmt.Printf("%s has no snapshots left by vmobj (%d others, untouched).\n", v.Name, len(snaps))
		return nil
	}
	for _, s := range ours {
		lock := ""
		if s.Locked {
			lock = " (locked)"
		}
		if !*apply {
			fmt.Printf("Would delete %d %s, taken %s%s\n", s.ID, s.Name, s.Created.Local().Format("2006-01-02 15:04"), lock)
			continue
		}
		if err := c.DeleteSnapshot(ctx, v.ID, s.ID); err != nil {
			return fmt.Errorf("deleting %s: %w", s.Name, err)
		}
		fmt.Printf("Deleted %d %s%s\n", s.ID, s.Name, lock)
	}
	if !*apply {
		fmt.Printf("%d to delete; nothing was changed. Run again with --apply.\n", len(ours))
	}
	return nil
}
