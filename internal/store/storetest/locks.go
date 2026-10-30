// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/store"
)

// LockBackend is a backend with Object Lock and version access.
type LockBackend interface {
	store.Backend
	store.OptionPutter
	store.Locker
	store.Versioned
}

// RunLocks checks Object Lock behaviour. newBackend must return an empty
// backend on a bucket created with Object Lock enabled. Locks here last a
// few seconds of real time, so the same test runs against real services.
func RunLocks(t *testing.T, newBackend func(t *testing.T) LockBackend) {
	ctx := context.Background()
	b := newBackend(t)

	on, err := b.LockingEnabled(ctx)
	if err != nil || !on {
		t.Fatalf("LockingEnabled = %v, %v; the test bucket needs Object Lock", on, err)
	}

	// Server clocks and ours differ slightly; allow for it.
	const lock = 4 * time.Second
	until := time.Now().Add(lock).Truncate(time.Second)
	must(t, b.PutWith(ctx, "l/obj", []byte("v1"), store.PutOptions{LockUntil: until}))

	vs, err := b.ListVersions(ctx, "l/")
	must(t, err)
	if len(vs) != 1 || !vs[0].Latest || vs[0].DeleteMarker {
		t.Fatalf("versions after one put: %+v", vs)
	}
	v1 := vs[0].VersionID
	info, err := b.StatVersion(ctx, "l/obj", v1)
	must(t, err)
	if d := info.LockUntil.Sub(until); d < -time.Second || d > time.Second {
		t.Fatalf("lock until %v, want %v", info.LockUntil, until)
	}

	// A locked version can't be deleted, and its lock can't be shortened.
	if err := b.DeleteVersion(ctx, "l/obj", v1); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("DeleteVersion of a locked version: %v, want ErrLocked", err)
	}
	if err := b.ExtendLock(ctx, "l/obj", until.Add(-2*time.Second)); err == nil {
		t.Fatal("a lock was shortened")
	}
	// It can be extended.
	until = until.Add(2 * time.Second)
	must(t, b.ExtendLock(ctx, "l/obj", until))
	info, _ = b.StatVersion(ctx, "l/obj", v1)
	if d := info.LockUntil.Sub(until); d < -time.Second || d > time.Second {
		t.Fatalf("after extending, lock until %v, want %v", info.LockUntil, until)
	}

	// A plain Delete only hides the object; the locked data stays.
	must(t, b.Delete(ctx, "l/obj"))
	if _, err := b.Get(ctx, "l/obj"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after Delete: %v, want ErrNotFound", err)
	}
	vs, _ = b.ListVersions(ctx, "l/")
	var markers, versions int
	for _, v := range vs {
		if v.DeleteMarker {
			markers++
		} else {
			versions++
		}
	}
	if markers != 1 || versions != 1 {
		t.Fatalf("after Delete: %d markers, %d versions; want 1 and 1", markers, versions)
	}

	// Once the lock runs out, every version can be removed for good.
	time.Sleep(time.Until(until) + 1500*time.Millisecond)
	for _, v := range vs {
		must(t, b.DeleteVersion(ctx, v.Key, v.VersionID))
	}
	if vs, _ := b.ListVersions(ctx, "l/"); len(vs) != 0 {
		t.Fatalf("versions left after deleting all: %+v", vs)
	}
}
