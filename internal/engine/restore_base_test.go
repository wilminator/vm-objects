// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
)

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (h *harness) restoreWith(o RestoreOptions) (*RestoreStats, error) {
	o.Repo, o.VM = h.repo, "win11"
	if o.Mode == "" {
		o.Mode = ModeInPlace
	}
	return Restore(h.ctx, o)
}

// A replay step with --base reads only the blocks that differ between the
// two backups, and a full check still proves the whole disk.
func TestBaseSkipsBlocksTheSameInBoth(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	if err := os.WriteFile(out, snaps[0], 0o640); err != nil {
		t.Fatal(err)
	}

	st, err := h.restoreWith(RestoreOptions{Manifest: names[1], Base: names[0], Output: out, Check: true, ExpectSHA256: sum(snaps[1])})
	if err != nil {
		t.Fatal(err)
	}
	fileEquals(t, out, snaps[1])
	if st.BlocksSkipped != 13 || st.Compared != 4*(64<<10) {
		t.Fatalf("skipped %d blocks and compared %d bytes; want 13 skipped and the 4 changed blocks compared", st.BlocksSkipped, st.Compared)
	}
	if st.PiecesWritten != 4 || st.SHA256 != sum(snaps[1]) || st.CheckedBytes != int64(len(snaps[1])) {
		t.Fatalf("wrote %d pieces, sha %s, checked %d bytes", st.PiecesWritten, st.SHA256, st.CheckedBytes)
	}

	// Incremental 1 -> 2: the four blocks with incremental 1's extents are
	// the same in both backups, so they are skipped too, and the full check
	// verifies them piece by piece.
	st, err = h.restoreWith(RestoreOptions{Manifest: names[2], Base: names[1], Output: out, Check: true, ExpectSHA256: sum(snaps[2])})
	if err != nil {
		t.Fatal(err)
	}
	fileEquals(t, out, snaps[2])
	if st.BlocksSkipped != 16 || st.PiecesWritten != 1 {
		t.Fatalf("skipped %d blocks, wrote %d pieces; want 16 skipped, 1 piece", st.BlocksSkipped, st.PiecesWritten)
	}
}

// --base trusts the target: damage in a skipped block isn't repaired, but a
// full check finds it, while a written-only check does not look there.
func TestBaseTrustsTheTargetAndTheFullCheckCatchesIt(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	bad := bytes.Clone(snaps[0])
	bad[500<<10] ^= 0xff // block 7, which no incremental touches
	if err := os.WriteFile(out, bad, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restoreWith(RestoreOptions{Manifest: names[1], Base: names[0], Output: out, Check: true}); err == nil || !strings.Contains(err.Error(), "block 7") {
		t.Fatalf("full check over a damaged skipped block: %v", err)
	}
	if err := os.WriteFile(out, bad, 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := h.restoreWith(RestoreOptions{Manifest: names[1], Base: names[0], Output: out, Check: true, CheckWritten: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA256 != "" || st.CheckedBytes != 4*(64<<10) {
		t.Fatalf("written-only check read %d bytes (sha %q); want the 4 written blocks, no sha", st.CheckedBytes, st.SHA256)
	}
	// Without --base the same restore repairs the block.
	if _, err := h.restoreWith(RestoreOptions{Manifest: names[1], Output: out, Check: true, ExpectSHA256: sum(snaps[1])}); err != nil {
		t.Fatal(err)
	}
	fileEquals(t, out, snaps[1])
}

func TestExpectSHA256(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	out := filepath.Join(h.dir, "target.raw")
	if err := os.WriteFile(out, snaps[0], 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := h.restoreWith(RestoreOptions{Manifest: names[1], Output: out, Check: true, ExpectSHA256: sum(snaps[0])})
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("wrong expected sha: %v", err)
	}
	for _, o := range []RestoreOptions{
		{Manifest: names[1], Output: out, ExpectSHA256: sum(snaps[1])},                                  // no --check
		{Manifest: names[1], Output: out, Check: true, CheckWritten: true, ExpectSHA256: sum(snaps[1])}, // written-only
		{Manifest: names[1], Output: out, CheckWritten: true},                                           // --check-written alone
		{Manifest: names[1], Output: out, Base: names[0], Mode: ModeCopy, Scratch: h.dir},               // --base with copy
	} {
		if _, err := h.restoreWith(o); err == nil {
			t.Fatalf("%+v was accepted", o)
		}
	}
}

func TestBaseMustFit(t *testing.T) {
	h := newHarness(t)
	names, snaps := waypoints(t, h)
	disk := bytes.Clone(snaps[2])
	h.write(append(disk, make([]byte, 64<<10)...)) // the disk grows a block
	other := h.backup(manifest.Full).Manifest
	out := filepath.Join(h.dir, "target.raw")
	if err := os.WriteFile(out, snaps[2], 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restoreWith(RestoreOptions{Manifest: other, Base: names[2], Output: out}); err == nil || !strings.Contains(err.Error(), "doesn't fit") {
		t.Fatalf("base of another size: %v", err)
	}
}

// A restore writes each block once, with its newer extents merged in, and
// doesn't fetch a full block that newer extents cover entirely.
func TestRestoreMergesNewerExtentsInOnePass(t *testing.T) {
	h := newHarness(t)
	rng := rand.New(rand.NewSource(3))
	disk := make([]byte, 4*(64<<10))
	rng.Read(disk)
	h.write(disk)
	h.backup(manifest.Full)
	rng.Read(disk[64<<10 : 128<<10]) // block 1, every extent new
	rng.Read(disk[200<<10 : 204<<10])
	h.write(disk)
	inc := h.backup(manifest.Incremental).Manifest

	out := filepath.Join(h.dir, "merged.raw")
	st, err := h.restoreWith(RestoreOptions{Manifest: inc, Output: out, Mode: ModeRename})
	if err != nil {
		t.Fatal(err)
	}
	fileEquals(t, out, disk)
	if st.BlocksRead != 3 || st.ExtentsRead != 17 {
		t.Fatalf("fetched %d blocks and %d extents; want 3 blocks (block 1 is covered) and 17 extents", st.BlocksRead, st.ExtentsRead)
	}
}
