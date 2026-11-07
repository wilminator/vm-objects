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
	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
)

// destRepo makes a destination repository on the fake S3 with the given
// key byte (7 is the harness's own key).
func destRepo(t *testing.T, srv *s3fake.Server, bucket string, keyByte byte, changes ...func(*repo.Config)) *repo.Repo {
	t.Helper()
	b, err := store.NewS3(srv, bucket, "dest")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := keys.Derive(bytes.Repeat([]byte{keyByte}, 32))
	cfg := repo.Config{BlockSize: 64 << 10, ExtentSize: 4 << 10, PackTarget: 1 << 20}
	for _, c := range changes {
		c(&cfg)
	}
	r, err := repo.Init(context.Background(), b, k, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// restoreFrom restores a manifest from any repository and compares it.
func restoreFrom(t *testing.T, h *harness, r *repo.Repo, vm, name string, want []byte) {
	t.Helper()
	out := filepath.Join(h.dir, "copy-"+name+".raw")
	os.Remove(out)
	if _, err := Restore(h.ctx, RestoreOptions{Repo: r, VM: vm, Manifest: name, Output: out}); err != nil {
		t.Fatalf("restore %s from the copy: %v", name, err)
	}
	if got := readFile(t, out); !bytes.Equal(got, want) {
		t.Fatalf("restore %s from the copy differs", name)
	}
}

func chainScenario(t *testing.T, h *harness) (names []string, disks [][]byte) {
	t.Helper()
	p, _ := policy.Lookup("standard")
	h.profile = &p
	disk := make([]byte, 512<<10)
	rng := rand.New(rand.NewSource(21))
	rng.Read(disk)
	for _, k := range []manifest.Kind{manifest.Full, manifest.Incremental, manifest.Incremental} {
		h.write(disk)
		names = append(names, h.backup(k).Manifest)
		disks = append(disks, bytes.Clone(disk))
		rng.Read(disk[rng.Intn(120)<<12:][:4<<10])
	}
	return names, disks
}

func TestCopySameKeyIsIncrementalAndIdempotent(t *testing.T) {
	h := newHarness(t)
	names, disks := chainScenario(t, h)
	srv := s3fake.New("b")
	to := destRepo(t, srv, "b", 7)
	prof, _ := policy.Lookup("standard")
	o := CopyOptions{From: h.repo, To: to, Profile: &prof, Now: func() time.Time { return h.clock }}

	st := &CopyStats{}
	if err := CopyManifest(h.ctx, o, "win11", names[0], st); err != nil {
		t.Fatal(err)
	}
	if st.BlocksCopied != 8 || st.Manifests != 1 {
		t.Fatalf("full copy: %+v", st)
	}
	// Same key: blocks travel as stored, so the stored bytes are equal.
	srcKeys, _ := h.repo.Backend.List(h.ctx, "blocks/")
	a, _ := h.repo.Backend.Get(h.ctx, srcKeys[0])
	b, _ := to.Backend.Get(h.ctx, srcKeys[0])
	if !bytes.Equal(a, b) {
		t.Fatal("same-key copy re-encrypted a block")
	}

	all, err := CopyAll(h.ctx, o, "win11")
	if err != nil {
		t.Fatal(err)
	}
	// The second incremental shares the first's extents: each moves once.
	if all.Manifests != 2 || all.ExtentsCopied != 2 || all.ExtentsPresent != 1 || len(all.Skipped) != 1 {
		t.Fatalf("copy all: %+v", all)
	}
	for i, n := range names {
		restoreFrom(t, h, to, "win11", n, disks[i])
	}
	puts := srv.Calls["PutObject"]
	again, err := CopyAll(h.ctx, o, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.Manifests != 0 || srv.Calls["PutObject"] != puts {
		t.Fatalf("a repeated copy wrote again: %+v", again)
	}
}

func TestCopyDifferentKeysTranslates(t *testing.T) {
	h := newHarness(t)
	names, disks := chainScenario(t, h)
	to := destRepo(t, s3fake.New("b"), "b", 9)
	o := CopyOptions{From: h.repo, To: to, Now: func() time.Time { return h.clock }}
	st, err := CopyAll(h.ctx, o, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Manifests != 3 {
		t.Fatalf("%+v", st)
	}
	// Content IDs are keyed, so no destination block shares a source name.
	src, _ := h.repo.Backend.List(h.ctx, "blocks/")
	for _, k := range src {
		if ok, _ := to.Backend.Exists(h.ctx, k); ok {
			t.Fatalf("%s kept its source name under a different key", k)
		}
	}
	for i, n := range names {
		restoreFrom(t, h, to, "win11", n, disks[i])
		vs, err := Verify(h.ctx, to, "win11", n, true)
		if err != nil || len(vs.Missing) > 0 {
			t.Fatalf("verify %s at the destination: %v %v", n, err, vs.Missing)
		}
	}
}

func TestCopyToLockedDestination(t *testing.T) {
	h := newHarness(t)
	names, _ := chainScenario(t, h)
	srv := s3fake.New()
	srv.AddLockedBucket("locked")
	srv.Now = func() time.Time { return h.clock }
	to := destRepo(t, srv, "locked", 7, func(c *repo.Config) { c.ObjectLock = true })

	// A locked destination needs a profile.
	if _, err := CopyAll(h.ctx, CopyOptions{From: h.repo, To: to}, ""); !errors.Is(err, ErrPolicyNeeded) {
		t.Fatalf("copy to a locked repository without a profile: %v", err)
	}
	prof, _ := policy.Lookup("standard")
	o := CopyOptions{From: h.repo, To: to, Profile: &prof, Now: func() time.Time { return h.clock }}
	if _, err := CopyAll(h.ctx, o, ""); err != nil {
		t.Fatal(err)
	}
	m, _ := h.repo.GetManifest(h.ctx, "win11", names[0])
	blocks, _ := to.Backend.List(h.ctx, "blocks/")
	for _, k := range blocks {
		got := srv.Object("locked", "dest/"+k).LockUntil
		if got.Before(m.Created.Add(90 * policy.Day)) {
			t.Fatalf("%s locked only until %v", k, got)
		}
	}

	// Copying a backup whose retention would already be over is skipped.
	late := CopyOptions{From: h.repo, To: destRepo(t, s3fake.New("c"), "c", 7), Profile: &prof,
		Now: func() time.Time { return h.clock.Add(100 * policy.Day) }}
	st, err := CopyAll(h.ctx, late, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Manifests != 0 || len(st.Skipped) != 3 || !strings.Contains(st.Skipped[0], "retention would have ended") {
		t.Fatalf("expired copy: %+v", st)
	}
}

func TestCopyRefusals(t *testing.T) {
	h := newHarness(t)
	names, _ := chainScenario(t, h)
	other := destRepo(t, s3fake.New("b"), "b", 7, func(c *repo.Config) { c.BlockSize = 128 << 10 })
	if err := CopyManifest(h.ctx, CopyOptions{From: h.repo, To: other}, "win11", names[0], &CopyStats{}); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("different block sizes: %v", err)
	}
	if err := CopyManifest(h.ctx, CopyOptions{From: h.repo, To: h.repo}, "win11", names[0], &CopyStats{}); err == nil {
		t.Fatal("copied a repository onto itself")
	}

	// Archived source data must be restored first.
	hs, _ := newS3Harness(t)
	prof, _ := policy.Lookup("disaster-recovery")
	hs.profile = &prof
	hs.write(bytes.Repeat([]byte{3}, 128<<10))
	full := hs.backup(manifest.Full)
	err := CopyManifest(hs.ctx, CopyOptions{From: hs.repo, To: destRepo(t, s3fake.New("d"), "d", 7)}, "win11", full.Manifest, &CopyStats{})
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		t.Fatalf("copy from Deep Archive: %v", err)
	}
}

func TestCopyVMRecord(t *testing.T) {
	v := newVMHarness(t)
	vda := filepath.Join(v.dir, "vda.img")
	disk := randomDisk(t, vda, 192<<10, 4)
	v.virsh.xml = domainXML(fileDisk("vda", vda), v.nvram)
	st, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	to := destRepo(t, s3fake.New("b"), "b", 9)
	prof, _ := policy.Lookup("standard")
	cs, err := CopyAll(v.ctx, CopyOptions{From: v.repo, To: to, Profile: &prof, Now: func() time.Time { return v.clock }}, "win11")
	if err != nil {
		t.Fatal(err)
	}
	if cs.Records != 1 || cs.Manifests != 1 {
		t.Fatalf("%+v", cs)
	}
	rs, err := RestoreVM(v.ctx, RestoreVMOptions{Repo: to, VM: "win11", Record: st.Record, Dir: filepath.Join(v.dir, "from-copy")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, rs.Disks["vda"]), disk) {
		t.Fatal("VM restored from the copy differs")
	}
}

// An overlay extent with the same content as a short final base block
// must still be copied. (Found in review: one translation table for both
// made copy skip it.)
func TestCopyExtentEqualToShortLastBlock(t *testing.T) {
	h := newHarness(t)
	disk := make([]byte, 68<<10) // a 64 KiB block plus a 4 KiB one (= one extent)
	rand.New(rand.NewSource(1)).Read(disk)
	h.write(disk)
	full := h.backup(manifest.Full).Manifest
	copy(disk[0:4<<10], disk[64<<10:]) // extent 0 now equals the last block
	h.write(disk)
	inc := h.backup(manifest.Incremental).Manifest
	to := destRepo(t, s3fake.New("b"), "b", 7)
	o := CopyOptions{From: h.repo, To: to, Now: func() time.Time { return h.clock }}
	st := &CopyStats{}
	for _, n := range []string{full, inc} {
		if err := CopyManifest(h.ctx, o, "win11", n, st); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := Verify(h.ctx, to, "win11", inc, true)
	if err != nil || len(vs.Missing) > 0 {
		t.Fatalf("copied incremental is missing pieces: %v %v", err, vs.Missing)
	}
	restoreFrom(t, h, to, "win11", inc, disk)
}
