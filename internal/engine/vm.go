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

	Virsh libvirt.Runner
	ZFS   source.ZFSRunner // nil: the real zfs command
	// ZFSDevDir is where zvol device nodes appear (default /dev/zvol).
	ZFSDevDir string
	// Quiesce freezes the guest's filesystems (through the QEMU guest
	// agent) while a running VM's disks are snapshotted.
	Quiesce bool
	// SwtpmDir holds emulated TPM state, one folder per VM UUID.
	SwtpmDir string // default /var/lib/libvirt/swtpm
	// Progress, when set, wraps each disk source (for progress output).
	Progress func(target string, s source.Source) (source.Source, func())
}

// DefaultSwtpmDir is where libvirt keeps emulated TPM state.
const DefaultSwtpmDir = "/var/lib/libvirt/swtpm"

// BackupVMStats summarise a whole-VM backup.
type BackupVMStats struct {
	Record      string
	Consistency string
	Disks       map[string]*BackupStats // by target
	Files       int
	Skipped     []string
}

// ErrLiveQCOW2 means a running VM has file-backed disks, which can't be
// snapshotted without changing the VM's disk chain.
var ErrLiveQCOW2 = errors.New("the VM is running and has disk image files; shut it down for the backup " +
	"(vmobj does not modify a running VM's disk chain), or keep its disks on ZFS zvols, which can be snapshotted live")

// BackupVM backs up every disk of a libvirt domain, then its definition,
// NVRAM and TPM state, and writes a VM record last.
func BackupVM(ctx context.Context, o BackupVMOptions) (*BackupVMStats, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SwtpmDir == "" {
		o.SwtpmDir = DefaultSwtpmDir
	}
	state, err := libvirt.DomainState(ctx, o.Virsh, o.Domain)
	if err != nil {
		return nil, err
	}
	xmlDef, err := libvirt.Definition(ctx, o.Virsh, o.Domain)
	if err != nil {
		return nil, err
	}
	dom, err := vmdef.Parse(xmlDef)
	if err != nil {
		return nil, err
	}
	if o.VM == "" {
		o.VM = dom.Name
	}
	if err := manifest.CheckVMName(o.VM); err != nil {
		return nil, fmt.Errorf("VM name: %w (use --vm to choose another)", err)
	}
	live := state != libvirt.ShutOff
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

	// Find every problem before touching anything.
	var zvols []string
	for _, d := range disks {
		if !live {
			continue
		}
		ds, ok := strings.CutPrefix(d.Path, "/dev/zvol/")
		if d.Type != "block" || !ok {
			return nil, fmt.Errorf("disk %s (%s): %w", d.Target, d.Path, ErrLiveQCOW2)
		}
		zvols = append(zvols, ds)
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
	files, err := vmFiles(dom, o.SwtpmDir)
	if err != nil {
		return nil, err
	}

	// Capture the disks.
	started := o.Now().UTC()
	stats.Consistency = "shut off"
	snaps := map[string]string{}
	if live {
		stats.Consistency = "snapshot"
		if snaps, err = snapshotVM(ctx, o, zvols, started); err != nil {
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
		DomainName: dom.Name, UUID: dom.UUID, Definition: xmlDef,
		Consistency: stats.Consistency, Files: files, Skipped: stats.Skipped,
	}
	for _, d := range disks {
		src, err := openVMDisk(ctx, o, d, live, pending)
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
func snapshotVM(ctx context.Context, o BackupVMOptions, zvols []string, now time.Time) (snaps map[string]string, err error) {
	if o.Quiesce {
		if err := libvirt.Freeze(ctx, o.Virsh, o.Domain); err != nil {
			return nil, fmt.Errorf("freezing the guest's filesystems (is the QEMU guest agent running?): %w", err)
		}
		defer func() {
			// Thaw even if the snapshot failed or ctx was cancelled.
			if terr := libvirt.Thaw(context.WithoutCancel(ctx), o.Virsh, o.Domain); terr != nil && err == nil {
				err = fmt.Errorf("thawing the guest's filesystems: %w", terr)
			}
		}()
	}
	return source.SnapshotZvols(ctx, o.zfs(), now, zvols...)
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

// vmFiles reads the NVRAM store and TPM state.
func vmFiles(dom *vmdef.Domain, swtpmDir string) ([]manifest.VMFile, error) {
	var files []manifest.VMFile
	total := 0
	add := func(f manifest.VMFile) error {
		total += len(f.Data)
		if total > manifest.MaxVMFiles {
			return fmt.Errorf("the VM's NVRAM and TPM files exceed %d bytes", manifest.MaxVMFiles)
		}
		files = append(files, f)
		return nil
	}
	if dom.NVRAM != "" {
		data, err := os.ReadFile(dom.NVRAM)
		if err != nil {
			return nil, fmt.Errorf("reading the NVRAM store: %w", err)
		}
		if err := add(manifest.VMFile{Role: "nvram", Name: filepath.Base(dom.NVRAM), Mode: 0o600, Data: data}); err != nil {
			return nil, err
		}
	}
	if dom.TPM {
		dir := filepath.Join(swtpmDir, dom.UUID)
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !e.Type().IsRegular() {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			return add(manifest.VMFile{Role: "tpm", Name: filepath.ToSlash(rel), Mode: uint32(info.Mode().Perm()), Data: data})
		})
		if err != nil {
			return nil, fmt.Errorf("reading the TPM state in %s (set --swtpm-dir if it lives elsewhere): %w", dir, err)
		}
	}
	return files, nil
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
}

// RestoreVMStats summarise a whole-VM restore.
type RestoreVMStats struct {
	Definition string // path of the rewritten definition
	Disks      map[string]string
	Files      int
	Record     *manifest.VMRecord
}

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
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty", dir)
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

	name := rec.DomainName
	if o.NewName != "" {
		name = o.NewName
	}
	st := &RestoreVMStats{Disks: map[string]string{}, Record: rec}
	changes := vmdef.Changes{Disks: map[string]vmdef.DiskChange{}}
	for _, d := range rec.Disks {
		out := filepath.Join(dir, name+"-"+d.Target+".raw")
		ro := RestoreOptions{Repo: o.Repo, VM: d.Stream, Manifest: d.Manifest, Output: out}
		if o.Progress != nil {
			target := d.Target
			ro.Progress = func(p Phase, done, total int64) { o.Progress(target, p, done, total) }
		}
		if _, err := Restore(ctx, ro); err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Target, err)
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
	def, err := vmdef.Rewrite(rec.Definition, changes)
	if err != nil {
		return nil, err
	}
	st.Definition = filepath.Join(dir, name+".xml")
	if err := atomicfile.Write(st.Definition, def, 0o600); err != nil {
		return nil, err
	}
	notes := restoreNotes(rec, st, name, o.NewName != "", tpmFiles)
	if err := atomicfile.Write(filepath.Join(dir, "RESTORE.txt"), []byte(notes), 0o600); err != nil {
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

func restoreNotes(rec *manifest.VMRecord, st *RestoreVMStats, name string, renamed, tpm bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Restored %s from backup %s (%s, taken %s, %s).\n\n",
		rec.DomainName, rec.Name, rec.Kind, rec.Created.Local().Format("2006-01-02 15:04"), rec.Consistency)
	fmt.Fprintf(&b, "Disks were restored as raw images; the definition %s points at them.\n", filepath.Base(st.Definition))
	b.WriteString("Nothing has been registered with libvirt. To use this VM:\n\n")
	if !renamed {
		fmt.Fprintf(&b, "WARNING: the definition keeps the original name (%s) and UUID (%s). If this host\n", rec.DomainName, rec.UUID)
		b.WriteString("still has that VM, defining it REPLACES the original's definition. To run it\n")
		b.WriteString("alongside the original, restore again with --new-name instead.\n\n")
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
		fmt.Fprintf(&b, "     mkdir -p %s/%s\n", DefaultSwtpmDir, uuid)
		fmt.Fprintf(&b, "     cp -a %s/. %s/%s/\n", filepath.Join(filepath.Dir(st.Definition), "tpm"), DefaultSwtpmDir, uuid)
		b.WriteString("     (QNAP keeps it under /QVS/var/lib/libvirt/swtpm instead.)\n\n")
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
