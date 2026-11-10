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
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	vtarget "github.com/wilminator/vm-objects/internal/target"
)

func qcow2Disk(target, path string) string {
	return fmt.Sprintf(`    <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='%s'/><target dev='%s'/></disk>`, path, target)
}

// fakeVS acts like QNAP Virtualization Station's snapshot API: a snapshot
// starts a new qcow2 overlay on the disk and repoints the definition at
// it, and the running guest's writes go on into the overlay.
type fakeVS struct {
	*platform.Libvirt
	v          *vmHarness
	top        string
	size       int64
	created    []string
	deleted    []string
	failCreate bool
	noOverlay  bool // a broken platform: reports a snapshot but changes nothing
	failAfter  bool // takes the snapshot, then reports a failure
	listed     []platform.SnapshotInfo
	nextID     int
}

func (f *fakeVS) ListSnapshots(context.Context, *platform.VM) ([]platform.SnapshotInfo, error) {
	return f.listed, nil
}
func (f *fakeVS) RevertSnapshot(context.Context, *platform.VM, string) error { return nil }

func (f *fakeVS) CreateSnapshot(_ context.Context, _ *platform.VM, s platform.SnapshotInfo) (platform.SnapshotInfo, error) {
	if f.failCreate {
		return s, errors.New("task failed")
	}
	f.created = append(f.created, s.Name)
	f.nextID++
	s.ID = fmt.Sprint(6 + f.nextID) // "7", "8", ...
	f.listed = append(f.listed, s)
	if f.failAfter {
		return s, errors.New("unexpected reply")
	}
	if f.noOverlay {
		return s, nil
	}
	ov := f.top + ".1794103287"
	setDirty(f.v.t, f.top, false) // the old top goes read-only, and QEMU leaves it clean
	q, err := vtarget.CreateQCOW2(ov, f.size, vtarget.QCOW2Options{Backing: f.top, BackingFormat: "qcow2"})
	if err != nil {
		return s, err
	}
	q.WriteAt(bytes.Repeat([]byte{0xEE}, 8192), 4096) // the guest goes on writing
	if err := q.Close(); err != nil {
		return s, err
	}
	// QEMU holds the live top open with its dirty flag set, which is why
	// the frozen layer under it is the one to read; vmobj may still read
	// the top's header to find that layer.
	setDirty(f.v.t, ov, true)
	f.top = ov
	f.v.virsh.xml = domainXML(qcow2Disk("vda", ov), "")
	return s, nil
}

// setDirty sets or clears a qcow2 v3 image's dirty bit (incompatible
// feature bit 0).
func setDirty(t *testing.T, path string, dirty bool) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var b [8]byte
	if _, err := fh.ReadAt(b[:], 72); err != nil {
		t.Fatal(err)
	}
	if dirty {
		b[7] |= 1
	} else {
		b[7] &^= 1
	}
	if _, err := fh.WriteAt(b[:], 72); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeVS) DeleteSnapshot(_ context.Context, _ *platform.VM, id string) error {
	f.deleted = append(f.deleted, id)
	var keep []platform.SnapshotInfo
	for _, l := range f.listed {
		if l.ID != id {
			keep = append(keep, l)
		}
	}
	f.listed = keep
	return nil
}

func newFakeVS(t *testing.T) (*vmHarness, *fakeVS, []byte) {
	v := newVMHarness(t)
	size := 512 << 10
	disk := make([]byte, size)
	rand.New(rand.NewSource(61)).Read(disk[:300<<10])
	base := filepath.Join(v.dir, "vm-vda.img")
	q, err := vtarget.CreateQCOW2(base, int64(size), vtarget.QCOW2Options{})
	if err != nil {
		t.Fatal(err)
	}
	q.WriteAt(disk, 0)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	v.virsh.xml = domainXML(qcow2Disk("vda", base), "")
	v.virsh.state = "running"
	f := &fakeVS{Libvirt: platform.NewQNAP(v.virsh, v.swtpm), v: v, top: base, size: int64(size)}
	return v, f, disk
}

// A running VM with qcow2 disks is backed up from the layer the platform's
// snapshot froze: the backup is the disk as it was at the snapshot, not
// what the guest wrote after, and the snapshot is deleted afterwards.
func TestBackupRunningVMThroughPlatformSnapshot(t *testing.T) {
	v, f, disk := newFakeVS(t)
	o := v.opts(manifest.Full)
	o.Platform = f
	st, err := BackupVM(v.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	// The first backup leaves its snapshot standing: deleting the newest
	// one wouldn't merge its layer back.
	if st.Consistency != "snapshot" || len(f.created) != 1 || len(f.deleted) != 0 || len(f.listed) != 1 {
		t.Fatalf("consistency %q, created %v, deleted %v, listed %v", st.Consistency, f.created, f.deleted, f.listed)
	}
	if got := f.created[0]; len(got) < len(SnapshotNamePrefix) || got[:len(SnapshotNamePrefix)] != SnapshotNamePrefix {
		t.Fatalf("snapshot named %q", got)
	}
	out := filepath.Join(v.dir, "restored.raw")
	if _, err := Restore(v.ctx, RestoreOptions{Repo: v.repo, VM: "win11.vda", Manifest: st.Disks["vda"].Manifest, Output: out}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, out), disk) {
		t.Fatal("the backup isn't the disk as it was at the snapshot")
	}
}

// When the snapshot can't be taken, nothing is backed up; when the
// platform reports one but nothing froze, the backup refuses and the
// snapshot is still deleted.
func TestBackupRunningVMSnapshotProblems(t *testing.T) {
	v, f, _ := newFakeVS(t)
	f.failCreate = true
	o := v.opts(manifest.Full)
	o.Platform = f
	if _, err := BackupVM(v.ctx, o); err == nil {
		t.Fatal("backed up without a snapshot")
	}
	if names, _ := v.repo.ListManifests(v.ctx, "win11.vda"); len(names) != 0 || len(f.deleted) != 0 {
		t.Fatalf("wrote %v, deleted %v", names, f.deleted)
	}

	v, f, _ = newFakeVS(t)
	f.noOverlay = true
	o = v.opts(manifest.Full)
	o.Platform = f
	if _, err := BackupVM(v.ctx, o); err == nil {
		t.Fatal("backed up a disk that wasn't frozen")
	}
	if len(f.deleted) != 1 || len(f.listed) != 0 {
		t.Fatalf("the snapshot wasn't deleted after refusing: %v", f.deleted)
	}
}

// Without a platform that snapshots, a running VM's image files are still
// refused, as before.
func TestBackupRunningVMWithoutManagerIsRefused(t *testing.T) {
	v, f, _ := newFakeVS(t)
	o := v.opts(manifest.Full)
	o.Platform = f.Libvirt
	if _, err := BackupVM(v.ctx, o); !errors.Is(err, ErrLiveQCOW2) {
		t.Fatalf("got %v", err)
	}
}

// A snapshot step that reports failure but took the snapshot anyway (as
// QNAP once did) leaves nothing behind: vmobj finds it by name and deletes it.
func TestBackupRunningVMCleansUpAFailedSnapshotStep(t *testing.T) {
	v, f, _ := newFakeVS(t)
	f.failAfter = true
	o := v.opts(manifest.Full)
	o.Platform = f
	if _, err := BackupVM(v.ctx, o); err == nil {
		t.Fatal("reported success")
	}
	if len(f.deleted) != 1 || f.deleted[0] != "7" || len(f.listed) != 0 {
		t.Fatalf("the stray snapshot wasn't removed: deleted %v, still listed %v", f.deleted, f.listed)
	}
}

// Each good backup of a running VM takes a new snapshot and then deletes
// vmobj's older ones, so exactly one stands and the layer chain doesn't
// grow. A snapshot that merely looks like vmobj's (no marker) is never
// touched, --keep-snapshots deletes nothing, and --no-snapshots refuses.
func TestBackupRunningVMRotatesItsSnapshot(t *testing.T) {
	v, f, _ := newFakeVS(t)
	f.listed = []platform.SnapshotInfo{
		{ID: "3", Name: "m9-probe", Description: "mine"},
		{ID: "4", Name: "vmobj-lookalike", Description: "named by a person"},
	}
	o := v.opts(manifest.Full)
	o.Platform = f
	if _, err := BackupVM(v.ctx, o); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 || len(f.listed) != 3 {
		t.Fatalf("first backup: deleted %v, listed %v", f.deleted, f.listed)
	}
	o = v.opts(manifest.Incremental)
	o.Platform = f
	o.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := BackupVM(v.ctx, o); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "7" {
		t.Fatalf("second backup should delete the first vmobj snapshot only: %v", f.deleted)
	}
	var ids []string
	for _, l := range f.listed {
		ids = append(ids, l.ID)
	}
	if fmt.Sprint(ids) != "[3 4 8]" {
		t.Fatalf("left %v", ids)
	}
}

func TestBackupRunningVMKeepAndNoSnapshots(t *testing.T) {
	v, f, _ := newFakeVS(t)
	o := v.opts(manifest.Full)
	o.Platform, o.KeepSnapshots = f, true
	if _, err := BackupVM(v.ctx, o); err != nil {
		t.Fatal(err)
	}
	o = v.opts(manifest.Incremental)
	o.Platform, o.KeepSnapshots = f, true
	o.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := BackupVM(v.ctx, o); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 || len(f.listed) != 2 {
		t.Fatalf("--keep-snapshots deleted %v, listed %v", f.deleted, f.listed)
	}

	v, f, _ = newFakeVS(t)
	o = v.opts(manifest.Full)
	o.Platform, o.NoSnapshots = f, true
	if _, err := BackupVM(v.ctx, o); !errors.Is(err, ErrNoSnapshots) {
		t.Fatalf("got %v", err)
	}
	if len(f.created) != 0 {
		t.Fatalf("took a snapshot anyway: %v", f.created)
	}
	o.KeepSnapshots = true
	if _, err := BackupVM(v.ctx, o); err == nil {
		t.Fatal("accepted --keep-snapshots with --no-snapshots")
	}
}
