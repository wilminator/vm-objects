// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
)

// A cancelled copy stops at the next block and writes no manifest.
// (Found on the QNAP: Ctrl+C was ignored until a 50 GB copy finished.)
func TestCopyStopsWhenCancelled(t *testing.T) {
	h := newHarness(t)
	names, _ := chainScenario(t, h)
	to := destRepo(t, s3fake.New("b"), "b", 7)
	ctx, cancel := context.WithCancel(h.ctx)
	calls := 0
	o := CopyOptions{From: h.repo, To: to, Now: func() time.Time { return h.clock },
		Progress: func(string, Phase, int64, int64) {
			if calls++; calls == 3 {
				cancel() // part way through the base blocks
			}
		}}
	err := CopyManifest(ctx, o, "win11", names[0], &CopyStats{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy returned %v", err)
	}
	if ok, _ := to.Backend.Exists(h.ctx, repo.ManifestKey("win11", names[0])); ok {
		t.Fatal("a cancelled copy wrote its manifest")
	}
}

// Copy reports each phase from 0 to its total.
func TestCopyReportsProgress(t *testing.T) {
	h := newHarness(t)
	names, _ := chainScenario(t, h)
	to := destRepo(t, s3fake.New("b"), "b", 7)
	log := newProgressLog(t)
	var labels []string
	o := CopyOptions{From: h.repo, To: to, Now: func() time.Time { return h.clock },
		Progress: func(name string, p Phase, done, total int64) {
			if len(labels) == 0 || labels[len(labels)-1] != name {
				labels = append(labels, name)
				log = newProgressLog(t)
			}
			log.report(p, done, total)
		}}
	if err := CopyManifest(h.ctx, o, "win11", names[0], &CopyStats{}); err != nil {
		t.Fatal(err)
	}
	m, _ := h.repo.GetManifest(h.ctx, "win11", names[0])
	if log.done[PhaseWriting] != m.DiskSize || log.total[PhaseWriting] != m.DiskSize {
		t.Fatalf("full: ended at %d of %d", log.done[PhaseWriting], log.total[PhaseWriting])
	}
	if err := CopyManifest(h.ctx, o, "win11", names[1], &CopyStats{}); err != nil {
		t.Fatal(err)
	}
	inc, _ := h.repo.GetManifest(h.ctx, "win11", names[1])
	want := int64(len(inc.Overlay)) * int64(inc.ExtentSize)
	if log.total[PhaseChanges] != want || log.done[PhaseChanges] != want {
		t.Fatalf("incremental changes: ended at %d of %d, want %d", log.done[PhaseChanges], log.total[PhaseChanges], want)
	}
	if len(labels) != 2 || labels[0] != "win11/"+names[0] {
		t.Fatalf("labels %v", labels)
	}
}
