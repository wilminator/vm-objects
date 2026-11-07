// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
)

// newLockedHarness is a harness on a fake bucket with Object Lock, whose
// clock follows the harness clock so tests can jump months ahead.
func newLockedHarness(t *testing.T) (*harness, *s3fake.Server) {
	t.Helper()
	srv := s3fake.New()
	srv.AddLockedBucket("locked")
	b, err := store.NewS3(srv, "locked", "repo")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOn(t, b, func(c *repo.Config) { c.ObjectLock = true })
	srv.Now = func() time.Time { return h.clock }
	prof, _ := policy.Lookup("standard") // full 90d, diff 30d, inc 14d; full every 30d
	h.profile = &prof
	return h, srv
}

// lockOf returns the current version's lock date.
func lockOf(srv *s3fake.Server, key string) time.Time {
	if o := srv.Object("locked", "repo/"+key); o != nil {
		return o.LockUntil
	}
	return time.Time{}
}

func TestLockedRepoLocksEverything(t *testing.T) {
	h, srv := newLockedHarness(t)
	disk := make([]byte, 512<<10)
	rand.New(rand.NewSource(1)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full)
	started := h.clock

	// Data is locked for retention + one full interval + margin; the
	// manifest for retention + margin; the config like data.
	wantData := started.Add(90*policy.Day + 30*policy.Day + policy.Day)
	blocks, _ := h.repo.Backend.List(h.ctx, "blocks/")
	for _, k := range blocks {
		if got := lockOf(srv, k); !got.Equal(wantData) {
			t.Fatalf("%s locked until %v, want %v", k, got, wantData)
		}
	}
	if got := lockOf(srv, repo.ConfigKey); !got.Equal(wantData) {
		t.Fatalf("config locked until %v, want %v", got, wantData)
	}
	mkey := repo.ManifestKey("win11", full.Manifest)
	if got, want := lockOf(srv, mkey), started.Add(91*policy.Day); !got.Equal(want) {
		t.Fatalf("manifest locked until %v, want %v", got, want)
	}

	// Nothing vmobj can do removes a locked backup: plain deletes only add
	// delete markers, and deleting the version itself is refused.
	b := h.repo.Backend.(*store.S3)
	vs, _ := b.ListVersions(h.ctx, mkey)
	if err := b.DeleteVersion(h.ctx, mkey, vs[0].VersionID); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("deleting a locked manifest version: %v", err)
	}

	// An incremental locks its new packs and TOCs, and relies on this
	// host's cache for the base blocks instead of asking S3 about each.
	rand.New(rand.NewSource(2)).Read(disk[100<<10 : 108<<10])
	h.write(disk)
	heads := srv.Calls["HeadObject"]
	inc := h.backup(manifest.Incremental)
	if n := srv.Calls["HeadObject"] - heads; n > 3 {
		t.Fatalf("incremental made %d HEAD requests; the lock cache should answer", n)
	}
	incStarted := h.clock
	packs, _ := h.repo.Backend.List(h.ctx, "packs/")
	if len(packs) != 2 {
		t.Fatalf("packs/ holds %v", packs)
	}
	for _, k := range packs {
		if got, want := lockOf(srv, k), incStarted.Add(14*policy.Day+31*policy.Day); !got.Equal(want) {
			t.Fatalf("%s locked until %v, want %v", k, got, want)
		}
	}
	if inc.LocksExtended != 0 {
		t.Fatalf("incremental extended %d locks; the full's locks already cover it", inc.LocksExtended)
	}
	h.restoreEquals(inc.Manifest, disk)
}

// When a later backup must outlive the locks it relies on, the locks are
// extended before its manifest is written.
func TestLocksExtendedForLongerRetention(t *testing.T) {
	h, srv := newLockedHarness(t)
	disk := make([]byte, 256<<10)
	rand.New(rand.NewSource(3)).Read(disk)
	h.write(disk)
	h.backup(manifest.Full)
	blocks, _ := h.repo.Backend.List(h.ctx, "blocks/")
	before := lockOf(srv, blocks[0])

	// 100 days later, a differential kept 30 days needs the base blocks
	// until day 131, beyond their day-121 lock.
	h.clock = h.clock.Add(100 * policy.Day)
	rand.New(rand.NewSource(4)).Read(disk[:4<<10])
	h.write(disk)
	diff := h.backup(manifest.Differential)
	if diff.LocksExtended == 0 {
		t.Fatal("no locks were extended")
	}
	want := h.clock.Add(30*policy.Day + 30*policy.Day + policy.Day)
	for _, k := range blocks {
		if got := lockOf(srv, k); !got.Equal(want) {
			t.Fatalf("%s locked until %v (was %v), want %v", k, got, before, want)
		}
	}
	h.restoreEquals(diff.Manifest, disk)
}

// Base blocks older than the refresh age are uploaded again by a full,
// so new fulls never rest on very old objects. The old versions stay
// until their own locks run out.
func TestFullRefreshesOldBlocks(t *testing.T) {
	h, srv := newLockedHarness(t)
	h.profile.RefreshAge = 60 * policy.Day
	disk := make([]byte, 256<<10)
	rand.New(rand.NewSource(5)).Read(disk)
	h.write(disk)
	h.backup(manifest.Full)

	h.clock = h.clock.Add(30 * policy.Day)
	second := h.backup(manifest.Full) // 30 days old: reused as is
	if second.BlocksRefresh != 0 || second.BlocksStored != 0 {
		t.Fatalf("second full refreshed %d, stored %d", second.BlocksRefresh, second.BlocksStored)
	}

	h.clock = h.clock.Add(31 * policy.Day)
	third := h.backup(manifest.Full) // 61 days old: refreshed
	if third.BlocksRefresh != 4 {
		t.Fatalf("third full refreshed %d blocks, want all 4", third.BlocksRefresh)
	}
	blocks, _ := h.repo.Backend.List(h.ctx, "blocks/")
	for _, k := range blocks {
		vs := srv.Versions("locked", "repo/"+k)
		if len(vs) != 2 {
			t.Fatalf("%s has %d versions, want the old and the refreshed one", k, len(vs))
		}
		if !vs[1].Modified.Equal(h.clock) {
			t.Fatalf("%s current version is from %v", k, vs[1].Modified)
		}
	}
	h.restoreEquals(third.Manifest, disk)
}

func TestLockedRepoNeedsProfileAndLockingBucket(t *testing.T) {
	h, _ := newLockedHarness(t)
	h.profile = nil
	h.write(make([]byte, 64<<10))
	if _, err := h.tryBackup(manifest.Full); !errors.Is(err, ErrPolicyNeeded) {
		t.Fatalf("backup without a profile: %v", err)
	}

	plain, _ := store.NewS3(s3fake.New("plain"), "plain", "")
	_, err := repo.Init(h.ctx, plain, keysForTest(t), repo.Config{
		BlockSize: 64 << 10, ExtentSize: 4 << 10, PackTarget: 1 << 20, ObjectLock: true})
	if !errors.Is(err, repo.ErrNoLocking) {
		t.Fatalf("init with Object Lock on a plain bucket: %v", err)
	}
	local, _ := store.NewLocal(t.TempDir())
	_, err = repo.Init(h.ctx, local, keysForTest(t), repo.Config{
		BlockSize: 64 << 10, ExtentSize: 4 << 10, PackTarget: 1 << 20, ObjectLock: true})
	if err == nil || !strings.Contains(err.Error(), "Object Lock") {
		t.Fatalf("init with Object Lock on a local folder: %v", err)
	}
}

func keysForTest(t *testing.T) keys.Set {
	t.Helper()
	k, err := keys.Derive(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}
