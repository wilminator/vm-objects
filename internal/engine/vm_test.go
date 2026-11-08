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
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

const testUUID = "11111111-2222-3333-4444-555555555555"

// fakeVirsh answers dumpxml and domstate, and records every call.
type fakeVirsh struct {
	xml    string
	state  string
	calls  []string
	failOn string
}

func (f *fakeVirsh) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args[0])
	if args[0] == f.failOn {
		return "", errors.New("simulated virsh failure")
	}
	switch args[0] {
	case "dumpxml":
		return f.xml, nil
	case "domstate":
		return f.state + "\n", nil
	}
	return "", nil
}

// fakeZFS imitates zfs for zvols whose data is held in memory; clones
// appear as files under devDir, as udev would make them. It shares the
// call log with fakeVirsh so tests can check the order across both.
type fakeZFS struct {
	devDir string
	vols   map[string][]byte
	snaps  map[string][]byte
	log    *[]string
	failOn string
	order  []string // user snapshots, oldest first (see userSnapshot)
}

// userSnapshot takes one of the zvol's own named snapshots, as a person
// would before an update.
func (f *fakeZFS) userSnapshot(full string) {
	vol, _, _ := strings.Cut(full, "@")
	f.snaps[full] = bytes.Clone(f.vols[vol])
	f.order = append(f.order, full)
}

func (f *fakeZFS) Run(_ context.Context, args ...string) (string, error) {
	*f.log = append(*f.log, "zfs "+args[0])
	if args[0] == f.failOn {
		return "", errors.New("simulated zfs failure")
	}
	switch args[0] {
	case "get":
		if _, ok := f.vols[args[len(args)-1]]; ok {
			return "volume\n", nil
		}
		return "", errors.New("no such dataset")
	case "snapshot":
		for _, s := range args[1:] {
			vol, _, _ := strings.Cut(s, "@")
			f.snaps[s] = bytes.Clone(f.vols[vol])
		}
	case "clone":
		snap, clone := args[len(args)-2], args[len(args)-1]
		p := filepath.Join(f.devDir, clone)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, f.snaps[snap], 0o600)
	case "list":
		name := args[len(args)-1]
		if slices.Contains(args, "-p") { // listing a zvol's snapshots, oldest first
			var lines []string
			for i, s := range f.order {
				if strings.HasPrefix(s, name+"@") {
					lines = append(lines, fmt.Sprintf("%s\t%d\t-\t-", s, 1735689600+i*3600))
				}
			}
			return strings.Join(lines, "\n") + "\n", nil
		}
		if strings.Contains(name, "@") {
			if _, ok := f.snaps[name]; !ok {
				return "", errors.New("no such snapshot")
			}
			return name + "\n", nil
		}
		if _, err := os.Stat(filepath.Join(f.devDir, name)); err != nil {
			return "", errors.New("no such dataset")
		}
	case "destroy":
		if strings.Contains(args[1], "@") {
			delete(f.snaps, args[1])
		} else {
			os.Remove(filepath.Join(f.devDir, args[1]))
		}
	}
	return "", nil
}

func domainXML(disks string, nvram string) string {
	nv := ""
	if nvram != "" {
		nv = "<nvram>" + nvram + "</nvram>"
	}
	return fmt.Sprintf(`<domain type='kvm'>
  <name>win11</name>
  <uuid>%s</uuid>
  <os><type>hvm</type>%s</os>
  <devices>
%s
    <tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm>
  </devices>
</domain>
`, testUUID, nv, disks)
}

func fileDisk(target, path string) string {
	return fmt.Sprintf(`    <disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='%s'/><target dev='%s'/></disk>`, path, target)
}

func zvolDisk(target, ds string) string {
	return fmt.Sprintf(`    <disk type='block' device='disk'><driver name='qemu' type='raw'/><source dev='/dev/zvol/%s'/><target dev='%s'/></disk>`, ds, target)
}

type vmHarness struct {
	*harness
	virsh *fakeVirsh
	swtpm string
	nvram string
}

func newVMHarness(t *testing.T) *vmHarness {
	h := newHarness(t)
	p, _ := policy.Lookup("standard")
	h.profile = &p
	v := &vmHarness{harness: h, virsh: &fakeVirsh{state: "shut off"}, swtpm: filepath.Join(h.dir, "swtpm")}
	// TPM state, in libvirt's layout, and an NVRAM store.
	tpmDir := filepath.Join(v.swtpm, testUUID, "tpm2")
	os.MkdirAll(tpmDir, 0o700)
	os.WriteFile(filepath.Join(tpmDir, "tpm2-00.permall"), []byte("sealed BitLocker key material"), 0o600)
	v.nvram = filepath.Join(h.dir, "win11_VARS.fd")
	os.WriteFile(v.nvram, bytes.Repeat([]byte{0xEF}, 128<<10), 0o600)
	return v
}

func (v *vmHarness) opts(kind manifest.Kind) BackupVMOptions {
	v.clock = v.clock.Add(time.Hour)
	return BackupVMOptions{Repo: v.repo, Domain: "win11", Kind: kind, Profile: v.profile, Workers: 2,
		StateDir: filepath.Join(v.dir, "state"), Now: func() time.Time { return v.clock },
		Virsh: v.virsh, SwtpmDir: v.swtpm}
}

func randomDisk(t *testing.T, path string, size int, seed int64) []byte {
	t.Helper()
	data := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(data[:size/2])
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBackupVMShutOffAndRestore(t *testing.T) {
	v := newVMHarness(t)
	vda, vdb := filepath.Join(v.dir, "vda.img"), filepath.Join(v.dir, "vdb.img")
	diskA := randomDisk(t, vda, 384<<10, 1)
	diskB := randomDisk(t, vdb, 192<<10, 2)
	v.virsh.xml = domainXML(fileDisk("vda", vda)+"\n"+fileDisk("vdb", vdb), v.nvram)

	full, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	if full.Consistency != "shut off" || len(full.Disks) != 2 || full.Files != 2 {
		t.Fatalf("full: %+v", full)
	}
	fullA := bytes.Clone(diskA)

	// Change one disk; the incremental covers both and stores only the change.
	rand.New(rand.NewSource(3)).Read(diskA[8<<10 : 12<<10])
	os.WriteFile(vda, diskA, 0o600)
	inc, err := BackupVM(v.ctx, v.opts(manifest.Incremental))
	if err != nil {
		t.Fatal(err)
	}
	if inc.Disks["vda"].ExtentsStored != 1 || inc.Disks["vdb"].ExtentsStored != 0 {
		t.Fatalf("incremental stored vda=%d vdb=%d", inc.Disks["vda"].ExtentsStored, inc.Disks["vdb"].ExtentsStored)
	}
	if names, _ := v.repo.ListVMRecords(v.ctx, "win11"); len(names) != 2 {
		t.Fatalf("records %v", names)
	}

	for _, c := range []struct {
		record string
		vda    []byte
	}{{full.Record, fullA}, {inc.Record, diskA}} {
		dir := filepath.Join(v.dir, "restore-"+c.record)
		st, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: c.record, Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(readFile(t, st.Disks["vda"]), c.vda) || !bytes.Equal(readFile(t, st.Disks["vdb"]), diskB) {
			t.Fatalf("%s: restored disks differ", c.record)
		}
		if !bytes.Equal(readFile(t, filepath.Join(dir, "win11_VARS.fd")), readFile(t, v.nvram)) {
			t.Fatal("NVRAM differs")
		}
		if string(readFile(t, filepath.Join(dir, "tpm", "tpm2", "tpm2-00.permall"))) != "sealed BitLocker key material" {
			t.Fatal("TPM state differs")
		}
		d, err := vmdef.Parse(readFile(t, st.Definition))
		if err != nil {
			t.Fatal(err)
		}
		if d.Disks[0].Path != st.Disks["vda"] || d.Disks[1].Path != st.Disks["vdb"] || d.NVRAM != filepath.Join(dir, "win11_VARS.fd") {
			t.Fatalf("definition not pointed at the restore: %+v nvram %s", d.Disks, d.NVRAM)
		}
		notes := string(readFile(t, filepath.Join(dir, "RESTORE.txt")))
		if !strings.Contains(notes, "REPLACES the original") || !strings.Contains(notes, testUUID) {
			t.Fatalf("RESTORE.txt lacks the replace warning or TPM steps:\n%s", notes)
		}
	}

	// A restore into a folder that has files in it is refused.
	busy := filepath.Join(v.dir, "busy")
	os.MkdirAll(busy, 0o700)
	os.WriteFile(filepath.Join(busy, "x"), nil, 0o600)
	if _, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: full.Record, Dir: busy}); err == nil {
		t.Fatal("restored into a non-empty folder")
	}

	// --new-name renames and drops the UUID, so it can sit next to the original.
	dir := filepath.Join(v.dir, "renamed")
	st, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: full.Record, Dir: dir, NewName: "win11-copy"})
	if err != nil {
		t.Fatal(err)
	}
	def := string(readFile(t, st.Definition))
	if !strings.Contains(def, "<name>win11-copy</name>") || strings.Contains(def, "<uuid>") {
		t.Fatalf("renamed definition:\n%s", def)
	}
}

func TestBackupVMRefusesLiveImageFiles(t *testing.T) {
	v := newVMHarness(t)
	vda := filepath.Join(v.dir, "vda.img")
	randomDisk(t, vda, 128<<10, 1)
	v.virsh.xml = domainXML(fileDisk("vda", vda), "")
	v.virsh.state = "running"
	if _, err := BackupVM(v.ctx, v.opts(manifest.Full)); !errors.Is(err, ErrLiveQCOW2) {
		t.Fatalf("live VM with image files: %v", err)
	}
	if keys, _ := v.repo.Backend.List(v.ctx, "manifests/"); len(keys) != 0 {
		t.Fatalf("a refused backup wrote %v", keys)
	}
	if slices.Contains(v.virsh.calls, "domfsfreeze") {
		t.Fatal("froze a guest for a backup that was refused")
	}
}

func TestBackupVMIncrementalNeedsFullFirst(t *testing.T) {
	v := newVMHarness(t)
	vda := filepath.Join(v.dir, "vda.img")
	randomDisk(t, vda, 128<<10, 1)
	v.virsh.xml = domainXML(fileDisk("vda", vda), "")
	if _, err := BackupVM(v.ctx, v.opts(manifest.Incremental)); !errors.Is(err, ErrNeedFull) {
		t.Fatalf("incremental with no full: %v", err)
	}
}

// A running VM on zvols: freeze, one atomic snapshot of every zvol, thaw,
// then read the snapshots while the guest carries on.
func TestBackupVMLiveZvolsQuiesced(t *testing.T) {
	v := newVMHarness(t)
	var log []string
	z := &fakeZFS{devDir: filepath.Join(v.dir, "dev"), vols: map[string][]byte{}, snaps: map[string][]byte{}, log: &log}
	diskA := make([]byte, 256<<10)
	rand.New(rand.NewSource(5)).Read(diskA)
	z.vols["tank/vms/win11-a"] = diskA
	z.vols["tank/vms/win11-b"] = bytes.Repeat([]byte{1}, 128<<10)
	v.virsh.xml = domainXML(zvolDisk("vda", "tank/vms/win11-a")+"\n"+zvolDisk("vdb", "tank/vms/win11-b"), "")
	v.virsh.state = "running"
	logged := &loggingVirsh{inner: v.virsh, log: &log}

	o := v.opts(manifest.Full)
	o.Virsh, o.ZFS, o.ZFSDevDir, o.Quiesce = logged, z, z.devDir, true
	st, err := BackupVM(v.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if st.Consistency != "quiesced snapshot" {
		t.Fatalf("consistency %q", st.Consistency)
	}
	i := slices.Index(log, "virsh domfsfreeze")
	if i < 0 || log[i+1] != "zfs snapshot" || log[i+2] != "virsh domfsthaw" {
		t.Fatalf("expected freeze, one snapshot, thaw in a row; got %v", log)
	}
	if len(z.snaps) != 0 {
		t.Fatalf("snapshots left behind: %v", z.snaps)
	}
	dir := filepath.Join(v.dir, "restored")
	rs, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: st.Record, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, rs.Disks["vda"]), diskA) {
		t.Fatal("restored zvol differs")
	}
	// The zvols became image files in the restored definition.
	d, _ := vmdef.Parse(readFile(t, rs.Definition))
	if d.Disks[0].Type != "file" || d.Disks[0].Path != rs.Disks["vda"] {
		t.Fatalf("restored definition disk %+v", d.Disks[0])
	}

	// If the snapshot fails, the guest is thawed anyway.
	z.failOn = "snapshot"
	o = v.opts(manifest.Full)
	o.Virsh, o.ZFS, o.ZFSDevDir, o.Quiesce = logged, z, z.devDir, true
	log = nil
	if _, err := BackupVM(v.ctx, o); err == nil {
		t.Fatal("backup succeeded although the snapshot failed")
	}
	if !slices.Contains(log, "virsh domfsthaw") {
		t.Fatalf("guest left frozen after a failed snapshot: %v", log)
	}
}

// loggingVirsh records virsh calls in a log shared with the fake zfs.
type loggingVirsh struct {
	inner *fakeVirsh
	log   *[]string
}

func (l *loggingVirsh) Run(ctx context.Context, args ...string) (string, error) {
	*l.log = append(*l.log, "virsh "+args[0])
	return l.inner.Run(ctx, args...)
}

// Prune treats a VM record like a manifest, and a kept record keeps its
// disks' manifests even when they would otherwise go.
func TestPruneVMRecords(t *testing.T) {
	v := newVMHarness(t)
	vda := filepath.Join(v.dir, "vda.img")
	disk := randomDisk(t, vda, 128<<10, 1)
	v.virsh.xml = domainXML(fileDisk("vda", vda), "")
	first, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	v.clock = v.clock.Add(100 * policy.Day)
	rand.New(rand.NewSource(9)).Read(disk[:4096])
	os.WriteFile(vda, disk, 0o600)
	second, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	opts := PruneOptions{Repo: v.repo, Now: v.clock.Add(policy.Day), KeepLast: 1, Grace: time.Hour, Apply: true}
	if real := time.Now().Add(2 * time.Hour); real.After(opts.Now) {
		opts.Now = real
	}
	rep, err := Prune(v.ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rep.ExpiredManifests, "vm record win11/"+first.Record) {
		t.Fatalf("first record not pruned: %v", rep.ExpiredManifests)
	}
	if names, _ := v.repo.ListVMRecords(v.ctx, "win11"); !slices.Equal(names, []string{second.Record}) {
		t.Fatalf("records left: %v", names)
	}
	dir := filepath.Join(v.dir, "after-prune")
	st, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: second.Record, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, st.Disks["vda"]), disk) {
		t.Fatal("the kept VM backup no longer restores")
	}
}

// If any disk of a VM backup is incomplete, restore-vm writes nothing at
// all, rather than restoring the disks before it. (Found in review.)
func TestRestoreVMWritesNothingIfADiskIsIncomplete(t *testing.T) {
	v := newVMHarness(t)
	vda, vdb := filepath.Join(v.dir, "vda.img"), filepath.Join(v.dir, "vdb.img")
	diskA := randomDisk(t, vda, 128<<10, 1)
	diskB := randomDisk(t, vdb, 128<<10, 2)
	v.virsh.xml = domainXML(fileDisk("vda", vda)+"\n"+fileDisk("vdb", vdb), "")
	if _, err := BackupVM(v.ctx, v.opts(manifest.Full)); err != nil {
		t.Fatal(err)
	}
	rand.New(rand.NewSource(7)).Read(diskA[:4096])
	rand.New(rand.NewSource(8)).Read(diskB[:4096])
	os.WriteFile(vda, diskA, 0o600)
	os.WriteFile(vdb, diskB, 0o600)
	inc, err := BackupVM(v.ctx, v.opts(manifest.Incremental))
	if err != nil {
		t.Fatal(err)
	}
	// Lose the pack holding vdb's changed extent: vda (restored first) is
	// fine, vdb is not.
	m, _ := v.repo.GetManifest(v.ctx, "win11.vdb", inc.Disks["vdb"].Manifest)
	packs, _ := v.repo.Backend.List(v.ctx, "packs/"+m.Chain+"/")
	for _, k := range packs {
		if strings.HasSuffix(k, ".pack") {
			v.repo.Backend.Delete(v.ctx, k)
		}
	}
	dir := filepath.Join(v.dir, "partial")
	if _, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: inc.Record, Dir: dir}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("restore with a damaged disk: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restore-vm wrote into the folder before finding the damaged disk")
	}
}

// backup-vm --history backs up each zvol's named snapshots into the
// disk's own series, oldest first, then the VM as usual; a second run
// adds only new snapshots, and the user's snapshots are never destroyed.
func TestBackupVMHistory(t *testing.T) {
	v := newVMHarness(t)
	var log []string
	z := &fakeZFS{devDir: filepath.Join(v.dir, "dev"), vols: map[string][]byte{}, snaps: map[string][]byte{}, log: &log}
	disk := make([]byte, 256<<10)
	rng := rand.New(rand.NewSource(11))
	rng.Read(disk[:128<<10])
	z.vols["tank/vms/win11-a"] = disk
	v.virsh.xml = domainXML(zvolDisk("vda", "tank/vms/win11-a"), "")
	v.virsh.state = "running" // the current state is read from a vmobj snapshot
	var states [][]byte
	for _, name := range []string{"installed", "updated"} {
		rng.Read(disk[(len(states)*40)<<10 : (len(states)*40+8)<<10])
		z.userSnapshot("tank/vms/win11-a@" + name)
		states = append(states, bytes.Clone(disk))
	}

	o := v.opts(manifest.Incremental) // the first history step makes the full
	o.ZFS, o.ZFSDevDir, o.History, o.Hash = z, z.devDir, true, true
	st, err := BackupVM(v.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if h := st.History["vda"]; len(h) != 2 || h[0].Skipped || h[1].Skipped {
		t.Fatalf("history %+v", st.History)
	}
	ms, err := HistoryManifests(v.ctx, v.repo, "win11.vda")
	if err != nil || len(ms) != 2 {
		t.Fatalf("%d history backups of win11.vda, %v", len(ms), err)
	}
	for i, m := range ms {
		if m.Snapshot.Name != []string{"installed", "updated"}[i] || m.Snapshot.Platform != "libvirt" || m.SHA256 != sum(states[i]) {
			t.Fatalf("history %d: %+v sha %s", i, m.Snapshot, m.SHA256)
		}
	}
	for _, name := range []string{"installed", "updated"} {
		if _, ok := z.snaps["tank/vms/win11-a@"+name]; !ok {
			t.Fatalf("the user's snapshot @%s was destroyed", name)
		}
	}

	// One more snapshot: only it is added.
	rng.Read(disk[200<<10 : 204<<10])
	z.userSnapshot("tank/vms/win11-a@patched")
	o = v.opts(manifest.Incremental)
	o.ZFS, o.ZFSDevDir, o.History = z, z.devDir, true
	st, err = BackupVM(v.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if h := st.History["vda"]; len(h) != 3 || !h[0].Skipped || !h[1].Skipped || h[2].Skipped {
		t.Fatalf("second run: %+v", h)
	}

	// The replayed history holds each snapshot exactly.
	tg := newFileTarget(t, v.dir, len(disk))
	if _, err := Replay(v.ctx, ReplayOptions{Repo: v.repo, VM: "win11.vda", Target: tg}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tg.snaps["installed"], states[0]) || !bytes.Equal(tg.snaps["updated"], states[1]) {
		t.Fatal("a replayed snapshot differs from the original")
	}
}

// --history with a disk whose snapshots can't be listed (a raw image)
// stops before anything is written.
func TestBackupVMHistoryNeedsZvolsOrASnapshotter(t *testing.T) {
	v := newVMHarness(t)
	img := filepath.Join(v.dir, "win11.raw")
	randomDisk(t, img, 128<<10, 3)
	v.virsh.xml = domainXML(fileDisk("vda", img), "")
	o := v.opts(manifest.Full)
	o.History = true
	if _, err := BackupVM(v.ctx, o); err == nil || !strings.Contains(err.Error(), "snapshot history needs a qcow2") {
		t.Fatalf("got %v", err)
	}
	if names, _ := v.repo.ListManifests(v.ctx, "win11.vda"); len(names) != 0 {
		t.Fatalf("wrote %v before failing", names)
	}
}
