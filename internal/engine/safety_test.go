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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/source"
)

// A failed restore must leave no partial image behind and must not harm
// an existing file, even with --force.
func TestFailedRestoreLeavesNothing(t *testing.T) {
	h := newHarness(t)
	disk := make([]byte, 256<<10)
	rand.New(rand.NewSource(3)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full)

	// Tamper with one stored block so the restore fails partway.
	keys, _ := h.repo.Backend.List(h.ctx, "blocks/")
	p := filepath.Join(h.dir, "repo", filepath.FromSlash(keys[len(keys)-1]))
	data, _ := os.ReadFile(p)
	data[len(data)-1] ^= 1
	os.WriteFile(p, data, 0o600)

	out := filepath.Join(h.dir, "existing.raw")
	os.WriteFile(out, []byte("precious"), 0o600)
	if _, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: full.Manifest, Output: out, Force: true}); err == nil {
		t.Fatal("restore of a tampered repository succeeded")
	}
	if got, _ := os.ReadFile(out); string(got) != "precious" {
		t.Fatal("a failed --force restore damaged the existing file")
	}
	entries, _ := os.ReadDir(h.dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "vmobj-restore") {
			t.Fatalf("temporary restore file left behind: %s", e.Name())
		}
	}
}

// A restore point with a missing piece is refused before anything is written.
func TestIncompleteRestoreRefused(t *testing.T) {
	h := newHarness(t)
	disk := make([]byte, 128<<10)
	rand.New(rand.NewSource(4)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full)
	keys, _ := h.repo.Backend.List(h.ctx, "blocks/")
	h.repo.Backend.Delete(h.ctx, keys[0])

	out := filepath.Join(h.dir, "new.raw")
	_, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: full.Manifest, Output: out})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("got %v, want ErrIncomplete", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("output file was created for an incomplete restore point")
	}
}

// --force replaces an existing file only once the new image is complete.
func TestForceReplacesFile(t *testing.T) {
	h := newHarness(t)
	disk := make([]byte, 100<<10)
	rand.New(rand.NewSource(5)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full)
	out := filepath.Join(h.dir, "replace.raw")
	os.WriteFile(out, []byte("old"), 0o600)
	if _, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: full.Manifest, Output: out, Force: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, disk) {
		t.Fatal("forced restore did not produce the disk")
	}
}

// The last extent of a disk is usually short. Changing it, then zeroing it,
// must restore exactly at each step.
func TestShortLastExtent(t *testing.T) {
	h := newHarness(t)
	rng := rand.New(rand.NewSource(6))
	disk := make([]byte, 1<<20+3000)
	rng.Read(disk)
	h.write(disk)
	h.backup(manifest.Full)

	rng.Read(disk[len(disk)-100:])
	h.write(disk)
	a := h.backup(manifest.Incremental)
	snapA := bytes.Clone(disk)

	copy(disk[len(disk)-3000:], make([]byte, 3000))
	h.write(disk)
	b := h.backup(manifest.Incremental)

	h.restoreEquals(a.Manifest, snapA)
	h.restoreEquals(b.Manifest, disk)
}

// Runs started in the same second get different manifest names.
func TestSameSecondNamesDiffer(t *testing.T) {
	h := newHarness(t)
	h.write(make([]byte, 64<<10))
	src, _ := source.OpenRaw(h.img)
	defer src.Close()
	opts := BackupOptions{Repo: h.repo, Source: src, VM: "win11", Kind: manifest.Full,
		StatePath: h.state, Now: func() time.Time { return h.clock }}
	a, err := Backup(h.ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Backup(h.ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.Manifest == b.Manifest {
		t.Fatalf("both runs named %s", a.Manifest)
	}
}

// A second run while one holds the lock fails immediately.
func TestLockBlocksConcurrentRun(t *testing.T) {
	h := newHarness(t)
	h.write(make([]byte, 64<<10))
	unlock, err := lockState(h.state)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	src, _ := source.OpenRaw(h.img)
	defer src.Close()
	_, err = Backup(h.ctx, BackupOptions{Repo: h.repo, Source: src, VM: "win11", Kind: manifest.Full, StatePath: h.state})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("got %v, want an already-running error", err)
	}
}

// A damaged state file asks for a full instead of crashing.
func TestDamagedStateNeedsFull(t *testing.T) {
	h := newHarness(t)
	h.write(make([]byte, 256<<10))
	h.backup(manifest.Full)
	st, err := LoadState(h.state)
	if err != nil {
		t.Fatal(err)
	}
	st.Base = st.Base[:3]
	if err := st.Save(h.state); err != nil {
		t.Fatal(err)
	}
	src, _ := source.OpenRaw(h.img)
	defer src.Close()
	_, err = Backup(h.ctx, BackupOptions{Repo: h.repo, Source: src, VM: "win11", Kind: manifest.Incremental, StatePath: h.state})
	if !errors.Is(err, ErrNeedFull) {
		t.Fatalf("got %v, want ErrNeedFull", err)
	}
}
