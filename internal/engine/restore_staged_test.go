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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

func noJournal(t *testing.T, out string) {
	t.Helper()
	if _, err := os.Stat(out + ".vmobj-journal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the journal was left behind (%v)", err)
	}
}

func TestStagedStepsAndCleansUp(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	writeFile(t, out, snaps[0])
	for i, base := range []string{"", names[1]} {
		st, err := h.restoreWith(RestoreOptions{Manifest: names[i+1], Base: base, Output: out, Mode: ModeStaged, ExpectSHA256: sum(snaps[i+1])})
		if err != nil {
			t.Fatal(err)
		}
		fileEquals(t, out, snaps[i+1])
		noJournal(t, out)
		if !st.Checked || st.SHA256 != sum(snaps[i+1]) || st.Applied == 0 || st.Applied != st.JournalBytes || st.Repaired != 0 {
			t.Fatalf("step %d: %+v", i+1, st)
		}
	}
}

func TestStagedLeavesTheTargetAloneUntilProven(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	writeFile(t, out, snaps[0])
	_, err := h.restoreWith(RestoreOptions{Manifest: names[1], Output: out, Mode: ModeStaged, ExpectSHA256: sum(snaps[0])})
	if err == nil || !strings.Contains(err.Error(), "not changed") {
		t.Fatalf("wrong expected SHA-256: %v", err)
	}
	fileEquals(t, out, snaps[0])
	noJournal(t, out)

	// Cancelled while checking: still untouched, journal dropped.
	ctx, cancel := context.WithCancel(h.ctx)
	o := RestoreOptions{Repo: h.repo, VM: "win11", Manifest: names[1], Output: out, Mode: ModeStaged,
		Progress: func(p Phase, done, _ int64) {
			if p == PhaseCheckingTarget && done > 0 {
				cancel()
			}
		}}
	if _, err := Restore(ctx, o); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled check: %v", err)
	}
	fileEquals(t, out, snaps[0])
	noJournal(t, out)
}

// --base that doesn't hold: the check finds the damage before the target
// is touched, and the block is fetched whole.
func TestStagedRepairsWhatBaseGotWrong(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	bad := bytes.Clone(snaps[0])
	bad[500<<10] ^= 0xff // block 7, the same in both backups, so skipped
	writeFile(t, out, bad)
	st, err := h.restoreWith(RestoreOptions{Manifest: names[1], Base: names[0], Output: out, Mode: ModeStaged, ExpectSHA256: sum(snaps[1])})
	if err != nil {
		t.Fatal(err)
	}
	fileEquals(t, out, snaps[1])
	if st.Repaired != 1 || st.BlocksSkipped != 13 {
		t.Fatalf("repaired %d, skipped %d; want 1 and 13", st.Repaired, st.BlocksSkipped)
	}
}

// Interrupted while applying: the journal is kept, and the same command
// finishes the job. A damaged journal is refused.
func TestStagedFinishesAnInterruptedApply(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	writeFile(t, out, snaps[0])
	ctx, cancel := context.WithCancel(h.ctx)
	o := RestoreOptions{Repo: h.repo, VM: "win11", Manifest: names[2], Output: out, Mode: ModeStaged,
		Progress: func(p Phase, done, _ int64) {
			if p == PhaseApplying && done > 0 {
				cancel()
			}
		}}
	if _, err := Restore(ctx, o); err == nil || !strings.Contains(err.Error(), "run the same command again") {
		t.Fatalf("interrupted apply: %v", err)
	}
	jpath := out + ".vmobj-journal"
	if _, err := os.Stat(jpath); err != nil {
		t.Fatalf("journal not kept: %v", err)
	}

	// A journal for another restore is refused.
	if _, err := h.restoreWith(RestoreOptions{Manifest: names[1], Output: out, Mode: ModeStaged}); err == nil || !strings.Contains(err.Error(), "journal for restoring") {
		t.Fatalf("other restore over a pending journal: %v", err)
	}

	// Damage, refused; then the good journal finishes it.
	good, _ := os.ReadFile(jpath)
	broken := bytes.Clone(good)
	broken[len(broken)-5] ^= 1
	writeFile(t, jpath, broken)
	if _, err := h.restoreWith(RestoreOptions{Manifest: names[2], Output: out, Mode: ModeStaged}); err == nil || !strings.Contains(err.Error(), "failed its check") {
		t.Fatalf("damaged journal: %v", err)
	}
	writeFile(t, jpath, good)
	st, err := h.restoreWith(RestoreOptions{Manifest: names[2], Output: out, Mode: ModeStaged})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Finished {
		t.Fatal("didn't say it finished an earlier apply")
	}
	fileEquals(t, out, snaps[2])
	noJournal(t, out)
}

// A journal left by a run that never committed (killed mid-way) is thrown
// away: the target was never touched.
func TestStagedDiscardsAnUncommittedJournal(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	writeFile(t, out, snaps[0])
	m, err := h.repo.GetManifest(h.ctx, "win11", names[1])
	if err != nil {
		t.Fatal(err)
	}
	j, err := createJournal(h.repo, out+".vmobj-journal", journalHeader{VM: "win11", Manifest: m.Name, DiskSize: m.DiskSize, Target: out}, int64(m.BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	j.WriteAt([]byte("half a run of junk"), 0)
	j.close()
	st, err := h.restoreWith(RestoreOptions{Manifest: names[1], Output: out, Mode: ModeStaged})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Discarded {
		t.Fatal("didn't say it discarded the old journal")
	}
	fileEquals(t, out, snaps[1])
	noJournal(t, out)
}

func TestStagedRefusals(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	for _, o := range []RestoreOptions{
		{Manifest: names[1], Output: filepath.Join(h.dir, "missing.raw"), Mode: ModeStaged},
		{Manifest: names[1], Output: out, Mode: ModeStaged, Check: true, CheckWritten: true},
	} {
		writeFile(t, out, snaps[0])
		if _, err := h.restoreWith(o); err == nil {
			t.Fatalf("%+v was accepted", o)
		}
	}
	writeFile(t, out, snaps[0][:1000]) // wrong size: staged doesn't resize
	if _, err := h.restoreWith(RestoreOptions{Manifest: names[1], Output: out, Mode: ModeStaged}); err == nil || !strings.Contains(err.Error(), "doesn't resize") {
		t.Fatalf("wrong size: %v", err)
	}
}
