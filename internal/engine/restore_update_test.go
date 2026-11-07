// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
)

// waypoints makes a full and two incrementals of a disk with a short last
// block and a zero region, and returns the three restore points and the
// disk as it was at each.
func waypoints(t *testing.T, h *harness) (names []string, snaps [][]byte) {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	disk := make([]byte, 1<<20+3000) // 16 blocks of 64 KiB and a short one
	rng.Read(disk[:600<<10])
	rng.Read(disk[len(disk)-3000:])
	h.write(disk)
	names = append(names, h.backup(manifest.Full).Manifest)
	snaps = append(snaps, bytes.Clone(disk))

	// Four 4 KiB extents change: three get new data (one in the zero
	// area), one is zeroed.
	rng.Read(disk[8<<10 : 12<<10])
	rng.Read(disk[100<<10 : 104<<10])
	rng.Read(disk[700<<10 : 701<<10])
	copy(disk[200<<10:204<<10], make([]byte, 4<<10))
	h.write(disk)
	names = append(names, h.backup(manifest.Incremental).Manifest)
	snaps = append(snaps, bytes.Clone(disk))

	// One more extent, in the short last block.
	rng.Read(disk[len(disk)-1000:])
	h.write(disk)
	names = append(names, h.backup(manifest.Incremental).Manifest)
	snaps = append(snaps, bytes.Clone(disk))
	return names, snaps
}

func (h *harness) update(mode Mode, name, out string, check bool) *RestoreStats {
	h.t.Helper()
	st, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: name, Output: out, Mode: mode, Check: check})
	if err != nil {
		h.t.Fatalf("%s restore of %s: %v", mode, name, err)
	}
	return st
}

func fileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s doesn't match the disk it should hold", filepath.Base(path))
	}
}

// The waypoint replay: a target holding one restore point is moved to the
// next, then back, writing only what differs each time.
func TestInPlaceStepsBetweenRestorePoints(t *testing.T) {
	for name, newBackend := range backends {
		t.Run(name, func(t *testing.T) {
			h := newHarnessOn(t, newBackend(t))
			names, snaps := waypoints(t, h)
			out := filepath.Join(h.dir, "target.raw")
			if err := os.WriteFile(out, snaps[0], 0o640); err != nil {
				t.Fatal(err)
			}

			st := h.update(ModeInPlace, names[1], out, true)
			fileEquals(t, out, snaps[1])
			if st.PiecesWritten != 4 || st.Written != 4*(4<<10) {
				t.Fatalf("full -> incremental 1 wrote %d pieces, %d bytes; want 4 extents, 16 KiB", st.PiecesWritten, st.Written)
			}
			if st.BlocksSame != 17-4 || !st.Checked {
				t.Fatalf("blocks left alone = %d (want 13), checked = %v", st.BlocksSame, st.Checked)
			}

			st = h.update(ModeInPlace, names[2], out, false)
			fileEquals(t, out, snaps[2])
			if st.PiecesWritten != 1 || st.Written != 3000 { // the short last extent, written whole
				t.Fatalf("incremental 1 -> 2 wrote %d pieces, %d bytes; want 1 extent of 3000 bytes", st.PiecesWritten, st.Written)
			}
			// The four extents incremental 1 brought are already in place, so
			// they aren't fetched again.
			if st.ExtentsSame != 4 {
				t.Fatalf("newer extents already in place = %d, want 4", st.ExtentsSame)
			}

			st = h.update(ModeInPlace, names[0], out, true)
			fileEquals(t, out, snaps[0])
			if st.PiecesWritten != 5 {
				t.Fatalf("back to the full wrote %d pieces, want 5", st.PiecesWritten)
			}

			st = h.update(ModeInPlace, names[0], out, false)
			if st.Written != 0 || st.BlocksRead != 0 || st.BlocksSame != 17 {
				t.Fatalf("a repeat wrote %d bytes and fetched %d blocks; want nothing", st.Written, st.BlocksRead)
			}
		})
	}
}

func TestInPlaceOverUnrelatedData(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	junk := make([]byte, len(snaps[2]))
	rand.New(rand.NewSource(99)).Read(junk)
	if err := os.WriteFile(out, junk, 0o640); err != nil {
		t.Fatal(err)
	}
	h.update(ModeInPlace, names[2], out, true)
	fileEquals(t, out, snaps[2])
}

func TestInPlaceResizesAFile(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	for _, size := range []int{1000, len(snaps[0]) + 70000} {
		out := filepath.Join(h.dir, "sized.raw")
		if err := os.WriteFile(out, make([]byte, size), 0o640); err != nil {
			t.Fatal(err)
		}
		st := h.update(ModeInPlace, names[0], out, false)
		fileEquals(t, out, snaps[0])
		if !st.Resized || st.ResizedFrom != int64(size) {
			t.Fatalf("resized=%v from=%d, want true from %d", st.Resized, st.ResizedFrom, size)
		}
	}
}

func TestUpdateModesNeedAnExistingTarget(t *testing.T) {
	h := newHarness(t)
	names, _ := waypoints(t, h)
	for _, mode := range []Mode{ModeInPlace, ModeCopy} {
		_, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: names[0], Output: filepath.Join(h.dir, "missing.raw"), Mode: mode})
		if err == nil || !strings.Contains(err.Error(), "--mode rename") {
			t.Fatalf("%s onto a missing file: %v; want a pointer to --mode rename", mode, err)
		}
	}
}

func TestCopyKeepsTheFileAndWritesOnlyChanges(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	if err := os.WriteFile(out, snaps[0], 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	st, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: names[2], Output: out, Mode: ModeCopy, Scratch: scratch, Check: true})
	if err != nil {
		t.Fatal(err)
	}
	fileEquals(t, out, snaps[2])
	after, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("copy mode replaced the file instead of updating it")
	}
	if st.PiecesWritten != 5 || !st.Checked {
		t.Fatalf("wrote %d pieces (want 5), checked=%v", st.PiecesWritten, st.Checked)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Fatalf("the scratch copy was left behind: %v", left)
	}
}

// An in-place restore stopped part-way leaves the target mixed; running it
// again finishes the job.
func TestInPlaceInterruptedThenFinished(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	junk := make([]byte, len(snaps[2]))
	rand.New(rand.NewSource(5)).Read(junk)
	if err := os.WriteFile(out, junk, 0o640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	_, err := Restore(ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: names[2], Output: out, Mode: ModeInPlace,
		Progress: func(p Phase, done, total int64) {
			if p == PhaseUpdating && done >= total/2 {
				cancel()
			}
		}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped restore returned %v, want context.Canceled", err)
	}
	if got, _ := os.ReadFile(out); bytes.Equal(got, snaps[2]) || bytes.Equal(got, junk) {
		t.Fatal("expected a part-updated target after the stop")
	}
	st := h.update(ModeInPlace, names[2], out, true)
	fileEquals(t, out, snaps[2])
	if st.BlocksSame == 0 {
		t.Fatal("the second run redid everything instead of skipping what the first had finished")
	}
}

func TestCheckOnlyWithUpdateModes(t *testing.T) {
	h := newHarness(t)
	names, _ := waypoints(t, h)
	_, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: names[0], Output: filepath.Join(h.dir, "r.raw"), Check: true})
	if err == nil || !strings.Contains(err.Error(), "--check") {
		t.Fatalf("rename with check: %v", err)
	}
	if _, err := ParseMode("sideways"); err == nil {
		t.Fatal("ParseMode accepted a bad mode")
	}
}

// writeDiff coalesces neighbouring changed pieces into one write and
// counts each piece.
func TestWriteDiffCoalesces(t *testing.T) {
	h := newHarness(t)
	f, err := os.Create(filepath.Join(h.dir, "w.raw"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	have := make([]byte, 40)
	want := bytes.Clone(have)
	copy(want[0:20], bytes.Repeat([]byte{1}, 20)) // pieces 0 and 1 of 10 bytes: one write
	want[39] = 9                                  // piece 3, after an unchanged piece 2: a second write
	if err := f.Truncate(40); err != nil {
		t.Fatal(err)
	}
	st := &RestoreStats{}
	if err := writeDiff(h.repo, f, 0, have, want, 10, st); err != nil {
		t.Fatal(err)
	}
	if st.PiecesWritten != 3 || st.Written != 30 || st.Writes != 2 {
		t.Fatalf("wrote %d pieces, %d bytes in %d writes; want 3 pieces, 30 bytes in 2 writes", st.PiecesWritten, st.Written, st.Writes)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, want) {
		t.Fatal("file doesn't hold what was meant")
	}
}
