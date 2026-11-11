// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/source"
)

// fakeManager creates VMs the way Virtualization Station does: it writes
// the new VM's definition into .<uuid>.meta beside its first disk.
type fakeManager struct {
	specs []platform.VMSpec
	fail  error // Create fails with this, until it is cleared
}

func (m *fakeManager) State(context.Context, *platform.VM) (string, error) {
	return platform.StateStopped, nil
}
func (m *fakeManager) Start(context.Context, *platform.VM) error { return nil }
func (m *fakeManager) Stop(context.Context, *platform.VM, time.Duration, bool) error {
	return nil
}
func (m *fakeManager) Create(_ context.Context, spec platform.VMSpec) (*platform.VM, error) {
	if m.fail != nil {
		return nil, m.fail
	}
	m.specs = append(m.specs, spec)
	uuid := "11111111-2222-3333-4444-555555555555"
	meta := filepath.Join(filepath.Dir(spec.Disks[0].Path), "."+uuid+".meta")
	os.MkdirAll(meta, 0o755)
	os.WriteFile(filepath.Join(meta, uuid+".xml"), []byte(domainXML(qcow2Disk("vda", spec.Disks[0].Path), "")), 0o600)
	return &platform.VM{Name: spec.Name, UUID: uuid}, nil
}

// restore-vm --create --history: the disks as the QNAP chain, the VM
// created on its top layer with the original's hardware, and the
// snapshots registered beside the new VM's definition.
func TestRestoreVMCreatesTheVM(t *testing.T) {
	v := newVMHarness(t)
	var log []string
	z := &fakeZFS{devDir: filepath.Join(v.dir, "dev"), vols: map[string][]byte{}, snaps: map[string][]byte{}, log: &log}
	disk := make([]byte, 512<<10)
	rng := rand.New(rand.NewSource(71))
	rng.Read(disk[:256<<10])
	z.vols["tank/vms/win11-a"] = disk
	v.virsh.xml = domainXML(zvolDisk("vda", "tank/vms/win11-a"), "")
	v.virsh.state = "running"
	for i, n := range []string{"installed", "updated"} {
		rng.Read(disk[(i*100)<<10 : (i*100+6)<<10])
		z.userSnapshot("tank/vms/win11-a@" + n)
	}
	o := v.opts(manifest.Full)
	o.ZFS, o.ZFSDevDir, o.History = z, z.devDir, true
	bst, err := BackupVM(v.ctx, o)
	if err != nil {
		t.Fatal(err)
	}

	m := &fakeManager{}
	dir := filepath.Join(v.dir, "qnap")
	st, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: bst.Record, Dir: dir, Target: "qnap",
		History: true, Create: true, Manager: m, OSType: "win11", NewName: "win11-restored"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.specs) != 1 || st.Created == nil || st.Registered != 2 {
		t.Fatalf("specs %d, created %+v, registered %d", len(m.specs), st.Created, st.Registered)
	}
	sp := m.specs[0]
	if sp.Name != "win11-restored" || sp.OSType != "win11" || sp.VCPUs < 1 || sp.Memory <= 0 ||
		len(sp.Disks) != 1 || filepath.Base(sp.Disks[0].Path) != "win11-restored-vda.qcow2" || sp.Disks[0].Format != "qcow2" {
		t.Fatalf("spec %+v", sp)
	}
	recs, _ := filepath.Glob(filepath.Join(dir, ".11111111-2222-3333-4444-555555555555.meta", "snapshot", "*.xml"))
	if len(recs) != 2 {
		t.Fatalf("records %v", recs)
	}
	notes, _ := os.ReadFile(filepath.Join(dir, "RESTORE.txt"))
	if !strings.HasPrefix(string(notes), "vmobj created this VM") {
		t.Fatalf("RESTORE.txt:\n%s", notes)
	}
	top, _ := source.Open(sp.Disks[0].Path, "qcow2")
	b := make([]byte, top.Size())
	top.ReadAt(b, 0)
	top.Close()
	if !bytes.Equal(b, disk) {
		t.Fatal("the VM's disk isn't the current state")
	}
}

// Without --history, --create restores each disk as one qcow2 image.
func TestRestoreVMCreateWithoutHistory(t *testing.T) {
	v := newVMHarness(t)
	img := filepath.Join(v.dir, "win11.raw")
	want := randomDisk(t, img, 256<<10, 9)
	v.virsh.xml = domainXML(fileDisk("vda", img), "")
	bst, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeManager{}
	st, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: bst.Record, Dir: filepath.Join(v.dir, "q"),
		Target: "qnap", Create: true, Manager: m, OSType: "win11"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Registered != 0 || len(m.specs) != 1 || !strings.HasSuffix(st.Disks["vda"], ".qcow2") {
		t.Fatalf("%+v %+v", st, m.specs)
	}
	src, _ := source.Open(st.Disks["vda"], "qcow2")
	b := make([]byte, src.Size())
	src.ReadAt(b, 0)
	src.Close()
	if !bytes.Equal(b, want) {
		t.Fatal("the qcow2 disk differs")
	}
	// The refusals: no manager, no OS type, another platform.
	for _, bad := range []RestoreVMOptions{
		{Target: "qnap", Create: true, OSType: "win11"},
		{Target: "qnap", Create: true, Manager: m},
		{Target: "libvirt", Create: true, Manager: m, OSType: "win11"},
	} {
		bad.Repo, bad.VM, bad.Record, bad.Dir = v.repo, "win11", bst.Record, filepath.Join(v.dir, "bad")
		if _, err := RestoreVM(v.ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// A long restore can finish its disks and then fail to create the VM (the
// platform logged vmobj out meanwhile). --create-only then creates the VM
// and registers the snapshots from the folder, without restoring again.
func TestRestoreVMCreateOnly(t *testing.T) {
	v := newVMHarness(t)
	var log []string
	z := &fakeZFS{devDir: filepath.Join(v.dir, "dev"), vols: map[string][]byte{}, snaps: map[string][]byte{}, log: &log}
	disk := make([]byte, 512<<10)
	rng := rand.New(rand.NewSource(72))
	rng.Read(disk[:256<<10])
	z.vols["tank/vms/win11-a"] = disk
	v.virsh.xml = domainXML(zvolDisk("vda", "tank/vms/win11-a"), "")
	v.virsh.state = "running"
	for i, n := range []string{"installed", "updated"} {
		rng.Read(disk[(i*100)<<10 : (i*100+6)<<10])
		z.userSnapshot("tank/vms/win11-a@" + n)
	}
	o := v.opts(manifest.Full)
	o.ZFS, o.ZFSDevDir, o.History = z, z.devDir, true
	bst, err := BackupVM(v.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(v.dir, "qnap")
	ro := RestoreVMOptions{Repo: v.repo, VM: "win11", Record: bst.Record, Dir: dir, Target: "qnap",
		History: true, Create: true, OSType: "win11", NewName: "win11-restored"}

	// Nothing to create from yet.
	ro.CreateOnly, ro.Manager = true, &fakeManager{}
	if _, err := RestoreVM(v.ctx, ro); err == nil {
		t.Fatal("--create-only worked on a folder with no restore")
	}

	// The restore writes everything, then the create fails.
	broken := &fakeManager{fail: errors.New("qvs: not logged in")}
	ro.CreateOnly, ro.Manager = false, broken
	if _, err := RestoreVM(v.ctx, ro); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RestoreVMMarker)); err != nil {
		t.Fatal("the folder should still be marked unfinished")
	}
	// Running it again without --create-only is still refused: no half-redo.
	if _, err := RestoreVM(v.ctx, ro); err == nil {
		t.Fatal("restarted a history restore in a used folder")
	}

	m := &fakeManager{}
	ro.CreateOnly, ro.Manager = true, m
	st, err := RestoreVM(v.ctx, ro)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.specs) != 1 || st.Created == nil || st.Registered != 2 {
		t.Fatalf("specs %d, created %+v, registered %d", len(m.specs), st.Created, st.Registered)
	}
	if _, err := os.Stat(filepath.Join(dir, RestoreVMMarker)); !os.IsNotExist(err) {
		t.Fatal("the marker should be gone once the VM exists")
	}
	notes, _ := os.ReadFile(filepath.Join(dir, "RESTORE.txt"))
	if !strings.HasPrefix(string(notes), "vmobj created this VM") {
		t.Fatalf("RESTORE.txt:\n%s", notes)
	}
	// And it cannot be done twice.
	if _, err := RestoreVM(v.ctx, ro); err == nil {
		t.Fatal("created the VM a second time")
	}
}
