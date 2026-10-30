// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeZFS records zfs commands and imitates a pool with one zvol. Its
// "clone" makes a device file holding the zvol's data, as udev would.
type fakeZFS struct {
	devDir   string
	data     []byte
	calls    []string
	datasets map[string]string // name -> type
	failOn   string            // command verb to fail
}

func (f *fakeZFS) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == f.failOn {
		return "", errors.New("simulated failure")
	}
	switch args[0] {
	case "get":
		t, ok := f.datasets[args[len(args)-1]]
		if !ok {
			return "", errors.New("dataset does not exist")
		}
		return t + "\n", nil
	case "snapshot":
		f.datasets[args[1]] = "snapshot"
	case "clone":
		clone := args[len(args)-1]
		f.datasets[clone] = "volume"
		p := filepath.Join(f.devDir, clone)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, f.data, 0o600)
	case "list":
		if _, ok := f.datasets[args[len(args)-1]]; !ok {
			return "", errors.New("dataset does not exist")
		}
	case "destroy":
		name := args[1]
		if _, ok := f.datasets[name]; !ok {
			return "", errors.New("dataset does not exist")
		}
		delete(f.datasets, name)
		os.Remove(filepath.Join(f.devDir, name))
	}
	return "", nil
}

func newFakeZFS(t *testing.T) *fakeZFS {
	return &fakeZFS{devDir: t.TempDir(), data: []byte(strings.Repeat("zvol data ", 1000)),
		datasets: map[string]string{"tank/vms/win11": "volume", "tank/vms": "filesystem"}}
}

func opts(f *fakeZFS) ZFSOptions {
	return ZFSOptions{Runner: f, DevDir: f.devDir, DeviceWait: time.Second,
		Now: func() time.Time { return time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC) }}
}

func TestZFSSnapshotReadAndCleanup(t *testing.T) {
	f := newFakeZFS(t)
	z, err := OpenZFS(context.Background(), "tank/vms/win11", opts(f))
	if err != nil {
		t.Fatal(err)
	}
	if z.Snapshot() != "tank/vms/win11@vmobj-20261103T020000Z" {
		t.Fatalf("snapshot %s", z.Snapshot())
	}
	got := make([]byte, z.Size())
	if _, err := z.ReadAt(got, 0); err != nil || string(got) != string(f.data) {
		t.Fatalf("read %q, %v", got[:20], err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"get -H -o value type tank/vms/win11",
		"snapshot tank/vms/win11@vmobj-20261103T020000Z",
		"clone -o readonly=on tank/vms/win11@vmobj-20261103T020000Z tank/vms/win11-vmobj-20261103T020000Z",
		"list -H -o name tank/vms/win11-vmobj-20261103T020000Z",
		"destroy tank/vms/win11-vmobj-20261103T020000Z",
		"destroy tank/vms/win11@vmobj-20261103T020000Z",
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("zfs commands:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if len(f.datasets) != 2 {
		t.Fatalf("left behind: %v", f.datasets)
	}
}

func TestZFSKeepSnapshot(t *testing.T) {
	f := newFakeZFS(t)
	o := opts(f)
	o.KeepSnapshot = true
	z, err := OpenZFS(context.Background(), "tank/vms/win11", o)
	if err != nil {
		t.Fatal(err)
	}
	z.Close()
	if f.datasets["tank/vms/win11@vmobj-20261103T020000Z"] != "snapshot" {
		t.Fatal("the snapshot was not kept")
	}
	if _, ok := f.datasets["tank/vms/win11-vmobj-20261103T020000Z"]; ok {
		t.Fatal("the clone was kept")
	}
}

// If cloning fails, the snapshot just taken is removed again.
func TestZFSUndoesOnFailure(t *testing.T) {
	f := newFakeZFS(t)
	f.failOn = "clone"
	if _, err := OpenZFS(context.Background(), "tank/vms/win11", opts(f)); err == nil {
		t.Fatal("expected the clone failure")
	}
	if len(f.datasets) != 2 {
		t.Fatalf("left behind after a failed clone: %v", f.datasets)
	}
}

func TestZFSRefusesNonVolumesAndBadNames(t *testing.T) {
	f := newFakeZFS(t)
	if _, err := OpenZFS(context.Background(), "tank/vms", opts(f)); err == nil || !strings.Contains(err.Error(), "not a zvol") {
		t.Fatalf("filesystem dataset: %v", err)
	}
	for _, bad := range []string{"tank", "/dev/zvol/tank/x", "tank/../etc", "tank/x; rm -rf /", "-o/x", "tank/x@snap"} {
		if _, err := OpenZFS(context.Background(), bad, opts(f)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if len(f.calls) != 1 { // only the "get" for tank/vms
		t.Fatalf("ran commands for bad names: %v", f.calls)
	}
}
