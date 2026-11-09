// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/wilminator/vm-objects/internal/target"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// vsLayout builds a VM folder the way Virtualization Station leaves one
// after two snapshots: vm-vda.img <- vm-vda.<e1> <- vm-vda.<e2> (live),
// and a record per snapshot in .<uuid>.meta/snapshot/. It returns the
// folder, the disk and what the disk held at each snapshot.
func vsLayout(t *testing.T) (dir string, disk vmdef.Disk, states [][]byte) {
	t.Helper()
	dir = t.TempDir()
	rng := rand.New(rand.NewSource(4))
	size := 2 << 20
	data := make([]byte, size)
	rng.Read(data[:1<<20])
	layers := []string{"vm-vda.img", "vm-vda.1794103287", "vm-vda.1794200000"}
	epochs := []int64{1794103287, 1794200000}
	meta := filepath.Join(dir, ".uuid-1.meta")
	os.MkdirAll(filepath.Join(meta, "snapshot"), 0o755)
	os.WriteFile(filepath.Join(meta, "uuid-1.xml"), []byte("<domain/>"), 0o600)
	for i, name := range layers {
		o := target.QCOW2Options{}
		if i > 0 {
			o = target.QCOW2Options{Backing: filepath.Join(dir, layers[i-1]), BackingFormat: "qcow2"}
			// The snapshot that created this overlay: the disk as it was.
			states = append(states, bytes.Clone(data))
			rec := fmt.Sprintf(`<domainsnapshot><name>%d</name><description>step %d</description><state>shutoff</state>
<creationTime>%d</creationTime><disks><disk name='vda' snapshot='external'><source file='%s'/></disk></disks></domainsnapshot>`,
				epochs[i-1], i, epochs[i-1], filepath.Join(dir, name))
			os.WriteFile(filepath.Join(meta, "snapshot", fmt.Sprint(epochs[i-1])+".xml"), []byte(rec), 0o600)
			rng.Read(data[i*300<<10 : i*300<<10+5000])
		}
		q, err := target.CreateQCOW2(filepath.Join(dir, name), int64(size), o)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			q.WriteAt(data, 0)
		} else {
			off := i * 300 << 10
			q.WriteAt(data[off:off+5000], int64(off))
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	}
	disk = vmdef.Disk{Target: "vda", Type: "file", Format: "qcow2", Path: filepath.Join(dir, layers[2])}
	return dir, disk, states
}

func TestDiskSnapshotsFromVSRecords(t *testing.T) {
	dir, disk, states := vsLayout(t)
	l := &Libvirt{DefinitionFile: filepath.Join(dir, ".uuid-1.meta", "uuid-1.xml"), name: "qnap"}
	snaps, err := l.DiskSnapshots(context.Background(), &VM{UUID: "uuid-1"}, disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	for i, s := range snaps {
		if s.Info.Name != []string{"1794103287", "1794200000"}[i] || s.Info.Description != fmt.Sprintf("step %d", i+1) || s.Info.Platform != "qnap" {
			t.Fatalf("snapshot %d: %+v", i, s.Info)
		}
		src, err := s.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, src.Size())
		src.ReadAt(got, 0)
		src.Close()
		if !bytes.Equal(got, states[i]) {
			t.Fatalf("snapshot %d doesn't read as the disk was when it was taken", i)
		}
	}
}

// A record whose overlay isn't in the disk's chain (a reverted branch) is
// left out; zvols are left to the engine's ZFS reading.
func TestDiskSnapshotsSkipsOtherBranches(t *testing.T) {
	dir, disk, _ := vsLayout(t)
	meta := filepath.Join(dir, ".uuid-1.meta")
	os.WriteFile(filepath.Join(meta, "snapshot", "1.xml"), []byte(`<domainsnapshot><name>1</name><creationTime>1</creationTime>
<disks><disk name='vda' snapshot='external'><source file='/elsewhere/vm-vda.1'/></disk></disks></domainsnapshot>`), 0o600)
	l := &Libvirt{DefinitionFile: filepath.Join(meta, "uuid-1.xml"), name: "qnap"}
	snaps, err := l.DiskSnapshots(context.Background(), &VM{}, disk)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("%d snapshots, %v", len(snaps), err)
	}
	if _, err := l.DiskSnapshots(context.Background(), &VM{}, vmdef.Disk{Type: "block", Path: "/dev/zvol/tank/x"}); err != ErrZvolSnapshots {
		t.Fatalf("zvol: %v", err)
	}
}

// vmobj's own backup snapshots (name prefix and marker) are not part of a
// VM's history; a person's snapshot named like them is.
func TestDiskSnapshotsLeavesOutVmobjsOwn(t *testing.T) {
	dir, disk, _ := vsLayout(t)
	rec := filepath.Join(dir, ".uuid-1.meta", "snapshot", "1794200000.xml")
	x, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	mine := bytes.Replace(x, []byte("<description>step 2</description>"),
		[]byte("<description>Taken by vmobj for a backup "+BackupSnapshotMarker+".</description>"), 1)
	mine = bytes.Replace(mine, []byte("<creationTime>"), []byte("<displayname>vmobj-20261108T120000Z</displayname><creationTime>"), 1)
	if err := os.WriteFile(rec, mine, 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Libvirt{DefinitionFile: filepath.Join(dir, ".uuid-1.meta", "uuid-1.xml"), name: "qnap"}
	snaps, err := l.DiskSnapshots(context.Background(), &VM{UUID: "uuid-1"}, disk)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("%d snapshots, %v", len(snaps), err)
	}
	// Without the marker the same name stays in the history.
	lookalike := bytes.Replace(mine, []byte(BackupSnapshotMarker), []byte("by a person"), 1)
	os.WriteFile(rec, lookalike, 0o600)
	if snaps, err = l.DiskSnapshots(context.Background(), &VM{UUID: "uuid-1"}, disk); err != nil || len(snaps) != 2 {
		t.Fatalf("%d snapshots, %v", len(snaps), err)
	}
}
