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
	"time"

	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
)

// harness keeps one small repository, disk image and state file for a test.
type harness struct {
	t     *testing.T
	ctx   context.Context
	dir   string
	repo  *repo.Repo
	img   string
	state string
	clock time.Time
	// Storage policy passed to each backup; nil means none.
	profile *policy.Profile
	class   policy.Class
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	b, err := store.NewLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	return newHarnessOn(t, b)
}

// backends lists every kind of repository storage the engine tests run on.
// Go note: a map of functions lets one test body run once per backend.
var backends = map[string]func(t *testing.T) store.Backend{
	"local": func(t *testing.T) store.Backend {
		b, err := store.NewLocal(filepath.Join(t.TempDir(), "repo"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	},
	"s3fake": func(t *testing.T) store.Backend {
		b, err := store.NewS3(s3fake.New("bucket"), "bucket", "repo")
		if err != nil {
			t.Fatal(err)
		}
		return b
	},
}

func newHarnessOn(t *testing.T, b store.Backend) *harness {
	t.Helper()
	dir := t.TempDir()
	k, err := keys.Derive(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	// Small sizes keep the test fast: 64 KiB blocks, 4 KiB extents,
	// and the smallest allowed pack size.
	cfg := repo.Config{BlockSize: 64 << 10, ExtentSize: 4 << 10, PackTarget: 1 << 20}
	r, err := repo.Init(context.Background(), b, k, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, ctx: context.Background(), dir: dir, repo: r,
		img: filepath.Join(dir, "disk.raw"), state: filepath.Join(dir, "vm.state"),
		clock: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
}

func (h *harness) write(data []byte) {
	h.t.Helper()
	if err := os.WriteFile(h.img, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) backup(kind manifest.Kind) *BackupStats {
	h.t.Helper()
	st, err := h.tryBackup(kind)
	if err != nil {
		h.t.Fatalf("%s backup: %v", kind, err)
	}
	return st
}

// tryBackup runs a backup one hour after the previous one.
func (h *harness) tryBackup(kind manifest.Kind) (*BackupStats, error) {
	h.t.Helper()
	src, err := source.OpenRaw(h.img)
	if err != nil {
		h.t.Fatal(err)
	}
	defer src.Close()
	h.clock = h.clock.Add(time.Hour)
	return Backup(h.ctx, BackupOptions{Repo: h.repo, Source: src, VM: "win11",
		Kind: kind, StatePath: h.state, Workers: 4, Now: func() time.Time { return h.clock },
		Profile: h.profile, Class: h.class})
}

// tamper flips the last bit of a stored object. It goes through the
// backend, so it works on any kind of storage.
func (h *harness) tamper(key string) {
	h.t.Helper()
	data, err := h.repo.Backend.Get(h.ctx, key)
	if err != nil {
		h.t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := h.repo.Backend.Put(h.ctx, key, data); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) restoreEquals(name string, want []byte) {
	h.t.Helper()
	out := filepath.Join(h.dir, "restore-"+name+".raw")
	if _, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: name, Output: out}); err != nil {
		h.t.Fatalf("restore %s: %v", name, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		h.t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		h.t.Fatalf("restore %s does not match the disk it backed up", name)
	}
}

func TestFullIncrementalDifferentialRestore(t *testing.T) {
	for name, newBackend := range backends {
		t.Run(name, func(t *testing.T) { testRoundTrips(t, newHarnessOn(t, newBackend(t))) })
	}
}

func testRoundTrips(t *testing.T, h *harness) {
	rng := rand.New(rand.NewSource(42))

	// 1 MiB + 3000 bytes: 16 full blocks plus a short one, half zeros.
	disk := make([]byte, 1<<20+3000)
	rng.Read(disk[:600<<10])
	rng.Read(disk[len(disk)-3000:])
	h.write(disk)
	full1 := h.backup(manifest.Full)
	snap1 := bytes.Clone(disk)
	if full1.BlocksStored == 0 || full1.ZeroBlocks == 0 {
		t.Fatalf("first full stored %d blocks, %d zero", full1.BlocksStored, full1.ZeroBlocks)
	}

	// Change three 4 KiB extents, one of them in the zero area, and zero one out.
	rng.Read(disk[8<<10 : 12<<10])
	rng.Read(disk[100<<10 : 104<<10])
	rng.Read(disk[700<<10 : 701<<10])
	copy(disk[200<<10:204<<10], make([]byte, 4<<10))
	h.write(disk)
	inc1 := h.backup(manifest.Incremental)
	snap2 := bytes.Clone(disk)
	if inc1.ExtentsChanged != 4 || inc1.ExtentsStored != 3 {
		t.Fatalf("incremental: changed=%d stored=%d, want 4 and 3", inc1.ExtentsChanged, inc1.ExtentsStored)
	}

	// Another small change: the next incremental stores only that extent,
	// while its manifest still lists every extent changed since the full.
	rng.Read(disk[300<<10 : 301<<10])
	h.write(disk)
	inc2 := h.backup(manifest.Incremental)
	snap3 := bytes.Clone(disk)
	if inc2.ExtentsChanged != 5 || inc2.ExtentsStored != 1 || inc2.ExtentsReused != 3 {
		t.Fatalf("incremental 2: changed=%d stored=%d reused=%d", inc2.ExtentsChanged, inc2.ExtentsStored, inc2.ExtentsReused)
	}

	// A differential stores every changed extent again.
	diff := h.backup(manifest.Differential)
	if diff.ExtentsStored != 4 { // 5 changed, one of them is zeros
		t.Fatalf("differential stored %d extents, want 4", diff.ExtentsStored)
	}

	// A second full reuses every block that did not change.
	full2 := h.backup(manifest.Full)
	if full2.BlocksStored != 5 || full2.BlocksReused == 0 {
		t.Fatalf("second full stored %d blocks (want 5, one per changed block), reused %d", full2.BlocksStored, full2.BlocksReused)
	}

	// Every restore point restores its own disk, independently.
	h.restoreEquals(full1.Manifest, snap1)
	h.restoreEquals(inc1.Manifest, snap2)
	h.restoreEquals(inc2.Manifest, snap3)
	h.restoreEquals(diff.Manifest, snap3)
	h.restoreEquals(full2.Manifest, snap3)

	names, err := h.repo.ListManifests(h.ctx, "win11")
	if err != nil || len(names) != 5 {
		t.Fatalf("ListManifests = %v, %v", names, err)
	}
	for _, n := range names {
		vs, err := Verify(h.ctx, h.repo, "win11", n, true)
		if err != nil || len(vs.Missing) > 0 {
			t.Fatalf("verify %s: %v %v", n, err, vs.Missing)
		}
	}
}

func TestIncrementalNeedsFull(t *testing.T) {
	h := newHarness(t)
	h.write(make([]byte, 256<<10))
	src, _ := source.OpenRaw(h.img)
	defer src.Close()
	_, err := Backup(h.ctx, BackupOptions{Repo: h.repo, Source: src, VM: "win11",
		Kind: manifest.Incremental, StatePath: h.state})
	if !errors.Is(err, ErrNeedFull) {
		t.Fatalf("got %v, want ErrNeedFull", err)
	}
}

func TestTamperedBlockFailsRestore(t *testing.T) {
	h := newHarness(t)
	disk := make([]byte, 128<<10)
	rand.New(rand.NewSource(1)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full)

	keys, _ := h.repo.Backend.List(h.ctx, "blocks/")
	h.tamper(keys[0])

	out := filepath.Join(h.dir, "bad.raw")
	_, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: full.Manifest, Output: out})
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("restore of a tampered block: %v", err)
	}
	vs, _ := Verify(h.ctx, h.repo, "win11", full.Manifest, true)
	if len(vs.Missing) == 0 {
		t.Fatal("deep verify did not report the tampered block")
	}
}

func TestRestoreRefusesToOverwrite(t *testing.T) {
	h := newHarness(t)
	h.write(make([]byte, 64<<10))
	full := h.backup(manifest.Full)
	out := filepath.Join(h.dir, "exists.raw")
	os.WriteFile(out, []byte("keep me"), 0o600)
	_, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: full.Manifest, Output: out})
	if err == nil {
		t.Fatal("restore replaced an existing file without --force")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	h := newHarness(t)
	other, _ := keys.Derive(bytes.Repeat([]byte{8}, 32))
	if _, err := repo.Open(h.ctx, h.repo.Backend, other); err == nil {
		t.Fatal("opened the repository with the wrong key")
	}
}
