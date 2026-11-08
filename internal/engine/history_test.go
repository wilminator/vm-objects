// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/store"
)

// history makes n snapshot images of one small disk, each changing a few
// extents of the last, and returns them as snapshot sources.
func history(t *testing.T, h *harness, n int) ([]SnapshotSource, [][]byte) {
	t.Helper()
	rng := rand.New(rand.NewSource(9))
	disk := make([]byte, 1<<20+3000)
	rng.Read(disk[:500<<10])
	var list []SnapshotSource
	var images [][]byte
	for i := range n {
		if i > 0 {
			off := (i * 37 << 10) % (len(disk) - 8<<10)
			rng.Read(disk[off : off+5000])
		}
		path := filepath.Join(h.dir, fmt.Sprintf("snap%d.raw", i))
		if err := os.WriteFile(path, disk, 0o600); err != nil {
			t.Fatal(err)
		}
		images = append(images, bytes.Clone(disk))
		list = append(list, SnapshotSource{
			Info: manifest.Snapshot{Name: fmt.Sprintf("step-%d", i), Platform: "test",
				Created: time.Date(2025, 1, 1+i, 9, 30, 0, 0, time.UTC)},
			Open: func(context.Context) (source.Source, error) { return source.OpenRaw(path) },
		})
	}
	return list, images
}

func (h *harness) historyOptions(snaps []SnapshotSource) HistoryOptions {
	return HistoryOptions{
		BackupOptions: BackupOptions{Repo: h.repo, VM: "win11", StatePath: h.state, Workers: 4,
			Now: func() time.Time { h.clock = h.clock.Add(time.Hour); return h.clock }},
		Snapshots: snaps, Hash: true,
	}
}

// fileTarget is a HistoryTarget backed by a plain file: Recreate keeps a
// copy of the file as the "snapshot", so a test can check each one.
type fileTarget struct {
	path  string
	snaps map[string][]byte
	order []string
}

func newFileTarget(t *testing.T, dir string, size int) *fileTarget {
	p := filepath.Join(dir, "target.raw")
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fileTarget{path: p, snaps: map[string][]byte{}}
}

func (f *fileTarget) Output() string { return f.path }
func (f *fileTarget) Has(_ context.Context, s manifest.Snapshot) (bool, error) {
	_, ok := f.snaps[s.Name]
	return ok, nil
}
func (f *fileTarget) Recreate(_ context.Context, s manifest.Snapshot) error {
	b, err := os.ReadFile(f.path)
	f.snaps[s.Name] = b
	f.order = append(f.order, s.Name)
	return err
}

func TestHistoryBacksUpOnlyWhatsNew(t *testing.T) {
	h := newHarness(t)
	snaps, images := history(t, h, 4)

	steps, err := BackupHistory(h.ctx, h.historyOptions(snaps[:3]))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 || steps[0].Stats == nil {
		t.Fatalf("steps %+v", steps)
	}
	kinds := []manifest.Kind{manifest.Full, manifest.Incremental, manifest.Incremental}
	for i, s := range steps {
		m, err := h.repo.GetManifest(h.ctx, "win11", s.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind != kinds[i] || !manifest.SameSnapshot(m.Snapshot, &snaps[i].Info) || m.SHA256 != sum(images[i]) {
			t.Fatalf("step %d: kind %s, snapshot %+v, sha %s", i, m.Kind, m.Snapshot, m.SHA256)
		}
	}

	// Run again with one more snapshot: only the new one is backed up.
	steps, err = BackupHistory(h.ctx, h.historyOptions(snaps))
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range steps {
		if s.Skipped != (i < 3) {
			t.Fatalf("step %d skipped=%v; only the 4th is new", i, s.Skipped)
		}
	}
	ms, err := HistoryManifests(h.ctx, h.repo, "win11")
	if err != nil || len(ms) != 4 {
		t.Fatalf("%d history backups, %v", len(ms), err)
	}
}

// Backups that aren't from named snapshots (the nightly ones) are left
// out of the history, and the history is ordered by when each snapshot was
// taken, not by when it was backed up.
func TestHistoryOrderAndPlainBackups(t *testing.T) {
	h := newHarness(t)
	snaps, _ := history(t, h, 3)
	h.write(make([]byte, 1<<20+3000))
	h.backup(manifest.Full) // a plain backup, no snapshot record
	// Back up the newest first, then the two older ones.
	if _, err := BackupHistory(h.ctx, h.historyOptions(snaps[2:])); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupHistory(h.ctx, h.historyOptions(snaps[:2])); err != nil {
		t.Fatal(err)
	}
	ms, err := HistoryManifests(h.ctx, h.repo, "win11")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("%d history backups; the plain backup must be left out", len(ms))
	}
	for i, m := range ms {
		if m.Snapshot.Name != snaps[i].Info.Name {
			t.Fatalf("position %d is %s; want the snapshots in the order they were taken", i, m.Snapshot.Name)
		}
	}
}

func TestReplayRebuildsEachSnapshot(t *testing.T) {
	for _, mode := range []string{"in-place", "written-only", "staged"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			snaps, images := history(t, h, 4)
			if _, err := BackupHistory(h.ctx, h.historyOptions(snaps)); err != nil {
				t.Fatal(err)
			}
			tg := newFileTarget(t, h.dir, len(images[0]))
			o := ReplayOptions{Repo: h.repo, VM: "win11", Target: tg}
			switch mode {
			case "written-only":
				o.WrittenOnly = true
			case "staged":
				o.Staged = t.TempDir()
			}
			steps, err := Replay(h.ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			for i, s := range snaps {
				if !bytes.Equal(tg.snaps[s.Info.Name], images[i]) {
					t.Fatalf("snapshot %s differs from the original", s.Info.Name)
				}
			}
			for i, s := range steps {
				proven := mode != "written-only" || i == len(steps)-1
				if s.Proven != proven || s.Skipped {
					t.Fatalf("step %d: proven=%v skipped=%v", i, s.Proven, s.Skipped)
				}
			}
			// Later steps only write what changed.
			if w := steps[2].Stats.PiecesWritten; w == 0 || w > 3 {
				t.Fatalf("step 3 wrote %d pieces; want only the few that changed", w)
			}
		})
	}
}

// A replay that stopped part-way continues: the snapshots the target has
// are skipped, and the next step compares the whole disk, since the
// target isn't known to hold any one backup any more.
func TestReplayResumes(t *testing.T) {
	h := newHarness(t)
	snaps, images := history(t, h, 3)
	if _, err := BackupHistory(h.ctx, h.historyOptions(snaps)); err != nil {
		t.Fatal(err)
	}
	tg := newFileTarget(t, h.dir, len(images[0]))
	tg.snaps["step-0"], tg.snaps["step-1"] = images[0], images[1]
	// The interrupted step left the disk half-way to step 2.
	half := bytes.Clone(images[1])
	copy(half[:len(half)/2], images[2])
	if err := os.WriteFile(tg.path, half, 0o600); err != nil {
		t.Fatal(err)
	}
	steps, err := Replay(h.ctx, ReplayOptions{Repo: h.repo, VM: "win11", Target: tg})
	if err != nil {
		t.Fatal(err)
	}
	if !steps[0].Skipped || !steps[1].Skipped || steps[2].Skipped || !steps[2].Proven {
		t.Fatalf("steps %+v", steps)
	}
	if !bytes.Equal(tg.snaps["step-2"], images[2]) || len(tg.order) != 1 {
		t.Fatalf("resumed snapshot differs, or others were recreated again: %v", tg.order)
	}
	if steps[2].Stats.BlocksSkipped != 0 {
		t.Fatal("the first step after a resume skipped blocks without reading them")
	}
}

// A wrong target is caught by the SHA-256 recorded at backup, and nothing
// is snapshotted.
func TestReplayStopsOnAWrongResult(t *testing.T) {
	h := newHarness(t)
	snaps, images := history(t, h, 2)
	if _, err := BackupHistory(h.ctx, h.historyOptions(snaps)); err != nil {
		t.Fatal(err)
	}
	ms, _ := HistoryManifests(h.ctx, h.repo, "win11")
	m := ms[1]
	m.SHA256 = sum([]byte("not the disk"))
	if err := h.repo.PutManifest(h.ctx, m, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	tg := newFileTarget(t, h.dir, len(images[0]))
	if _, err := Replay(h.ctx, ReplayOptions{Repo: h.repo, VM: "win11", Target: tg}); err == nil {
		t.Fatal("a SHA-256 mismatch was accepted")
	}
	if _, ok := tg.snaps["step-1"]; ok || len(tg.order) != 1 {
		t.Fatalf("snapshotted after a failed check: %v", tg.order)
	}
}

func TestReplayWithoutHistory(t *testing.T) {
	h := newHarness(t)
	h.write(make([]byte, 1<<20))
	h.backup(manifest.Full)
	if _, err := Replay(h.ctx, ReplayOptions{Repo: h.repo, VM: "win11", Target: newFileTarget(t, h.dir, 1<<20)}); err != ErrNoHistory {
		t.Fatalf("got %v, want ErrNoHistory", err)
	}
}
