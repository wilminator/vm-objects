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
	"path/filepath"
	"testing"

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
	s.ID = "7"
	if f.failAfter {
		f.listed = append(f.listed, s)
		return s, errors.New("unexpected reply")
	}
	if f.noOverlay {
		return s, nil
	}
	ov := f.top + ".1794103287"
	q, err := vtarget.CreateQCOW2(ov, f.size, vtarget.QCOW2Options{Backing: f.top, BackingFormat: "qcow2"})
	if err != nil {
		return s, err
	}
	q.WriteAt(bytes.Repeat([]byte{0xEE}, 8192), 4096) // the guest goes on writing
	if err := q.Close(); err != nil {
		return s, err
	}
	f.top = ov
	f.v.virsh.xml = domainXML(qcow2Disk("vda", ov), "")
	return s, nil
}

func (f *fakeVS) DeleteSnapshot(_ context.Context, _ *platform.VM, id string) error {
	f.deleted = append(f.deleted, id)
	f.listed = nil
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
	if st.Consistency != "snapshot" || len(f.created) != 1 || len(f.deleted) != 1 || f.deleted[0] != "7" {
		t.Fatalf("consistency %q, created %v, deleted %v", st.Consistency, f.created, f.deleted)
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
	if len(f.deleted) != 1 {
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
