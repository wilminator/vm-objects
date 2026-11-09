// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/atomicfile"
	"github.com/wilminator/vm-objects/internal/libvirt"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// BackupVMOptions describe a whole-VM backup.
type BackupVMOptions struct {
	Repo     *repo.Repo
	Domain   string // libvirt name or UUID
	VM       string // name in the repository; default: the domain's name
	Kind     manifest.Kind
	Profile  *policy.Profile
	Class    policy.Class
	Workers  int
	StateDir string // per-disk state files live here
	Now      func() time.Time

	// Platform describes the VM. When nil, the generic libvirt module is
	// built from Virsh and SwtpmDir.
	Platform platform.Platform
	Virsh    libvirt.Runner
	ZFS      source.ZFSRunner // nil: the real zfs command
	// ZFSDevDir is where zvol device nodes appear (default /dev/zvol).
	ZFSDevDir string
	// Quiesce freezes the guest's filesystems (through the QEMU guest
	// agent) while a running VM's disks are snapshotted.
	Quiesce bool
	// SwtpmDir holds emulated TPM state, one folder per VM UUID.
	SwtpmDir string // default /var/lib/libvirt/swtpm
	// Progress, when set, wraps each disk source (for progress output).
	Progress func(target string, s source.Source) (source.Source, func())

	// History first backs up each disk's named snapshots that the
	// repository lacks (milestone 9), then the VM as usual. Zvol disks'
	// ZFS snapshots are read directly; other disks need a platform that
	// lists snapshots (platform.Snapshotter).
	History bool
	// KeepSnapshots leaves vmobj's older platform snapshots of a running
	// VM alone (by default each good backup deletes them, keeping only the
	// newest). NoSnapshots refuses to back up a running VM at all.
	KeepSnapshots bool
	NoSnapshots   bool
	// Hash records each history snapshot's whole-disk SHA-256.
	Hash bool
	// HistoryStep, when set, is told about each disk's snapshots in turn.
	HistoryStep func(target string, i, n int, snap manifest.Snapshot, skipped bool)
}

// DefaultSwtpmDir is where libvirt keeps emulated TPM state.
const DefaultSwtpmDir = platform.DefaultSwtpmDir

// platform returns the module that describes the VM.
func (o BackupVMOptions) platform() platform.Platform {
	if o.Platform != nil {
		return o.Platform
	}
	return platform.NewLibvirt(o.Virsh, o.SwtpmDir)
}

// BackupVMStats summarise a whole-VM backup.
type BackupVMStats struct {
	Record      string
	Consistency string
	Disks       map[string]*BackupStats // by target
	Files       int                     // every file kept with the record
	NVRAMFiles  int
	TPMFiles    int
	ConfigFiles []string // names of platform settings files, such as truenas-vm.json
	Skipped     []string
	// History is what --history did, by disk target.
	History map[string][]HistoryStep
}

// ErrLiveQCOW2 means a running VM has file-backed disks, which can't be
// snapshotted without changing the VM's disk chain.
var ErrLiveQCOW2 = errors.New("the VM is running and has disk image files; shut it down for the backup " +
	"(vmobj does not modify a running VM's disk chain), or keep its disks on ZFS zvols, which can be snapshotted live")

// BackupVM backs up every disk of a libvirt domain, then its definition,
// NVRAM and TPM state, and writes a VM record last.
func BackupVM(ctx context.Context, o BackupVMOptions) (st *BackupVMStats, err error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.KeepSnapshots && o.NoSnapshots {
		return nil, errors.New("--keep-snapshots and --no-snapshots contradict each other")
	}
	plat := o.platform()
	dom, err := plat.Describe(ctx, o.Domain)
	if err != nil {
		return nil, err
	}
	if o.Quiesce {
		if _, ok := plat.(platform.Quiescer); !ok {
			return nil, fmt.Errorf("--quiesce: the %s platform module can't freeze a guest", plat.Name())
		}
	}
	if o.VM == "" {
		o.VM = dom.Name
	}
	if err := manifest.CheckVMName(o.VM); err != nil {
		return nil, fmt.Errorf("VM name: %w (use --vm to choose another)", err)
	}
	live := dom.Running
	var disks []vmdef.Disk
	stats := &BackupVMStats{Disks: map[string]*BackupStats{}, Skipped: dom.Skipped}
	for _, d := range dom.Disks {
		if d.ReadOnly || d.Shared {
			stats.Skipped = append(stats.Skipped, fmt.Sprintf("disk %s (read-only or shared)", d.Target))
			continue
		}
		if err := manifest.CheckVMName(manifest.DiskStream(o.VM, d.Target)); err != nil {
			return nil, err
		}
		disks = append(disks, d)
	}
	if len(disks) == 0 {
		return nil, fmt.Errorf("domain %s has no disks to back up", dom.Name)
	}

	// Find every problem before touching anything. A running VM's disks
	// are read from a snapshot: ZFS's for zvols, the platform's own (a
	// qcow2 overlay, through its API) for image files.
	var zvols []string
	var overlayDisks []vmdef.Disk
	_, canOverlay := plat.(platform.SnapshotManager)
	for _, d := range disks {
		if !live {
			continue
		}
		if ds, ok := strings.CutPrefix(d.Path, "/dev/zvol/"); ok && d.Type == "block" {
			zvols = append(zvols, ds)
			continue
		}
		if canOverlay && d.Type == "file" && d.Format == "qcow2" {
			overlayDisks = append(overlayDisks, d)
			continue
		}
		return nil, fmt.Errorf("disk %s (%s): %w", d.Target, d.Path, ErrLiveQCOW2)
	}
	if len(zvols) > 0 && len(overlayDisks) > 0 {
		return nil, errors.New("the VM runs with both zvol and image-file disks; vmobj can't snapshot both kinds at the same moment, so shut it down for the backup")
	}
	if live && o.NoSnapshots {
		return nil, ErrNoSnapshots
	}
	// History goes first: its first snapshot makes the disk's full, so an
	// incremental of the current state can follow on a new repository.
	if o.History {
		if stats.History, err = backupVMHistory(ctx, o, plat, dom, disks); err != nil {
			return nil, err
		}
	}
	if o.Kind != manifest.Full {
		for _, d := range disks {
			st, err := LoadState(diskStatePath(o, d.Target))
			if err != nil {
				return nil, err
			}
			if !st.usableFor(st.sizeOrZero(), o.Repo.Config) || st.RepoID != o.Repo.Config.RepoID {
				return nil, fmt.Errorf("disk %s: %w", d.Target, ErrNeedFull)
			}
		}
	}
	files := dom.Files

	// Capture the disks.
	started := o.Now().UTC()
	stats.Consistency = "shut off"
	snaps := map[string]string{}
	var frozen map[string]string // image-file disks of a running VM: target -> the layer to read
	if live && len(overlayDisks) > 0 {
		stats.Consistency = "snapshot"
		var finish func(bool)
		if frozen, finish, err = snapshotOverlays(ctx, o, plat, dom, overlayDisks, started); err != nil {
			return nil, err
		}
		defer func() { finish(err == nil) }()
		if o.Quiesce {
			stats.Consistency = "quiesced snapshot"
		}
	} else if live {
		stats.Consistency = "snapshot"
		if snaps, err = snapshotVM(ctx, o, plat, zvols, started); err != nil {
			return nil, err
		}
		if o.Quiesce {
			stats.Consistency = "quiesced snapshot"
		}
	}
	// Snapshots not yet handed to a source must still be removed if a
	// later disk fails.
	pending := map[string]string{}
	for k, v := range snaps {
		pending[k] = v
	}
	defer func() {
		for _, snap := range pending {
			o.zfs().Run(context.WithoutCancel(ctx), "destroy", snap)
		}
	}()

	rec := &manifest.VMRecord{
		Format: manifest.Format, VM: o.VM, Kind: o.Kind, Created: started,
		Platform: plat.Name(), DomainName: dom.Name, UUID: dom.UUID,
		Definition: dom.Definition, DefinitionType: dom.DefinitionType,
		Consistency: stats.Consistency, Files: files, Skipped: stats.Skipped,
	}
	for _, d := range disks {
		var src source.Source
		if layer, ok := frozen[d.Target]; ok {
			src, err = source.Open(layer, "qcow2") // with everything below it
		} else {
			src, err = openVMDisk(ctx, o, d, live, pending)
		}
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Target, err)
		}
		var read source.Source = src
		done := func() {}
		if o.Progress != nil {
			read, done = o.Progress(d.Target, src)
		}
		st, err := Backup(ctx, BackupOptions{
			Repo: o.Repo, Source: read, VM: manifest.DiskStream(o.VM, d.Target), Kind: o.Kind,
			StatePath: diskStatePath(o, d.Target), Workers: o.Workers,
			Now: func() time.Time { return started }, Profile: o.Profile, Class: o.Class,
		})
		done()
		cerr := src.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Target, err)
		}
		stats.Disks[d.Target] = st
		rec.Disks = append(rec.Disks, manifest.VMDisk{
			Target: d.Target, Stream: manifest.DiskStream(o.VM, d.Target), Manifest: st.Manifest,
			Type: d.Type, Format: d.Format, Path: d.Path,
		})
	}

	// The record goes last, with the same retention and lock as a manifest.
	var lockUntil time.Time
	if o.Profile != nil {
		rec.Profile = o.Profile.Name
		rec.RetainUntil = started.Add(o.Profile.Rule(o.Kind).Retain)
		if o.Repo.Config.ObjectLock {
			lockUntil = rec.RetainUntil.Add(o.Profile.Margin)
		}
	} else if o.Repo.Config.ObjectLock {
		return nil, ErrPolicyNeeded
	}
	name, err := uniqueRecordName(ctx, o.Repo, o.VM, started, o.Kind)
	if err != nil {
		return nil, err
	}
	rec.Name = name
	if err := o.Repo.PutVMRecord(ctx, rec, store.PutOptions{LockUntil: lockUntil}); err != nil {
		return nil, err
	}
	stats.Record, stats.Files = name, len(files)
	for _, f := range files {
		switch f.Role {
		case "nvram":
			stats.NVRAMFiles++
		case "tpm":
			stats.TPMFiles++
		case "config":
			stats.ConfigFiles = append(stats.ConfigFiles, f.Name)
		}
	}
	return stats, nil
}

func (o BackupVMOptions) zfs() source.ZFSRunner {
	if o.ZFS == nil {
		return source.ExecZFS{}
	}
	return o.ZFS
}

func diskStatePath(o BackupVMOptions, target string) string {
	return filepath.Join(o.StateDir, manifest.DiskStream(o.VM, target)+".state")
}

// sizeOrZero lets a missing state fail usableFor without a nil check.
func (s *State) sizeOrZero() int64 {
	if s == nil {
		return 0
	}
	return s.DiskSize
}

// snapshotVM snapshots a running VM's zvols in one atomic command, inside
// a filesystem freeze when asked. The thaw always runs.
func snapshotVM(ctx context.Context, o BackupVMOptions, plat platform.Platform, zvols []string, now time.Time) (snaps map[string]string, err error) {
	if q, ok := plat.(platform.Quiescer); o.Quiesce && ok {
		if err := q.Freeze(ctx, o.Domain); err != nil {
			return nil, fmt.Errorf("freezing the guest's filesystems (is the QEMU guest agent running?): %w", err)
		}
		defer func() {
			// Thaw even if the snapshot failed or ctx was cancelled.
			if terr := q.Thaw(context.WithoutCancel(ctx), o.Domain); terr != nil && err == nil {
				err = fmt.Errorf("thawing the guest's filesystems: %w", terr)
			}
		}()
	}
	return source.SnapshotZvols(ctx, o.zfs(), now, zvols...)
}

// SnapshotNamePrefix starts the name of every platform snapshot vmobj
// takes for a backup of a running VM.
//
// Deleting the newest snapshot of a layered disk doesn't merge its overlay
// back (Virtualization Station only drops the record), so taking one per
// backup and deleting it again would add a layer to the live disk each
// night. Instead one vmobj snapshot is left standing: each backup takes a
// new one, reads from it, and after it succeeds deletes the older ones,
// which sit in the middle of the chain and are merged. See KeepSnapshots
// and NoSnapshots.
const SnapshotNamePrefix = platform.BackupSnapshotPrefix

// snapshotDescription is what vmobj writes in its snapshots' description.
const snapshotDescription = "Taken by vmobj for a backup " + platform.BackupSnapshotMarker +
	". vmobj keeps the newest and deletes older ones after each good backup."

// ErrNoSnapshots means --no-snapshots was given and the VM is running.
var ErrNoSnapshots = errors.New("the VM is running and --no-snapshots forbids taking a platform snapshot; shut it down for the backup, or drop --no-snapshots")

// snapshotOverlays takes a platform snapshot of a running VM whose disks
// are qcow2 image files (QNAP Virtualization Station, through its API).
// The platform starts a new overlay on top of each disk, so the file each
// disk used until then can't change any more: that is the layer to read,
// with everything below it. release deletes the snapshot again (the
// platform merges the overlay back); it must run even if the backup fails.
//
// finish(ok) is called once the backup is done. The new snapshot always
// stays. If the backup succeeded (and keep is false), vmobj's older
// snapshots of this VM are deleted: they are middle points, so the platform
// merges them down and the chain stays the same depth.
func snapshotOverlays(ctx context.Context, o BackupVMOptions, plat platform.Platform, dom *platform.VM,
	disks []vmdef.Disk, now time.Time) (layers map[string]string, finish func(ok bool), err error) {
	sm := plat.(platform.SnapshotManager)
	tops := map[string]string{}
	for _, d := range disks {
		tops[d.Target] = d.Path
	}
	info := platform.SnapshotInfo{
		Name:        SnapshotNamePrefix + now.UTC().Format("20060102T150405Z"),
		Description: snapshotDescription,
	}
	snap, err := func() (snap platform.SnapshotInfo, err error) {
		if q, ok := plat.(platform.Quiescer); o.Quiesce && ok {
			if err := q.Freeze(ctx, o.Domain); err != nil {
				return snap, fmt.Errorf("freezing the guest's filesystems (is the QEMU guest agent running?): %w", err)
			}
			defer func() {
				if terr := q.Thaw(context.WithoutCancel(ctx), o.Domain); terr != nil && err == nil {
					err = fmt.Errorf("thawing the guest's filesystems: %w", terr)
				}
			}()
		}
		return sm.CreateSnapshot(ctx, dom, info)
	}()
	if err != nil {
		// The platform may have taken it anyway (QNAP did, once, behind a
		// reply vmobj didn't expect): look, and remove it if it's there.
		if snaps, lerr := sm.ListSnapshots(context.WithoutCancel(ctx), dom); lerr == nil {
			for _, s := range snaps {
				if s.Name == info.Name {
					if derr := sm.DeleteSnapshot(context.WithoutCancel(ctx), dom, s.ID); derr != nil {
						err = fmt.Errorf("%w; it was taken anyway and deleting it failed too (%v): delete %s in the platform's UI", err, derr, info.Name)
					}
				}
			}
		}
		return nil, nil, fmt.Errorf("taking the platform snapshot %s: %w", info.Name, err)
	}
	// release removes the new snapshot again: only when it turns out not to
	// be usable. (The newest snapshot's layer isn't merged back, but a
	// snapshot that froze nothing is no use to keep.)
	release := func() {
		if derr := sm.DeleteSnapshot(context.WithoutCancel(ctx), dom, snap.ID); derr != nil {
			fmt.Fprintf(os.Stderr, "vmobj: deleting the backup snapshot %s failed: %v; delete it in the platform's UI\n", info.Name, derr)
		}
	}
	finish = func(ok bool) {
		if !ok {
			fmt.Fprintf(os.Stderr, "vmobj: the backup failed, so snapshot %s stays; the next good backup deletes the older ones\n", info.Name)
			return
		}
		if o.KeepSnapshots {
			return
		}
		all, lerr := sm.ListSnapshots(context.WithoutCancel(ctx), dom)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "vmobj: listing snapshots to remove the old ones failed: %v\n", lerr)
			return
		}
		for _, s := range all {
			if s.ID == snap.ID || !platform.IsBackupSnapshot(s.Name, s.Description) {
				continue
			}
			if derr := sm.DeleteSnapshot(context.WithoutCancel(ctx), dom, s.ID); derr != nil {
				fmt.Fprintf(os.Stderr, "vmobj: deleting the old backup snapshot %s failed: %v; delete it in the platform's UI\n", s.Name, derr)
			}
		}
	}
	after, err := plat.Describe(ctx, o.Domain)
	if err != nil {
		release()
		return nil, nil, err
	}
	layers = map[string]string{}
	for _, d := range after.Disks {
		old, ok := tops[d.Target]
		if !ok {
			continue
		}
		if sameFile(d.Path, old) {
			release()
			return nil, nil, fmt.Errorf("disk %s: the platform's snapshot didn't start a new overlay (still %s), so it isn't frozen", d.Target, d.Path)
		}
		chain, err := source.QCOW2Chain(d.Path)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("disk %s: %w", d.Target, err)
		}
		if len(chain) < 2 || !sameFile(chain[1].Path, old) {
			release()
			return nil, nil, fmt.Errorf("disk %s: the new top %s doesn't sit on %s, so vmobj can't tell which layer is frozen", d.Target, d.Path, old)
		}
		layers[d.Target] = old
	}
	for t := range tops {
		if _, ok := layers[t]; !ok {
			release()
			return nil, nil, fmt.Errorf("disk %s disappeared from the VM during the snapshot", t)
		}
	}
	return layers, finish, nil
}

// openVMDisk opens one disk: from its snapshot when the VM runs, directly
// when it is shut off. The format comes from the VM's definition.
func openVMDisk(ctx context.Context, o BackupVMOptions, d vmdef.Disk, live bool, pending map[string]string) (source.Source, error) {
	if live {
		ds := strings.TrimPrefix(d.Path, "/dev/zvol/")
		z, err := source.OpenZFS(ctx, ds, source.ZFSOptions{Runner: o.zfs(), DevDir: o.ZFSDevDir, Snapshot: pending[ds]})
		if err != nil {
			return nil, err
		}
		delete(pending, ds) // the source now owns the snapshot
		return z, nil
	}
	return source.Open(d.Path, d.Format)
}

func uniqueRecordName(ctx context.Context, r *repo.Repo, vm string, now time.Time, k manifest.Kind) (string, error) {
	base := manifest.NewName(now, k)
	name := base
	for i := 2; ; i++ {
		ok, err := r.Backend.Exists(ctx, repo.VMRecordKey(vm, name))
		if err != nil || !ok {
			return name, err
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// RestoreVMOptions describe a whole-VM restore into a folder.
type RestoreVMOptions struct {
	Repo   *repo.Repo
	VM     string
	Record string
	Dir    string // created; must be empty if it exists
	// NewName gives the restored VM a new name and drops its UUID, so it
	// can be defined next to the original. Without it, defining the result
	// replaces the original VM's definition on a host that still has it.
	NewName string
	// Progress, when set, reports each disk's restore as in
	// RestoreOptions.Progress, with the disk's target name (vda, hda...).
	Progress func(target string, phase Phase, done, total int64)
	// Target is the platform the VM will run on next (libvirt, qnap or
	// truenas): RESTORE.txt explains the steps for it. Empty means libvirt.
	Target string
	// History restores each disk's snapshot history too (milestone 9), as
	// the qcow2 overlay chain QNAP Virtualization Station keeps, with a
	// plan for its snapshot records (QNAPPlanFile). QNAP only for now;
	// restore-history rebuilds a single disk's history in a ZFS zvol.
	History bool
	// Create also defines the VM on the platform, through its Manager, on
	// the restored disks, never starting it (QNAP: Virtualization
	// Station). The disks are written as qcow2. With History, the
	// snapshots are registered too. OSType and Bridges fill in what the
	// original's definition can't say for another platform.
	Create  bool
	Manager platform.Manager
	OSType  string
	Bridges []string
}

// RestoreVMStats summarise a whole-VM restore.
type RestoreVMStats struct {
	Definition string // path of the rewritten definition
	Disks      map[string]string
	Files      int
	Record     *manifest.VMRecord
	Resumed    bool // continued an interrupted restore into the same folder
	// Created is the VM Create defined; Registered the snapshots
	// registered for it.
	Created    *platform.VM
	Registered int
	// HistoryPlan is the snapshot plan restore-vm --history wrote, for
	// "vmobj qnap-snapshots".
	HistoryPlan string
}

// RestoreVMMarker is the file restore-vm keeps in its folder until the
// restore is complete. It names the VM and backup, so running the same
// restore again continues it, and nothing else is written over.
const RestoreVMMarker = ".vmobj-restore-vm"

// RestoreVM writes a VM's disks (as raw images), definition, NVRAM and
// TPM state into a folder. It changes nothing on the host's libvirt:
// defining the VM is left to the operator, with instructions written next
// to the files.
func RestoreVM(ctx context.Context, o RestoreVMOptions) (*RestoreVMStats, error) {
	rec, err := o.Repo.GetVMRecord(ctx, o.VM, o.Record)
	if err != nil {
		return nil, err
	}
	if o.NewName != "" {
		if err := manifest.CheckVMName(o.NewName); err != nil {
			return nil, fmt.Errorf("new name: %w", err)
		}
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	// A folder in use by an earlier, interrupted restore of this same
	// backup is continued; any other non-empty folder is refused.
	marker := filepath.Join(dir, RestoreVMMarker)
	want := fmt.Sprintf("vm=%s\nrecord=%s\n", o.VM, o.Record)
	resuming := false
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		got, rerr := os.ReadFile(marker)
		switch {
		case rerr != nil:
			return nil, fmt.Errorf("%s is not empty", dir)
		case string(got) != want:
			return nil, fmt.Errorf("%s holds an unfinished restore of another backup (%s); pick another folder", dir, strings.ReplaceAll(strings.TrimSpace(string(got)), "\n", ", "))
		}
		resuming = true
	}

	// Every disk must be complete and readable before anything is written,
	// so a failure never leaves some disks restored and others not.
	for _, d := range rec.Disks {
		vs, err := Verify(ctx, o.Repo, d.Stream, d.Manifest, false)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Target, err)
		}
		if len(vs.Missing) > 0 {
			return nil, fmt.Errorf("disk %s: %w: %d pieces missing, first: %s", d.Target, ErrIncomplete, len(vs.Missing), vs.Missing[0])
		}
	}
	total := &Readiness{}
	for _, d := range rec.Disks {
		rd, err := CheckReady(ctx, o.Repo, d.Stream, d.Manifest)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Target, err)
		}
		total.merge(rd)
	}
	if !total.AllReady() {
		return nil, &NotReadyError{Readiness: total}
	}
	if err := atomicfile.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := atomicfile.Write(marker, []byte(want), 0o600); err != nil {
		return nil, err
	}

	name := rec.DomainName
	if o.NewName != "" {
		name = o.NewName
	}
	st := &RestoreVMStats{Disks: map[string]string{}, Record: rec}
	changes := vmdef.Changes{Disks: map[string]vmdef.DiskChange{}}
	// QNAP Virtualization Station's disk picker lists only image names it
	// knows, such as .img; raw disk data either way.
	ext := ".raw"
	if o.Target == "qnap" {
		ext = ".img"
	}
	if o.Create {
		switch {
		case o.Target != "qnap":
			return nil, errors.New("restore-vm --create works with Virtualization Station (--platform qnap) for now")
		case o.Manager == nil:
			return nil, errors.New("restore-vm --create needs the platform's API (--qvs-credentials)")
		case o.OSType == "":
			return nil, errors.New("restore-vm --create needs --os-type (Virtualization Station's OS type, such as win11)")
		case resuming:
			return nil, fmt.Errorf("%s holds an unfinished restore; to create the VM, restore into a new folder", dir)
		}
	}
	if o.History {
		if o.Target != "qnap" {
			return nil, fmt.Errorf("restore-vm --history writes QNAP Virtualization Station's layout; for a zvol, use restore-history per disk")
		}
		if resuming {
			return nil, fmt.Errorf("%s holds an unfinished history restore; restore into a new folder (overlays can't be continued part-way)", dir)
		}
		if err := restoreQNAPHistory(ctx, o, rec, dir, name, st, &changes); err != nil {
			return nil, err
		}
		st.HistoryPlan = filepath.Join(dir, QNAPPlanFile)
	}
	for _, d := range rec.Disks {
		if o.History {
			break
		}
		if o.Create {
			// The platform takes qcow2 images (QNAP's does).
			out := filepath.Join(dir, name+"-"+d.Target+".qcow2")
			qo := QCOW2Options{Repo: o.Repo, VM: d.Stream, Manifest: d.Manifest, Output: out, Check: true}
			if o.Progress != nil {
				target := d.Target
				qo.Progress = func(p Phase, done, total int64) { o.Progress(target, p, done, total) }
			}
			if _, err := RestoreQCOW2(ctx, qo); err != nil {
				return nil, fmt.Errorf("disk %s: %w", d.Target, err)
			}
			st.Disks[d.Target] = out
			changes.Disks[d.Target] = vmdef.DiskChange{Path: out, Format: "qcow2", AsFile: d.Type == "block"}
			continue
		}
		out := filepath.Join(dir, name+"-"+d.Target+ext)
		ro := RestoreOptions{Repo: o.Repo, VM: d.Stream, Manifest: d.Manifest, Output: out}
		if fi, err := os.Stat(out); resuming && err == nil && fi.Mode().IsRegular() {
			// Finished by the earlier run: read it back and fix anything
			// that isn't the backup, fetching nothing if it all is.
			ro.Mode, ro.Check = ModeInPlace, true
			st.Resumed = true
		} else if _, err := os.Stat(PartialName(out)); resuming && err == nil {
			st.Resumed = true // Restore continues the partial file
		}
		if o.Progress != nil {
			target := d.Target
			ro.Progress = func(p Phase, done, total int64) { o.Progress(target, p, done, total) }
		}
		if _, err := Restore(ctx, ro); err != nil {
			return nil, fmt.Errorf("disk %s: %w\nRun the same restore-vm again to continue it", d.Target, err)
		}
		st.Disks[d.Target] = out
		changes.Disks[d.Target] = vmdef.DiskChange{Path: out, Format: "raw", AsFile: d.Type == "block"}
	}
	var tpmFiles bool
	for _, f := range rec.Files {
		var p string
		switch f.Role {
		case "nvram":
			p = filepath.Join(dir, f.Name)
			changes.NVRAM = p
		case "tpm":
			p = filepath.Join(dir, "tpm", filepath.FromSlash(f.Name))
			tpmFiles = true
		case "config":
			p = filepath.Join(dir, f.Name)
		default:
			continue
		}
		if err := atomicfile.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := atomicfile.Write(p, f.Data, fs.FileMode(f.Mode)&0o700|0o600); err != nil {
			return nil, err
		}
		st.Files++
	}
	changes.Name, changes.DropUUID = o.NewName, o.NewName != ""
	if tpmFiles {
		// A definition that names its TPM folder (TrueNAS's do) now names
		// the restored one.
		changes.TPMDir = filepath.Join(dir, "tpm")
	}
	def, err := vmdef.Rewrite(rec.Definition, changes)
	if err != nil {
		return nil, err
	}
	st.Definition = filepath.Join(dir, name+".xml")
	if err := atomicfile.Write(st.Definition, def, 0o600); err != nil {
		return nil, err
	}
	if o.Create {
		if err := createRestoredVM(ctx, o, rec, def, name, st); err != nil {
			return st, err
		}
	}
	notes := restoreNotesFor(o.Target, rec, st, name, o.NewName != "", tpmFiles)
	if st.Created != nil {
		head := fmt.Sprintf("vmobj created this VM in Virtualization Station as %q (UUID %s), on the\nrestored disks, and did not start it.", st.Created.Name, st.Created.UUID)
		if st.Registered > 0 {
			head += fmt.Sprintf(" Its %d snapshots are registered.", st.Registered)
		}
		notes = head + "\nThe steps below that create the VM or register snapshots are done; the\nrest (TPM state, NVRAM) still apply.\n\n" + notes
	}
	if err := atomicfile.Write(filepath.Join(dir, "RESTORE.txt"), []byte(notes), 0o600); err != nil {
		return nil, err
	}
	if err := os.Remove(marker); err != nil {
		return nil, err
	}
	return st, nil
}

// merge adds another disk's readiness to a VM-wide total.
func (rd *Readiness) merge(o *Readiness) {
	rd.Objects += o.Objects
	rd.Ready += o.Ready
	rd.Restoring += o.Restoring
	rd.Pending = append(rd.Pending, o.Pending...)
	rd.PendingBytes += o.PendingBytes
	seen := map[policy.Class]bool{}
	for _, c := range rd.Classes {
		seen[c] = true
	}
	for _, c := range o.Classes {
		if !seen[c] {
			rd.Classes = append(rd.Classes, c)
		}
	}
	sort.Slice(rd.Classes, func(i, j int) bool { return rd.Classes[i] < rd.Classes[j] })
}

// restoreNotesFor writes RESTORE.txt for the platform the VM will run on.
func restoreNotesFor(target string, rec *manifest.VMRecord, st *RestoreVMStats, name string, renamed, tpm bool) string {
	var b strings.Builder
	from := rec.Platform
	if from == "" {
		from = "libvirt"
	}
	fmt.Fprintf(&b, "Restored %s from backup %s (%s, taken %s, %s, made on %s).\n\n",
		rec.DomainName, rec.Name, rec.Kind, rec.Created.Local().Format("2006-01-02 15:04"), rec.Consistency, from)
	dir := filepath.Dir(st.Definition)
	var disks []string
	for t := range st.Disks {
		disks = append(disks, t)
	}
	sort.Strings(disks)
	var config string
	for _, f := range rec.Files {
		if f.Role == "config" {
			config = f.Name
		}
	}
	// Libvirt keeps TPM 2.0 state in a tpm2/ subfolder; TrueNAS keeps it
	// directly in the VM's folder. Say where the files go either way.
	flatTPM := true
	for _, f := range rec.Files {
		if f.Role == "tpm" && strings.HasPrefix(f.Name, "tpm2/") {
			flatTPM = false
		}
	}
	tpmInto := func(folder string) string {
		if flatTPM {
			return folder + "/tpm2"
		}
		return folder
	}
	switch target {
	case "qnap":
		b.WriteString("To run it in QNAP Virtualization Station (which manages its own VMs, so\n")
		b.WriteString("nothing is defined from here):\n\n")
		if st.HistoryPlan != "" {
			b.WriteString("1. The disks are already qcow2 overlay chains (one layer per snapshot):\n")
			b.WriteString("   don't convert, move or rename any of these files. Each layer names the\n")
			b.WriteString("   one below it, and the snapshot records will name them all.\n\n")
			b.WriteString("2. In Virtualization Station, create a VM with UEFI firmware")
			if tpm {
				b.WriteString(" and a TPM 2.0 device")
			}
			b.WriteString(",\n   and give it each disk's top layer as its disk image:\n\n")
			for _, t := range disks {
				fmt.Fprintf(&b, "     %s  %s\n", t, st.Disks[t])
			}
			b.WriteString("\n   Use the original's settings (CPU, memory, firmware) from the definition\n")
			fmt.Fprintf(&b, "   %s", filepath.Base(st.Definition))
		} else {
			// Virtualization Station's disk picker lists only qcow2 images, so
			// each raw disk is converted first, with QNAP's own qemu-img.
			b.WriteString("1. Convert each restored disk to qcow2: Virtualization Station's disk\n")
			b.WriteString("   picker lists only .qcow2 files. Use QNAP's own qemu-img:\n\n")
			for _, t := range disks {
				raw := st.Disks[t]
				fmt.Fprintf(&b, "     LD_LIBRARY_PATH=%s %s convert -p -f raw -O qcow2 %s %s\n", platform.QNAPLibDir, platform.QNAPQemuImg, raw, strings.TrimSuffix(raw, filepath.Ext(raw))+".qcow2")
			}
			b.WriteString("\n   Keep the .img until the VM runs: it is the copy checked against the backup.\n\n")
			b.WriteString("2. In Virtualization Station, create a VM with UEFI firmware")
			if tpm {
				b.WriteString(" and a TPM 2.0 device")
			}
			b.WriteString(",\n   and give it the converted disks as its disk images:\n\n")
			for _, t := range disks {
				raw := st.Disks[t]
				fmt.Fprintf(&b, "     %s  %s\n", t, strings.TrimSuffix(raw, filepath.Ext(raw))+".qcow2")
			}
			b.WriteString("\n   Use the original's settings (CPU, memory, firmware) from the definition\n")
			fmt.Fprintf(&b, "   %s", filepath.Base(st.Definition))
			if config != "" {
				fmt.Fprintf(&b, " and %s", config)
			}
		}
		b.WriteString(". Don't start it yet.\n\n")
		step := 3 // the steps after creating the VM are numbered as they're written
		if tpm {
			fmt.Fprintf(&b, "%d. Find the new VM's UUID, then put the TPM state where QNAP keeps it:\n\n", step)
			step++
			fmt.Fprintf(&b, "     %s list --all --uuid\n", platform.QNAPVirsh)
			fmt.Fprintf(&b, "     mkdir -p %s\n", tpmInto(platform.QNAPSwtpmDir+"/<UUID>"))
			fmt.Fprintf(&b, "     cp -a %s/. %s/\n\n", filepath.Join(dir, "tpm"), tpmInto(platform.QNAPSwtpmDir+"/<UUID>"))
		}
		nvram := ""
		for _, f := range rec.Files {
			if f.Role == "nvram" {
				nvram = filepath.Join(dir, f.Name)
			}
		}
		if nvram != "" {
			fmt.Fprintf(&b, "%d. If the original had UEFI settings to keep, copy the restored NVRAM file\n", step)
			step++
			b.WriteString("   over the new VM's while it is stopped. Its path is the <nvram> line of\n")
			fmt.Fprintf(&b, "   `%s dumpxml <UUID>`:\n\n", platform.QNAPVirsh)
			fmt.Fprintf(&b, "     cp %s <that path>\n\n", nvram)
		}
		if st.HistoryPlan != "" {
			fmt.Fprintf(&b, "%d. Register the snapshots, with the VM still shut off. The new VM's\n", step)
			b.WriteString("   definition is .<UUID>.meta/<UUID>.xml in the folder Virtualization\n")
			b.WriteString("   Station made for it. Check first (writes nothing), then add --apply:\n\n")
			fmt.Fprintf(&b, "     vmobj qnap-snapshots --plan %s --definition <that file>\n", st.HistoryPlan)
			fmt.Fprintf(&b, "     vmobj qnap-snapshots --plan %s --definition <that file> --apply\n\n", st.HistoryPlan)
			b.WriteString("   Its Snapshots tab then lists them, with their original names and dates.\n\n")
		}
		if from == "truenas" {
			b.WriteString("Windows activation is tied to the original's hardware identity (its SMBIOS\n")
			fmt.Fprintf(&b, "line is in %s, command_line_args). Virtualization Station may not let\n", config)
			b.WriteString("you set it, so expect Windows to ask for activation on the copy. Keep the\n")
			b.WriteString("copy off the network if the original is still in use.\n")
		}
	case "truenas":
		b.WriteString("To run it on TrueNAS:\n\n")
		b.WriteString("1. Make a zvol per disk, the same size as the original, and restore each\n")
		b.WriteString("   disk straight onto it (faster than copying the .raw file):\n\n")
		for _, t := range disks {
			d := diskOf(rec, t)
			fmt.Fprintf(&b, "     vmobj restore --repo REPO --key-file KEY --vm %s --manifest %s --output /dev/zvol/POOL/ZVOL --mode in-place --check\n", d.Stream, d.Manifest)
		}
		b.WriteString("\n2. Create the VM in the TrueNAS UI (Virtualization) with the original's\n")
		if config != "" {
			fmt.Fprintf(&b, "   settings, which are in %s: CPU mode, cores, memory, firmware\n", config)
			b.WriteString("   (bootloader_ovmf), machine_type, and command_line_args (the SMBIOS line\n")
			b.WriteString("   that keeps Windows activated). Add a DISK device per zvol.\n")
		} else {
			fmt.Fprintf(&b, "   settings from %s.\n", filepath.Base(st.Definition))
		}
		b.WriteString("   Don't start it yet.\n\n")
		b.WriteString("3. TrueNAS names the new VM <ID>_<NAME> in its libvirt. With it stopped,\n")
		b.WriteString("   put the firmware state where TrueNAS keeps it:\n\n")
		for _, f := range rec.Files {
			if f.Role == "nvram" {
				fmt.Fprintf(&b, "     cp %s /var/db/system/vm/nvram/<ID>_<NAME>_VARS.fd\n", filepath.Join(dir, f.Name))
			}
		}
		if tpm {
			src := filepath.Join(dir, "tpm")
			if !flatTPM {
				src = filepath.Join(src, "tpm2")
			}
			b.WriteString("     mkdir -p /var/db/system/vm/tpm/<ID>_<NAME>_tpm_state\n")
			fmt.Fprintf(&b, "     cp -a %s/. /var/db/system/vm/tpm/<ID>_<NAME>_tpm_state/\n", src)
			b.WriteString("     chown -R tss:tss /var/db/system/vm/tpm/<ID>_<NAME>_tpm_state\n")
			b.WriteString("     chmod 700 /var/db/system/vm/tpm/<ID>_<NAME>_tpm_state\n")
		}
		b.WriteString("\n4. Start it.\n")
	default:
		return restoreNotes(rec, st, name, renamed, tpm, flatTPM)
	}
	if tpm {
		b.WriteString("\nWithout the TPM state, Windows asks for the BitLocker recovery key and\n")
		b.WriteString("Windows Hello PINs must be set up again.\n")
		b.WriteString("\nTPM state only moves forward: it loads on the same or a newer libtpms\n")
		b.WriteString("than the one that wrote it, never an older one. If the VM won't start\n")
		b.WriteString("and the VM log shows \"TPM result for CMD_INIT: 0x101\", this host's\n")
		b.WriteString("libtpms is older than the source's (QNAP Virtualization Station's 0.8.8\n")
		b.WriteString("is older than TrueNAS 25.10's 0.9.2). Then the only way to start it is\n")
		b.WriteString("with a fresh TPM: move the copied TPM folder aside and start again.\n")
	}
	return b.String()
}

// diskOf finds a disk of a VM record by its target.
func diskOf(rec *manifest.VMRecord, target string) manifest.VMDisk {
	for _, d := range rec.Disks {
		if d.Target == target {
			return d
		}
	}
	return manifest.VMDisk{}
}

// restoreNotes are the steps for a libvirt host that defines VMs with virsh.
func restoreNotes(rec *manifest.VMRecord, st *RestoreVMStats, name string, renamed, tpm, flatTPM bool) string {
	var b strings.Builder
	from := rec.Platform
	if from == "" {
		from = "libvirt"
	}
	fmt.Fprintf(&b, "Restored %s from backup %s (%s, taken %s, %s, made on %s).\n\n",
		rec.DomainName, rec.Name, rec.Kind, rec.Created.Local().Format("2006-01-02 15:04"), rec.Consistency, from)
	// The name libvirt will see is the one in the definition (TrueNAS's
	// libvirt calls WinGames 1_WinGames).
	defName := rec.DomainName
	if dom, err := vmdef.Parse(rec.Definition); err == nil && dom.Name != "" {
		defName = dom.Name
	}
	fmt.Fprintf(&b, "Disks were restored as raw images; the definition %s points at them.\n", filepath.Base(st.Definition))
	b.WriteString("Nothing has been registered with libvirt. To use this VM:\n\n")
	if !renamed {
		fmt.Fprintf(&b, "WARNING: the definition keeps the original name (%s) and UUID (%s). If this host\n", defName, rec.UUID)
		b.WriteString("still has that VM, defining it REPLACES the original's definition. To run it\n")
		b.WriteString("alongside the original, restore again with --new-name instead.\n\n")
	}
	if dom, err := vmdef.Parse(rec.Definition); err == nil && dom.TPMPath != "" && tpm {
		// The definition names its TPM folder, and Rewrite pointed it at
		// the restored state: nothing to copy.
		fmt.Fprintf(&b, "The definition keeps its TPM state in %s (as restored), so just:\n\n", filepath.Join(filepath.Dir(st.Definition), "tpm"))
		fmt.Fprintf(&b, "     virsh define %s\n\n", st.Definition)
		b.WriteString("swtpm must be able to write that folder (on most hosts, user tss or swtpm).\n")
		return b.String()
	}
	if tpm {
		uuid := rec.UUID
		if renamed {
			uuid = "<the new VM's UUID>"
			b.WriteString("1. Define the VM, then find its new UUID:\n\n")
			fmt.Fprintf(&b, "     virsh define %s\n     virsh domuuid %s\n\n", st.Definition, name)
			b.WriteString("2. Put the TPM state where libvirt expects it (stop the VM first if it started):\n\n")
		} else {
			b.WriteString("1. Put the TPM state where libvirt expects it:\n\n")
		}
		// The record says which platform made it; libvirt's folder is the
		// usual one, QNAP keeps its own.
		swtpm := DefaultSwtpmDir
		if rec.Platform == "qnap" {
			swtpm = platform.QNAPSwtpmDir
		}
		into := swtpm + "/" + uuid
		if flatTPM {
			// TrueNAS keeps the files directly in the folder; libvirt
			// expects them in tpm2/.
			into += "/tpm2"
		}
		fmt.Fprintf(&b, "     mkdir -p %s\n", into)
		fmt.Fprintf(&b, "     cp -a %s/. %s/\n", filepath.Join(filepath.Dir(st.Definition), "tpm"), into)
		if rec.Platform != "qnap" {
			fmt.Fprintf(&b, "     (On QNAP it goes under %s instead.)\n", platform.QNAPSwtpmDir)
		}
		b.WriteString("\n")
		if !renamed {
			fmt.Fprintf(&b, "2. Define the VM:\n\n     virsh define %s\n\n", st.Definition)
		}
		b.WriteString("Without the TPM state, Windows asks for the BitLocker recovery key and Windows\n")
		b.WriteString("Hello PINs must be set up again.\n")
	} else {
		fmt.Fprintf(&b, "     virsh define %s\n", st.Definition)
	}
	return b.String()
}

// backupVMHistory backs up each disk's named snapshots the repository
// lacks, into the disk's own series, oldest first. All disks are listed
// before any is backed up, so a disk that can't be read stops the run
// before anything is written.
func backupVMHistory(ctx context.Context, o BackupVMOptions, plat platform.Platform, dom *platform.VM, disks []vmdef.Disk) (map[string][]HistoryStep, error) {
	lists := map[string][]SnapshotSource{}
	for _, d := range disks {
		snaps, err := diskSnapshots(ctx, o, plat, dom, d)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Target, err)
		}
		lists[d.Target] = snaps
	}
	out := map[string][]HistoryStep{}
	for _, d := range disks {
		target := d.Target
		ho := HistoryOptions{
			BackupOptions: BackupOptions{
				Repo: o.Repo, VM: manifest.DiskStream(o.VM, target), StatePath: diskStatePath(o, target),
				Workers: o.Workers, Profile: o.Profile, Class: o.Class, Now: o.Now,
			},
			Snapshots: lists[target], Hash: o.Hash,
		}
		if o.HistoryStep != nil {
			ho.Step = func(i, n int, s manifest.Snapshot, skipped bool) { o.HistoryStep(target, i, n, s, skipped) }
		}
		if o.Progress != nil {
			ho.Wrap = func(src source.Source) (source.Source, func()) { return o.Progress(target, src) }
		}
		steps, err := BackupHistory(ctx, ho)
		out[target] = steps
		if err != nil {
			return out, fmt.Errorf("disk %s history: %w", target, err)
		}
	}
	return out, nil
}

// diskSnapshots lists one disk's named snapshots: the platform's own when
// it has a way, otherwise a zvol's ZFS snapshots.
func diskSnapshots(ctx context.Context, o BackupVMOptions, plat platform.Platform, dom *platform.VM, d vmdef.Disk) ([]SnapshotSource, error) {
	if s, ok := plat.(platform.Snapshotter); ok {
		list, err := s.DiskSnapshots(ctx, dom, d)
		if !errors.Is(err, platform.ErrZvolSnapshots) {
			return list, err
		}
	}
	zvol, ok := strings.CutPrefix(d.Path, "/dev/zvol/")
	if d.Type != "block" || !ok {
		return nil, fmt.Errorf("%s: --history reads ZFS zvol snapshots; the %s module can't list this disk's snapshots", d.Path, plat.Name())
	}
	snaps, err := source.ListZFSSnapshots(ctx, o.zfs(), zvol)
	if err != nil {
		return nil, err
	}
	var list []SnapshotSource
	for _, sn := range snaps {
		full := zvol + "@" + sn.Name
		list = append(list, SnapshotSource{
			Info: manifest.Snapshot{Name: sn.Name, Created: sn.Created, Description: sn.Description, Platform: plat.Name()},
			Open: func(ctx context.Context) (source.Source, error) {
				return source.OpenZFS(ctx, zvol, source.ZFSOptions{Runner: o.zfs(), DevDir: o.ZFSDevDir, Existing: full})
			},
		})
	}
	return list, nil
}
