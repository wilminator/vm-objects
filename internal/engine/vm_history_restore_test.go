// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// A VM's history, backed up from zvol snapshots (as on TrueNAS), restored
// as QNAP Virtualization Station's overlay chain: once the records are in
// place, the QNAP module's own reader finds every snapshot with its name,
// date and exact contents, and the top layer holds the current state.
func TestRestoreVMHistoryForQNAP(t *testing.T) {
	v := newVMHarness(t)
	var log []string
	z := &fakeZFS{devDir: filepath.Join(v.dir, "dev"), vols: map[string][]byte{}, snaps: map[string][]byte{}, log: &log}
	disk := make([]byte, 512<<10)
	rng := rand.New(rand.NewSource(51))
	rng.Read(disk[:256<<10])
	z.vols["tank/vms/win11-a"] = disk
	v.virsh.xml = domainXML(zvolDisk("vda", "tank/vms/win11-a"), "")
	v.virsh.state = "running"
	names := []string{"installed", "before update"}
	var states [][]byte
	for i, n := range names {
		rng.Read(disk[(i*100)<<10 : (i*100+6)<<10])
		z.userSnapshot("tank/vms/win11-a@" + source.ZFSSnapshotName(n))
		states = append(states, bytes.Clone(disk))
	}
	rng.Read(disk[300<<10 : 304<<10]) // and the current state differs again
	o := v.opts(manifest.Full)
	o.ZFS, o.ZFSDevDir, o.History, o.Hash = z, z.devDir, true, true
	bst, err := BackupVM(v.ctx, o)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(v.dir, "qnap")
	st, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: bst.Record, Dir: dir, Target: "qnap", History: true})
	if err != nil {
		t.Fatal(err)
	}
	top := st.Disks["vda"]
	if notes, _ := os.ReadFile(filepath.Join(dir, "RESTORE.txt")); os.Getenv("SHOW_NOTES") != "" {
		t.Logf("%s", notes)
	}
	if filepath.Base(top) != "win11-vda.qcow2" {
		t.Fatalf("top layer %s; it must end in .qcow2 for Virtualization Station's picker", top)
	}
	plan, err := ReadQNAPPlan(filepath.Join(dir, QNAPPlanFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Records) != 2 || plan.Tops["vda"] != top {
		t.Fatalf("plan %+v", plan)
	}
	// The layout QNAP uses: <disk>.img at the bottom, one <disk>.<epoch>
	// per later snapshot.
	e0 := strconv.FormatInt(plan.Records[0].Epoch, 10)
	chain, err := source.QCOW2Chain(top)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range chain {
		got = append(got, filepath.Base(l.Path))
	}
	want := []string{"win11-vda.qcow2", "win11-vda." + e0, "win11-vda.img"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("chain %v, want %v", got, want)
	}
	if plan.Records[1].Parent != plan.Records[0].Epoch || plan.Records[0].Overlays["vda"] != filepath.Join(dir, "win11-vda."+e0) {
		t.Fatalf("records %+v", plan.Records)
	}
	if _, err := exec.LookPath("qemu-img"); err == nil {
		if out, err := exec.Command("qemu-img", "check", top).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img check of the chain: %v\n%s", err, out)
		}
	}

	// Register the records with qnap-snapshots' own code, against a
	// definition like the one Virtualization Station writes for a VM
	// created on the top layer, then read the VM the way the QNAP module
	// reads one.
	meta := filepath.Join(dir, ".uuid-1.meta")
	os.MkdirAll(meta, 0o755)
	def := filepath.Join(meta, "uuid-1.xml")
	os.WriteFile(def, []byte("<?xml version='1.0'?>\n"+domainXML(fileDisk("vda", top), "")), 0o600)
	v.virsh.state = "running"
	if _, err := RegisterQNAPSnapshots(v.ctx, plan, def, v.virsh, true); err == nil {
		t.Fatal("registered snapshots while the VM runs")
	}
	v.virsh.state = "shut off"
	paths, err := RegisterQNAPSnapshots(v.ctx, plan, def, v.virsh, false)
	if err != nil || len(paths) != 2 {
		t.Fatalf("dry run: %v %v", paths, err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("the dry run wrote a record")
	}
	if _, err := RegisterQNAPSnapshots(v.ctx, plan, def, v.virsh, true); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterQNAPSnapshots(v.ctx, plan, def, v.virsh, true); err == nil {
		t.Fatal("wrote over existing records")
	}
	rec, _ := os.ReadFile(paths[1])
	if !bytes.Contains(rec, []byte("<name>win11</name>")) || bytes.Contains(rec, []byte("<?xml")) {
		t.Fatalf("record's definition copy:\n%s", rec)
	}
	other := filepath.Join(meta, "other.xml")
	os.WriteFile(other, []byte(domainXML(fileDisk("vda", filepath.Join(dir, "win11-vda.img")), "")), 0o600)
	if _, err := RegisterQNAPSnapshots(v.ctx, plan, other, nil, false); err == nil {
		t.Fatal("accepted a VM whose disk isn't the restored top layer")
	}
	p, err := platform.New("qnap", platform.LibvirtConfig{})
	if err != nil {
		t.Fatal(err)
	}
	l := p.(*platform.Libvirt)
	l.DefinitionFile = def
	snaps, err := l.DiskSnapshots(context.Background(), &platform.VM{}, vmdef.Disk{Target: "vda", Type: "file", Format: "qcow2", Path: top})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots read back", len(snaps))
	}
	for i, s := range snaps {
		orig := bst.History["vda"][i].Snapshot
		if s.Info.Name != source.ZFSSnapshotName(names[i]) || s.Info.Created.Unix() != orig.Created.Unix() {
			t.Fatalf("snapshot %d read back as %q %v, was %q %v", i, s.Info.Name, s.Info.Created, names[i], orig.Created)
		}
		src, err := s.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, src.Size())
		src.ReadAt(b, 0)
		src.Close()
		if !bytes.Equal(b, states[i]) {
			t.Fatalf("snapshot %q's contents differ", names[i])
		}
	}
	cur, _ := source.Open(top, "qcow2")
	b := make([]byte, cur.Size())
	cur.ReadAt(b, 0)
	cur.Close()
	if !bytes.Equal(b, disk) {
		t.Fatal("the top layer isn't the current state")
	}
}
