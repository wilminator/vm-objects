// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package target

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
)

// fake answers zfs commands from a table of snapshot -> vmobj:created.
type fake struct {
	snaps map[string]string
	calls []string
}

func (f *fake) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	name := args[len(args)-1]
	switch args[0] {
	case "list":
		if _, ok := f.snaps[name]; !ok {
			return "", errors.New("does not exist")
		}
		return name + "\n", nil
	case "get":
		return f.snaps[name] + "\n", nil
	case "snapshot":
		f.snaps[name] = "set"
	}
	return "", nil
}

var when = time.Date(2025, 9, 14, 3, 12, 44, 0, time.UTC)

func TestRecreateKeepsDateNameAndDescription(t *testing.T) {
	f := &fake{snaps: map[string]string{}}
	z := &ZFS{Runner: f, Zvol: "tank/vms/new"}
	err := z.Recreate(context.Background(), manifest.Snapshot{Name: "Before Win 11", Created: when, Description: "clean install"})
	if err != nil {
		t.Fatal(err)
	}
	want := "snapshot -o vmobj:created=2025-09-14T03:12:44Z -o vmobj:name=Before Win 11 -o vmobj:description=clean install tank/vms/new@Before_Win_11"
	if f.calls[0] != want {
		t.Fatalf("ran %q\nwant %q", f.calls[0], want)
	}
	f.calls = nil
	z.Recreate(context.Background(), manifest.Snapshot{Name: "plain", Created: when})
	if f.calls[0] != "snapshot -o vmobj:created=2025-09-14T03:12:44Z tank/vms/new@plain" {
		t.Fatalf("a name ZFS accepts as is needs no vmobj:name: %q", f.calls[0])
	}
}

func TestHas(t *testing.T) {
	f := &fake{snaps: map[string]string{
		"tank/vms/new@done":    "2025-09-14T03:12:44Z",
		"tank/vms/new@other":   "2024-01-01T00:00:00Z",
		"tank/vms/new@foreign": "-",
	}}
	z := &ZFS{Runner: f, Zvol: "tank/vms/new"}
	ctx := context.Background()
	if has, err := z.Has(ctx, manifest.Snapshot{Name: "done", Created: when}); !has || err != nil {
		t.Fatalf("done: %v %v", has, err)
	}
	if has, err := z.Has(ctx, manifest.Snapshot{Name: "missing", Created: when}); has || err != nil {
		t.Fatalf("missing: %v %v", has, err)
	}
	if _, err := z.Has(ctx, manifest.Snapshot{Name: "other", Created: when}); err == nil {
		t.Fatal("a same-named snapshot with another date was taken as this one")
	}
	if _, err := z.Has(ctx, manifest.Snapshot{Name: "foreign", Created: when}); err == nil {
		t.Fatal("a snapshot vmobj didn't make was taken as this one")
	}
}
