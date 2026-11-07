// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
)

// scenario makes a full, two incrementals and a second full with its own
// incremental, a day apart, under the standard profile (incrementals kept
// 14 days, fulls 90). It returns the backups' names and disk contents.
func scenario(t *testing.T, h *harness) (names []string, disks [][]byte) {
	t.Helper()
	if h.profile == nil {
		p, _ := policy.Lookup("standard")
		h.profile = &p
	}
	rng := rand.New(rand.NewSource(11))
	disk := make([]byte, 512<<10)
	rng.Read(disk)
	step := func(k manifest.Kind) {
		h.write(disk)
		names = append(names, h.backup(k).Manifest)
		disks = append(disks, bytes.Clone(disk))
		rng.Read(disk[rng.Intn(500)<<10:][:4<<10])
		h.clock = h.clock.Add(policy.Day)
	}
	step(manifest.Full)
	step(manifest.Incremental)
	step(manifest.Incremental)
	step(manifest.Full)
	step(manifest.Incremental)
	return names, disks
}

func TestPruneLocal(t *testing.T) {
	h := newHarness(t)
	names, disks := scenario(t, h)

	// 20 days on, the first three incrementals are past their 14 days;
	// the fulls (90 days) are not.
	now := h.clock.Add(20 * policy.Day)
	opts := PruneOptions{Repo: h.repo, Now: now, KeepLast: 1, Grace: DefaultGrace}

	dry, err := Prune(h.ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"win11/" + names[1], "win11/" + names[2], "win11/" + names[4]}
	if !slices.Equal(dry.ExpiredManifests, want[:2]) || !slices.Equal(dry.KeptExpired, want[2:]) {
		t.Fatalf("expired %v, kept %v", dry.ExpiredManifests, dry.KeptExpired)
	}
	if dry.Deleted != 0 {
		t.Fatal("a dry run deleted something")
	}
	if left, _ := h.repo.ListManifests(h.ctx, "win11"); len(left) != 5 {
		t.Fatalf("a dry run removed manifests: %v", left)
	}

	// The local files were written moments ago in real time, so prune's
	// grace period protects them when Now is the real clock.
	recent := opts
	recent.Now, recent.Apply = time.Now(), true
	got, err := Prune(h.ctx, recent)
	if err != nil {
		t.Fatal(err)
	}
	if got.Deleted != 0 {
		t.Fatalf("deleted %d objects inside the grace period", got.Deleted)
	}

	// Expiry is judged in harness time, and the grace period against the
	// files' real modification times, so use a clock at least 20 days past
	// the harness and at least 2 hours past real time, with a 1 hour grace.
	opts.Apply = true
	opts.Grace = time.Hour
	opts.Now = h.clock.Add(20 * policy.Day)
	if real := time.Now().Add(2 * time.Hour); real.After(opts.Now) {
		opts.Now = real
	}
	got, err = Prune(h.ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ExpiredManifests) != 2 || got.Deleted < 2 {
		t.Fatalf("applied prune: %+v", got)
	}
	left, _ := h.repo.ListManifests(h.ctx, "win11")
	if !slices.Equal(left, []string{names[0], names[3], names[4]}) {
		t.Fatalf("manifests left: %v", left)
	}
	// Everything kept still restores, and verifies with nothing missing.
	for _, i := range []int{0, 3, 4} {
		h.restoreEquals(names[i], disks[i])
		vs, err := Verify(h.ctx, h.repo, "win11", names[i], true)
		if err != nil || len(vs.Missing) > 0 {
			t.Fatalf("verify %s after prune: %v %v", names[i], err, vs.Missing)
		}
	}
	// The first chain's packs only the pruned incrementals used are gone.
	if got.Unreferenced == 0 {
		t.Fatal("no unreferenced data was found")
	}
}

func TestPruneLockedRepo(t *testing.T) {
	h, srv := newLockedHarness(t)
	names, disks := scenario(t, h)

	// Incrementals keep 14 days, and their manifests are locked one more
	// day. The scenario ends on day 5; ten days later (day 15) the first
	// incremental (day 1) is expired, but its manifest is locked until day
	// 16: prune must report it, not remove it.
	// at moves both clocks (prune's and the fake S3's, which decides
	// whether a lock has expired) to d after the scenario ended.
	end := h.clock
	at := func(d time.Duration) PruneOptions {
		h.clock = end.Add(d)
		return PruneOptions{Repo: h.repo, Now: h.clock, KeepLast: 1, Grace: DefaultGrace, Apply: true}
	}
	rep, err := Prune(h.ctx, at(10*policy.Day))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ExpiredManifests) != 1 || rep.Deleted != 0 || len(rep.Locked) != 1 {
		t.Fatalf("prune while locked: expired %v, deleted %d, locked %v", rep.ExpiredManifests, rep.Deleted, rep.Locked)
	}
	if left, _ := h.repo.ListManifests(h.ctx, "win11"); len(left) != 5 {
		t.Fatalf("a locked manifest disappeared: %v", left)
	}

	// Far enough on that every lock on the first chain's data has run out
	// (incremental packs: 14 + 30 + 1 days), but the fulls (90 days) are
	// still kept.
	rep, err = Prune(h.ctx, at(50*policy.Day))
	if err != nil {
		t.Fatal(err)
	}
	left, _ := h.repo.ListManifests(h.ctx, "win11")
	if !slices.Equal(left, []string{names[0], names[3], names[4]}) {
		t.Fatalf("manifests left: %v (report %+v)", left, rep)
	}
	if rep.Deleted == 0 {
		t.Fatal("nothing was deleted once the locks ran out")
	}
	for _, i := range []int{0, 3, 4} {
		h.restoreEquals(names[i], disks[i])
	}
	// No version of a pruned manifest is left behind.
	if vs := srv.Versions("locked", "repo/manifests/win11/"+names[1]+".manifest"); len(vs) != 0 {
		t.Fatalf("pruned manifest still has %d versions", len(vs))
	}
}

func TestPruneKeepsLastBackupOfIdleVM(t *testing.T) {
	h := newHarness(t)
	p, _ := policy.Lookup("standard")
	h.profile = &p
	h.write(make([]byte, 64<<10))
	only := h.backup(manifest.Full)
	rep, err := Prune(h.ctx, PruneOptions{Repo: h.repo, Now: h.clock.Add(400 * policy.Day),
		KeepLast: 1, Grace: time.Hour, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ExpiredManifests) != 0 || len(rep.KeptExpired) != 1 {
		t.Fatalf("report %+v", rep)
	}
	if left, _ := h.repo.ListManifests(h.ctx, "win11"); len(left) != 1 || left[0] != only.Manifest {
		t.Fatalf("the VM's only backup was pruned: %v", left)
	}
}
