// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/atomicfile"
	"github.com/wilminator/vm-objects/internal/libvirt"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// Restoring a VM's snapshot history for QNAP Virtualization Station
// (milestone 9). Virtualization Station keeps a snapshot as a qcow2
// overlay: taking one turns the disk's top file read-only and starts a new
// overlay, <disk>.<epoch>, on top of it. So for each disk, restore-vm
// --history writes:
//
//	NAME-vda.img           the first snapshot, a standalone qcow2 image
//	NAME-vda.<epoch1>      overlay: the second snapshot (what changed since the first)
//	...
//	NAME-vda.qcow2         overlay: the current state, the top layer
//
// The top layer ends in .qcow2 because Virtualization Station's disk
// picker lists only .qcow2 files: the VM is created in its UI with that
// file as the disk, so Virtualization Station records it as the disk's
// top itself and nothing writes to its database. The snapshot records it
// lists snapshots from are prepared here (QNAPPlanFile) and put in place
// by "vmobj qnap-snapshots" once the VM exists.

// QNAPPlanFile is the snapshot plan restore-vm --history leaves in its
// folder for "vmobj qnap-snapshots".
const QNAPPlanFile = "qnap-snapshots.json"

// QNAPPlan lists the snapshot records to create for a restored VM, oldest
// first, and the top layer of each disk (what the VM must be created on).
type QNAPPlan struct {
	VM      string             `json:"vm"`
	Record  string             `json:"record"`
	Tops    map[string]string  `json:"tops"` // disk target -> top layer (.qcow2)
	Records []vmdef.QNAPRecord `json:"records"`
}

// ReadQNAPPlan reads a plan written by restore-vm --history.
func ReadQNAPPlan(path string) (*QNAPPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p QNAPPlan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(p.Tops) == 0 {
		return nil, fmt.Errorf("%s: no disks", path)
	}
	return &p, nil
}

// snapshotKey identifies one snapshot across a VM's disks.
type snapshotKey struct {
	name    string
	created int64
}

// qnapEpochs gives every snapshot found on any disk one epoch, the same on
// all disks: its creation time, nudged a second later when two snapshots
// share a second, since the epoch names the record and the overlays.
func qnapEpochs(histories map[string][]*manifest.Manifest) ([]snapshotKey, map[snapshotKey]int64) {
	seen := map[snapshotKey]bool{}
	var keys []snapshotKey
	for _, hs := range histories {
		for _, m := range hs {
			k := snapshotKey{m.Snapshot.Name, m.Snapshot.Created.Unix()}
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	slices.SortFunc(keys, func(a, b snapshotKey) int {
		if a.created != b.created {
			return int(a.created - b.created)
		}
		if a.name < b.name {
			return -1
		}
		return 1
	})
	epochs := map[snapshotKey]int64{}
	var last int64
	for _, k := range keys {
		e := max(k.created, last+1)
		epochs[k] = e
		last = e
	}
	return keys, epochs
}

// restoreQNAPHistory writes every disk's history as an overlay chain in
// dir and the plan for its snapshot records. It is called by RestoreVM
// instead of the plain disk restore.
func restoreQNAPHistory(ctx context.Context, o RestoreVMOptions, rec *manifest.VMRecord, dir, name string,
	st *RestoreVMStats, changes *vmdef.Changes) error {
	histories := map[string][]*manifest.Manifest{}
	for _, d := range rec.Disks {
		hs, err := HistoryManifests(ctx, o.Repo, d.Stream)
		if err != nil {
			return err
		}
		histories[d.Target] = hs
	}
	keys, epochs := qnapEpochs(histories)
	if len(keys) == 0 {
		return fmt.Errorf("%w (none of the VM's disks has any)", ErrNoHistory)
	}
	overlays := map[snapshotKey]map[string]string{} // snapshot -> disk -> its overlay
	plan := &QNAPPlan{VM: o.VM, Record: rec.Name, Tops: map[string]string{}}
	for _, d := range rec.Disks {
		base := filepath.Join(dir, name+"-"+d.Target)
		hs := histories[d.Target]
		report := func(Phase, int64, int64) {}
		if o.Progress != nil {
			target := d.Target
			report = func(p Phase, done, total int64) { o.Progress(target, p, done, total) }
		}
		// layer i holds hs[i]; the top holds the current backup.
		prev, prevManifest := "", ""
		var prevSize int64
		step := func(manifestName, out string, size int64) error {
			qo := QCOW2Options{Repo: o.Repo, VM: d.Stream, Manifest: manifestName, Output: out, Check: true, Progress: report}
			if prev != "" {
				qo.Backing, qo.BackingFormat = prev, "qcow2"
				if size == prevSize {
					qo.Base = prevManifest // only blocks that differ are read
				}
			}
			if _, err := RestoreQCOW2(ctx, qo); err != nil {
				return fmt.Errorf("disk %s: %s: %w", d.Target, filepath.Base(out), err)
			}
			prev, prevManifest, prevSize = out, manifestName, size
			return nil
		}
		for i, m := range hs {
			out := base + ".img"
			if i > 0 {
				k := snapshotKey{hs[i-1].Snapshot.Name, hs[i-1].Snapshot.Created.Unix()}
				out = base + "." + strconv.FormatInt(epochs[k], 10)
			}
			if err := step(m.Name, out, m.DiskSize); err != nil {
				return err
			}
		}
		top := base + ".qcow2"
		cur, err := o.Repo.GetManifest(ctx, d.Stream, d.Manifest)
		if err != nil {
			return err
		}
		if err := step(d.Manifest, top, cur.DiskSize); err != nil {
			return err
		}
		// Each snapshot's overlay is the layer started on top of it: the
		// next one up, or the top for the newest.
		for i, m := range hs {
			k := snapshotKey{m.Snapshot.Name, m.Snapshot.Created.Unix()}
			ov := top
			if i+1 < len(hs) {
				ov = base + "." + strconv.FormatInt(epochs[k], 10)
			}
			if overlays[k] == nil {
				overlays[k] = map[string]string{}
			}
			overlays[k][d.Target] = ov
		}
		st.Disks[d.Target] = top
		plan.Tops[d.Target] = top
		changes.Disks[d.Target] = vmdef.DiskChange{Path: top, Format: "qcow2", AsFile: d.Type == "block"}
	}
	var parent int64
	for _, k := range keys {
		var desc string
		for _, hs := range histories {
			for _, m := range hs {
				if m.Snapshot.Name == k.name && m.Snapshot.Created.Unix() == k.created && m.Snapshot.Description != "" {
					desc = m.Snapshot.Description
				}
			}
		}
		e := epochs[k]
		_, off := time.Unix(e, 0).In(time.Local).Zone()
		plan.Records = append(plan.Records, vmdef.QNAPRecord{
			Epoch: e, Parent: parent, DisplayName: k.name, Description: desc,
			Overlays: overlays[k], LocalOffset: off,
		})
		parent = e
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, QNAPPlanFile), append(data, '\n'), 0o600)
}

// RegisterQNAPSnapshots puts a plan's snapshot records in place for a VM
// created in Virtualization Station on the restored top layers: into
// .<uuid>.meta/snapshot/ beside the VM's definition, each with a copy of
// that definition (libvirt keeps one in every snapshot). It checks first
// that the definition's disks are the plan's top layers, that the VM is
// not running (when virsh is given; Virtualization Station defines a VM
// in libvirt only while it runs), and that no record would be written
// over. With apply false it only returns the files it would write.
func RegisterQNAPSnapshots(ctx context.Context, plan *QNAPPlan, definition string, virsh libvirt.Runner, apply bool) ([]string, error) {
	data, err := os.ReadFile(definition)
	if err != nil {
		return nil, err
	}
	dom, err := vmdef.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", definition, err)
	}
	for target, top := range plan.Tops {
		i := slices.IndexFunc(dom.Disks, func(d vmdef.Disk) bool { return d.Target == target })
		if i < 0 {
			return nil, fmt.Errorf("the VM has no disk %s; create it with %s as that disk", target, top)
		}
		if !sameFile(dom.Disks[i].Path, top) {
			return nil, fmt.Errorf("the VM's disk %s is %s, not the restored top layer %s; the snapshots belong to that file's chain", target, dom.Disks[i].Path, top)
		}
	}
	if virsh != nil && dom.UUID != "" && libvirt.CheckDomain(dom.UUID) == nil {
		if out, err := virsh.Run(ctx, "domstate", dom.UUID); err == nil {
			if state := strings.TrimSpace(out); state != "shut off" {
				return nil, fmt.Errorf("the VM is %s; shut it down in Virtualization Station first", state)
			}
		}
	}
	dir := filepath.Join(filepath.Dir(definition), "snapshot")
	var paths []string
	for _, r := range plan.Records {
		p := filepath.Join(dir, strconv.FormatInt(r.Epoch, 10)+".xml")
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("%s already exists; nothing was written", p)
		}
		paths = append(paths, p)
	}
	if !apply {
		return paths, nil
	}
	if err := atomicfile.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	domain := stripXMLDecl(data)
	for i, r := range plan.Records {
		rec, err := r.Marshal(domain)
		if err != nil {
			return paths[:i], err
		}
		if err := atomicfile.Write(paths[i], rec, 0o644); err != nil {
			return paths[:i], err
		}
	}
	return paths, nil
}

func stripXMLDecl(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if bytes.HasPrefix(b, []byte("<?xml")) {
		if i := bytes.Index(b, []byte("?>")); i >= 0 {
			return bytes.TrimSpace(b[i+2:])
		}
	}
	return b
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// createRestoredVM defines the restored VM through the platform's API, on
// the restored disks, and registers its snapshot history. It never
// starts it.
func createRestoredVM(ctx context.Context, o RestoreVMOptions, rec *manifest.VMRecord, def []byte, name string, st *RestoreVMStats) error {
	hw, err := vmdef.ParseHardware(def)
	if err != nil {
		return err
	}
	dom, err := vmdef.ParseNoUUID(def) // a new name drops the UUID
	if err != nil {
		return err
	}
	spec := platform.VMSpec{Name: name, OSType: o.OSType, Machine: hw.Machine, Memory: hw.Memory, VCPUs: hw.VCPUs,
		UEFI: hw.UEFI, TPM: dom.TPM, Networks: o.Bridges, Definition: def, DefinitionType: "libvirt"}
	for _, d := range rec.Disks {
		spec.Disks = append(spec.Disks, platform.DiskSpec{Target: d.Target, Path: st.Disks[d.Target], Format: "qcow2", Bus: hw.Buses[d.Target]})
	}
	vm, err := o.Manager.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating the VM: %w (the disks are restored in %s; RESTORE.txt says how to create it by hand)", err, filepath.Dir(st.Definition))
	}
	st.Created = vm
	if st.HistoryPlan == "" {
		return nil
	}
	plan, err := ReadQNAPPlan(st.HistoryPlan)
	if err != nil {
		return err
	}
	if err := libvirt.CheckDomain(vm.UUID); err != nil {
		return err
	}
	// Virtualization Station writes the new VM's definition into
	// .<uuid>.meta beside its disk; the records go next to it.
	defPath := filepath.Join(filepath.Dir(plan.Tops[rec.Disks[0].Target]), "."+vm.UUID+".meta", vm.UUID+".xml")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat(defPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the VM was created, but its definition %s didn't appear; register the snapshots with vmobj qnap-snapshots", defPath)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	paths, err := RegisterQNAPSnapshots(ctx, plan, defPath, nil, true)
	if err != nil {
		return fmt.Errorf("the VM was created, but registering its snapshots failed: %w", err)
	}
	st.Registered = len(paths)
	return nil
}

// createOnly is restore-vm --create-only: the disks are already in dir, so
// only the platform's create step (and the snapshot records) is left.
func createOnly(ctx context.Context, o RestoreVMOptions, rec *manifest.VMRecord, dir, marker, want string, resuming bool) (*RestoreVMStats, error) {
	switch {
	case !o.Create:
		return nil, errors.New("restore-vm --create-only needs --create")
	case o.Target != "qnap":
		return nil, errors.New("restore-vm --create works with Virtualization Station (--platform qnap) for now")
	case o.Manager == nil:
		return nil, errors.New("restore-vm --create needs the platform's API (--qvs-credentials)")
	case o.OSType == "":
		return nil, errors.New("restore-vm --create needs --os-type (Virtualization Station's OS type, such as win11)")
	case !resuming:
		return nil, fmt.Errorf("%s holds no restore of this backup to create the VM from; run restore-vm --create (without --create-only) first", dir)
	}
	name := rec.DomainName
	if o.NewName != "" {
		name = o.NewName
	}
	st := &RestoreVMStats{Disks: map[string]string{}, Record: rec, Resumed: true,
		Definition: filepath.Join(dir, name+".xml")}
	def, err := os.ReadFile(st.Definition)
	if err != nil {
		return nil, fmt.Errorf("%s: the earlier restore didn't get as far as writing the definition; run it again: %w", dir, err)
	}
	if o.History {
		st.HistoryPlan = filepath.Join(dir, QNAPPlanFile)
		plan, err := ReadQNAPPlan(st.HistoryPlan)
		if err != nil {
			return nil, fmt.Errorf("%s: the earlier history restore didn't finish writing its layers; restore into a new folder: %w", dir, err)
		}
		for _, d := range rec.Disks {
			top := plan.Tops[d.Target]
			if top == "" {
				return nil, fmt.Errorf("%s: no layers for disk %s", st.HistoryPlan, d.Target)
			}
			st.Disks[d.Target] = top
		}
	} else {
		for _, d := range rec.Disks {
			st.Disks[d.Target] = filepath.Join(dir, name+"-"+d.Target+".qcow2")
		}
	}
	// Every layer the VM will use must be there and read as a chain.
	for t, top := range st.Disks {
		if _, err := source.QCOW2Chain(top); err != nil {
			return nil, fmt.Errorf("disk %s: the restored layers aren't all there: %w", t, err)
		}
	}
	if err := createRestoredVM(ctx, o, rec, def, name, st); err != nil {
		return st, err
	}
	var tpmFiles bool
	for _, f := range rec.Files {
		tpmFiles = tpmFiles || f.Role == "tpm"
	}
	notes := restoreNotesFor(o.Target, rec, st, name, o.NewName != "", tpmFiles, o.TargetHost)
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
