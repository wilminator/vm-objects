// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

// Regression tests for problems found in review. Each one failed before
// its fix.

import (
	"bytes"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
)

// A block a kept backup needs, hidden by a delete marker (an ordinary
// delete outside vmobj), must keep its data through a prune.
func TestPruneKeepsLiveDataHiddenByDeleteMarker(t *testing.T) {
	h, srv := newLockedHarness(t)
	disk := make([]byte, 256<<10)
	rand.New(rand.NewSource(5)).Read(disk)
	h.write(disk)
	h.backup(manifest.Full)
	h.clock = h.clock.Add(200 * policy.Day) // only backup: kept by KeepLast, locks run out
	keys, _ := h.repo.Backend.List(h.ctx, "blocks/")
	victim := keys[0]
	if err := h.repo.Backend.Delete(h.ctx, victim); err != nil {
		t.Fatal(err)
	}
	h.clock = h.clock.Add(3 * policy.Day)
	rep, err := Prune(h.ctx, PruneOptions{Repo: h.repo, Now: h.clock, KeepLast: 1, Grace: DefaultGrace, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	real := 0
	for _, v := range srv.Versions("locked", "repo/"+victim) {
		if !v.DeleteMarker {
			real++
		}
	}
	if real != 1 || rep.Hidden != 1 {
		t.Fatalf("hidden live block: %d real versions left, Hidden=%d", real, rep.Hidden)
	}
}

// A TOC left behind after its pack is gone must not be trusted: the next
// incremental stores the extent again instead of pointing at nothing.
func TestIncrementalIgnoresTOCWithoutPack(t *testing.T) {
	h := newHarness(t)
	disk := make([]byte, 128<<10)
	rng := rand.New(rand.NewSource(2))
	rng.Read(disk)
	orig := bytes.Clone(disk)
	h.write(disk)
	h.backup(manifest.Full)
	rng.Read(disk[0 : 4<<10])
	changed := bytes.Clone(disk)
	h.write(disk)
	h.backup(manifest.Incremental)
	h.write(orig)
	h.backup(manifest.Incremental)
	keys, _ := h.repo.Backend.List(h.ctx, "packs/")
	for _, k := range keys {
		if strings.HasSuffix(k, ".pack") {
			h.repo.Backend.Delete(h.ctx, k)
		}
	}
	h.write(changed)
	st := h.backup(manifest.Incremental)
	if st.ExtentsStored != 1 {
		t.Fatalf("stored %d extents; the one in the missing pack should be stored again", st.ExtentsStored)
	}
	vs, err := Verify(h.ctx, h.repo, "win11", st.Manifest, true)
	if err != nil || len(vs.Missing) > 0 {
		t.Fatalf("new incremental: %v %v", err, vs.Missing)
	}
}

// An expired manifest that prune can't delete yet (inside the grace
// period) keeps its data, including an old pack it reuses. A one-day
// retention makes "expired but younger than the 48-hour grace period"
// possible.
func TestPruneKeepsDataOfManifestItCannotDeleteYet(t *testing.T) {
	h, srv := newS3Harness(t)
	srv.Now = func() time.Time { return h.clock }
	p, _ := policy.Lookup("standard")
	r := p.Rules[manifest.Incremental]
	r.Retain = policy.Day
	p.Rules[manifest.Incremental] = r
	h.profile = &p
	disk := make([]byte, 128<<10)
	rand.New(rand.NewSource(3)).Read(disk)
	base := bytes.Clone(disk)
	h.write(disk)
	h.backup(manifest.Full)
	rand.New(rand.NewSource(4)).Read(disk[:4<<10])
	h.write(disk)
	h.backup(manifest.Incremental) // writes pack P with the changed extent

	h.clock = h.clock.Add(3 * policy.Day)
	reuser := h.backup(manifest.Incremental) // same change: reuses old pack P
	h.write(base)
	h.backup(manifest.Incremental) // newest; back to the base, needs no pack

	// The first incremental is expired and old: it goes. The reuser is
	// expired but only 36h old: it stays, so pack P (4 days old) must too.
	h.clock = h.clock.Add(36 * time.Hour)
	rep, err := Prune(h.ctx, PruneOptions{Repo: h.repo, Now: h.clock, KeepLast: 1, Grace: DefaultGrace, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ExpiredManifests) != 2 {
		t.Fatalf("expired %v", rep.ExpiredManifests)
	}
	if left, _ := h.repo.ListManifests(h.ctx, "win11"); !slices.Contains(left, reuser.Manifest) {
		t.Fatalf("the manifest inside the grace period was deleted: %v", left)
	}
	vs, err := Verify(h.ctx, h.repo, "win11", reuser.Manifest, true)
	if err != nil || len(vs.Missing) > 0 {
		t.Fatalf("surviving manifest lost data: %v %v", err, vs.Missing)
	}
}

// Whatever pack a restore resolves an extent to (maybe an older
// incremental pack holding the same extent) must be locked as long as the
// backup that needs it. With a one-day full interval an incremental's
// pack is locked 14+1+1 days, short of a differential's 30+1. Which pack a
// restore picks depends on random pack IDs, so several chains are tried.
func TestPacksARestoreReadsAreLockedLongEnough(t *testing.T) {
	for trial := range 8 {
		h, srv := newLockedHarness(t)
		r := h.profile.Rules[manifest.Full]
		r.Every = policy.Day
		h.profile.Rules[manifest.Full] = r
		disk := make([]byte, 256<<10)
		rand.New(rand.NewSource(int64(trial))).Read(disk)
		h.write(disk)
		h.backup(manifest.Full)
		rand.New(rand.NewSource(int64(100 + trial))).Read(disk[:16<<10])
		h.write(disk)
		h.backup(manifest.Incremental)
		diff := h.backup(manifest.Differential)
		m, _ := h.repo.GetManifest(h.ctx, "win11", diff.Manifest)
		idx, _ := h.repo.ChainIndex(h.ctx, m.Chain)
		need := m.RetainUntil.Add(h.profile.Margin)
		for _, ov := range m.Overlay {
			pack := idx[ov.Ref.Hash].Pack
			if got := srv.Object("locked", "repo/"+pack).LockUntil; got.Before(need) {
				t.Fatalf("trial %d: restore reads %s, locked until %v, but the differential needs %v", trial, pack, got, need)
			}
		}
	}
}
