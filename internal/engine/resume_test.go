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
	"sync"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/store"
)

// flaky is a backend whose block reads or writes start failing after a
// set number succeed, as a connection that drops part-way does.
type flaky struct {
	store.Backend
	mu         sync.Mutex
	getsLeft   int // -1: never fail
	putsLeft   int
	blocksRead int
}

var errDropped = errors.New("read: network is unreachable")

func newFlaky(t *testing.T) *flaky {
	b, err := store.NewLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	return &flaky{Backend: b, getsLeft: -1, putsLeft: -1}
}

func (f *flaky) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.HasPrefix(key, "blocks/") {
		f.mu.Lock()
		if f.getsLeft == 0 {
			f.mu.Unlock()
			return nil, errDropped
		}
		if f.getsLeft > 0 {
			f.getsLeft--
		}
		f.blocksRead++
		f.mu.Unlock()
	}
	return f.Backend.Get(ctx, key)
}

func (f *flaky) Put(ctx context.Context, key string, data []byte) error {
	if strings.HasPrefix(key, "blocks/") {
		f.mu.Lock()
		if f.putsLeft == 0 {
			f.mu.Unlock()
			return errDropped
		}
		if f.putsLeft > 0 {
			f.putsLeft--
		}
		f.mu.Unlock()
	}
	return f.Backend.Put(ctx, key, data)
}

func noisyDisk(seed int64, size int) []byte {
	d := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(d[:size*3/4]) // the last quarter stays zeros
	return d
}

// A restore that breaks part-way keeps what it wrote; running it again
// fetches only the rest, and the result is the backup.
func TestRestoreResumes(t *testing.T) {
	f := newFlaky(t)
	h := newHarnessOn(t, f)
	disk := noisyDisk(41, 2<<20) // 32 blocks of 64 KiB, 24 with data
	h.write(disk)
	name := h.backup(manifest.Full).Manifest
	out := filepath.Join(h.dir, "restored.raw")

	f.getsLeft = 10
	_, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: name, Output: out})
	if err == nil || !errors.Is(err, errDropped) || !strings.Contains(err.Error(), "run the same restore again") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a partial restore appeared under the real name")
	}
	if _, err := os.Stat(PartialName(out)); err != nil {
		t.Fatalf("the partial file wasn't kept: %v", err)
	}

	f.getsLeft, f.blocksRead = -1, 0
	st, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: name, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Resumed || st.ResumedFrom != PartialName(out) {
		t.Fatalf("not resumed: %+v", st)
	}
	if f.blocksRead != 24-10 {
		t.Fatalf("fetched %d blocks on the rerun; only the 14 not yet written should be", f.blocksRead)
	}
	if !bytes.Equal(readFile(t, out), disk) {
		t.Fatal("the resumed restore differs from the disk")
	}
	if _, err := os.Stat(PartialName(out)); !os.IsNotExist(err) {
		t.Fatal("the partial file is still there after finishing")
	}
}

// Whatever the partial file holds, a resume makes it the backup: it
// trusts nothing it hasn't compared.
func TestRestoreResumeFixesAWrongPartial(t *testing.T) {
	h := newHarness(t)
	disk := noisyDisk(42, 1<<20)
	h.write(disk)
	name := h.backup(manifest.Full).Manifest
	out := filepath.Join(h.dir, "restored.raw")
	junk := noisyDisk(99, len(disk))
	os.WriteFile(PartialName(out), junk, 0o600)
	st, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: name, Output: out})
	if err != nil || !st.Resumed {
		t.Fatalf("%v %+v", err, st)
	}
	if !bytes.Equal(readFile(t, out), disk) {
		t.Fatal("a junk partial file survived into the result")
	}
}

// --no-resume, or a partial file of another size, starts afresh.
func TestRestoreNoResumeAndWrongSize(t *testing.T) {
	h := newHarness(t)
	disk := noisyDisk(43, 1<<20)
	h.write(disk)
	name := h.backup(manifest.Full).Manifest
	for _, c := range []struct {
		name     string
		partial  int
		noResume bool
	}{{"no-resume", len(disk), true}, {"other size", len(disk) / 2, false}} {
		out := filepath.Join(h.dir, c.name+".raw")
		os.WriteFile(PartialName(out), noisyDisk(7, c.partial), 0o600)
		st, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: name, Output: out, NoResume: c.noResume})
		if err != nil || st.Resumed {
			t.Fatalf("%s: %v resumed=%v", c.name, err, st != nil && st.Resumed)
		}
		if !bytes.Equal(readFile(t, out), disk) {
			t.Fatalf("%s: the result differs", c.name)
		}
	}
}

// Two restores can't write one partial file at once.
func TestRestorePartialIsLocked(t *testing.T) {
	h := newHarness(t)
	disk := noisyDisk(44, 1<<20)
	h.write(disk)
	name := h.backup(manifest.Full).Manifest
	out := filepath.Join(h.dir, "restored.raw")
	os.WriteFile(PartialName(out), make([]byte, len(disk)), 0o600)
	held, err := os.OpenFile(PartialName(out), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := lockFile(held); err != nil {
		t.Skip("no file locking here")
	}
	if _, err := Restore(h.ctx, RestoreOptions{Repo: h.repo, VM: "win11", Manifest: name, Output: out}); err == nil || !strings.Contains(err.Error(), "another restore") {
		t.Fatalf("got %v", err)
	}
}

// A backup that breaks part-way lists nothing; running it again uploads
// only the blocks that didn't make it.
func TestBackupResumes(t *testing.T) {
	f := newFlaky(t)
	h := newHarnessOn(t, f)
	h.write(noisyDisk(45, 2<<20)) // 24 blocks with data
	f.putsLeft = 9
	if _, err := h.tryBackup(manifest.Full); err == nil {
		t.Fatal("the failure wasn't reported")
	}
	if names, _ := h.repo.ListManifests(h.ctx, "win11"); len(names) != 0 {
		t.Fatalf("a broken backup was listed: %v", names)
	}
	f.putsLeft = -1
	st, err := h.tryBackup(manifest.Full)
	if err != nil {
		t.Fatal(err)
	}
	if st.BlocksReused < 9 || st.BlocksReused+st.BlocksStored != 24 {
		t.Fatalf("rerun reused %d and stored %d blocks; the 9 already uploaded must be reused", st.BlocksReused, st.BlocksStored)
	}
}

// restore-vm stopped part-way through its second disk continues when run
// again into the same folder: the finished disk is checked in place, the
// unfinished one continues from its partial file. Another backup's
// unfinished folder is refused.
func TestRestoreVMResumes(t *testing.T) {
	v := newVMHarness(t)
	vda, vdb := filepath.Join(v.dir, "vda.img"), filepath.Join(v.dir, "vdb.img")
	diskA := randomDisk(t, vda, 512<<10, 1)
	diskB := randomDisk(t, vdb, 512<<10, 2)
	v.virsh.xml = domainXML(fileDisk("vda", vda)+"\n"+fileDisk("vdb", vdb), "")
	full, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(v.dir, "restored")
	ctx, cancel := context.WithCancel(v.ctx)
	_, err = RestoreVM(ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: full.Record, Dir: dir,
		Progress: func(target string, p Phase, done, total int64) {
			if target == "vdb" && done > total/2 {
				cancel() // stop half-way through the second disk
			}
		}})
	cancel()
	if err == nil {
		t.Fatal("the interrupted restore reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, RestoreVMMarker)); err != nil {
		t.Fatal("no marker left for the rerun")
	}
	if _, err := os.Stat(filepath.Join(dir, "win11.xml")); !os.IsNotExist(err) {
		t.Fatal("the definition was written before the disks were done")
	}

	// Another backup's restore won't write into this folder.
	other, err := BackupVM(v.ctx, v.opts(manifest.Incremental))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: other.Record, Dir: dir}); err == nil || !strings.Contains(err.Error(), "another backup") {
		t.Fatalf("another backup's restore into this folder: %v", err)
	}

	st, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: v.repo, VM: "win11", Record: full.Record, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Resumed {
		t.Fatal("not reported as resumed")
	}
	if !bytes.Equal(readFile(t, st.Disks["vda"]), diskA) || !bytes.Equal(readFile(t, st.Disks["vdb"]), diskB) {
		t.Fatal("a resumed disk differs")
	}
	if _, err := os.Stat(filepath.Join(dir, RestoreVMMarker)); !os.IsNotExist(err) {
		t.Fatal("the marker is still there after finishing")
	}
}
