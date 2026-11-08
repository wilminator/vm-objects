// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package source

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An existing snapshot is read through a clone, and only the clone is
// destroyed afterwards: the user's snapshot stays.
func TestZFSExistingSnapshotIsNeverDestroyed(t *testing.T) {
	f := newFakeZFS(t)
	f.datasets["tank/vms/win11@before-update"] = "snapshot"
	o := opts(f)
	o.Existing = "tank/vms/win11@before-update"
	z, err := OpenZFS(context.Background(), "tank/vms/win11", o)
	if err != nil {
		t.Fatal(err)
	}
	if z.Snapshot() != o.Existing {
		t.Fatalf("read from %s", z.Snapshot())
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.datasets["tank/vms/win11@before-update"]; !ok {
		t.Fatal("the existing snapshot was destroyed")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "snapshot ") {
			t.Fatalf("took a snapshot of its own: %v", f.calls)
		}
		if strings.HasPrefix(c, "destroy ") && c != "destroy tank/vms/win11-vmobj-20261103T020000Z-before-update" {
			t.Fatalf("destroyed something other than its clone: %v", f.calls)
		}
	}
}

func TestZFSExistingSnapshotChecks(t *testing.T) {
	f := newFakeZFS(t)
	o := opts(f)
	for _, bad := range []string{"tank/vms/other@x", "tank/vms/win11@has space", "tank/vms/win11@missing"} {
		o.Existing = bad
		if _, err := OpenZFS(context.Background(), "tank/vms/win11", o); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	o.Existing, o.Snapshot = "tank/vms/win11@x", "tank/vms/win11@vmobj-20261103T020000Z"
	if _, err := OpenZFS(context.Background(), "tank/vms/win11", o); err == nil {
		t.Error("both kinds of snapshot were accepted at once")
	}
}

type listZFS struct{ out string }

func (l listZFS) Run(_ context.Context, args ...string) (string, error) { return l.out, nil }

func TestListZFSSnapshots(t *testing.T) {
	out := "tank/vms/win11@first\t1762000000\t-\t-\n" +
		"tank/vms/win11@vmobj-20261103T020000Z\t1762000100\t-\t-\n" +
		"tank/vms/win11@moved\t1762000200\t2025-09-14T03:12:44Z\tbefore the move\n"
	snaps, err := ListZFSSnapshots(context.Background(), listZFS{out}, "tank/vms/win11")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots; vmobj's own must be left out: %+v", len(snaps), snaps)
	}
	if snaps[0].Name != "first" || !snaps[0].Created.Equal(time.Unix(1762000000, 0)) || snaps[0].Description != "" {
		t.Fatalf("first: %+v", snaps[0])
	}
	if snaps[1].Name != "moved" || snaps[1].Created.Format(time.RFC3339) != "2025-09-14T03:12:44Z" || snaps[1].Description != "before the move" {
		t.Fatalf("a recreated snapshot keeps its original date and description: %+v", snaps[1])
	}
	if _, err := ListZFSSnapshots(context.Background(), listZFS{"garbage\n"}, "tank/vms/win11"); err == nil {
		t.Fatal("garbage output was accepted")
	}
}

func TestZFSSnapshotName(t *testing.T) {
	for in, want := range map[string]string{
		"before-update":  "before-update",
		"Before Win 11!": "Before_Win_11_",
		"vmobj-mine":     "_vmobj-mine",
		"":               "unnamed",
		"café":           "caf__",
	} {
		if got := ZFSSnapshotName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
		if err := CheckZFSSnapshotName(ZFSSnapshotName(in)); err != nil {
			t.Errorf("%q: result not accepted: %v", in, err)
		}
	}
}
