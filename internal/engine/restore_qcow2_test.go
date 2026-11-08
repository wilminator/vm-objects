// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
)

// qemuSame asks QEMU whether a qcow2 image (with its chain) reads as want.
func qemuSame(t *testing.T, img string, want []byte) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return // vmobj's own check still ran; CI has qemu-img
	}
	if out, err := exec.Command("qemu-img", "check", img).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img check %s: %v\n%s", img, err, out)
	}
	raw := img + ".want"
	os.WriteFile(raw, want, 0o600)
	if out, err := exec.Command("qemu-img", "compare", "-f", "qcow2", "-F", "raw", img, raw).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img compare %s: %v\n%s", img, err, out)
	}
}

// A history restored the way QNAP keeps one: the first snapshot as a
// standalone qcow2 image, each later one as an overlay on the last that
// holds only what changed.
func TestRestoreQCOW2Chain(t *testing.T) {
	h := newHarness(t)
	rng := rand.New(rand.NewSource(21))
	disk := make([]byte, 1<<20+12<<10) // a whole number of sectors, not of blocks
	rng.Read(disk[:600<<10])
	var names []string
	var images [][]byte
	for i := range 3 {
		if i > 0 {
			rng.Read(disk[i*150<<10 : i*150<<10+3000])
			copy(disk[700<<10:], make([]byte, 8<<10)) // and some zeroing over data
		}
		h.write(disk)
		kind := manifest.Incremental
		if i == 0 {
			kind = manifest.Full
		}
		names = append(names, h.backup(kind).Manifest)
		images = append(images, append([]byte(nil), disk...))
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "vm-vda.img") // qcow2 data under QNAP's .img name
	st, err := RestoreQCOW2(h.ctx, QCOW2Options{Repo: h.repo, VM: "win11", Manifest: names[0], Output: base, Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA256 != sum(images[0]) {
		t.Fatal("standalone image's SHA-256 differs")
	}
	qemuSame(t, base, images[0])

	prev, prevName := base, names[0]
	for i := 1; i < 3; i++ {
		ov := filepath.Join(dir, "vm-vda."+string(rune('0'+i))+"794103287")
		st, err := RestoreQCOW2(h.ctx, QCOW2Options{Repo: h.repo, VM: "win11", Manifest: names[i], Output: ov,
			Backing: prev, BackingFormat: "qcow2", Base: prevName, Check: true, ExpectSHA256: sum(images[i])})
		if err != nil {
			t.Fatalf("overlay %d: %v", i, err)
		}
		if st.BlocksSkipped == 0 {
			t.Fatalf("overlay %d: no block was skipped thanks to the base", i)
		}
		qemuSame(t, ov, images[i])
		if fi, _ := os.Stat(ov); fi.Size() > 512<<10 {
			t.Fatalf("overlay %d is %d bytes; it should hold only a few clusters", i, fi.Size())
		}
		prev, prevName = ov, names[i]
	}
}

// Claiming the wrong base leaves blocks out that do differ; the check
// finds it and the overlay is removed.
func TestRestoreQCOW2WrongBaseIsCaught(t *testing.T) {
	h := newHarness(t)
	rng := rand.New(rand.NewSource(22))
	disk := make([]byte, 1<<20)
	rng.Read(disk[:500<<10])
	h.write(disk)
	full := h.backup(manifest.Full).Manifest
	rng.Read(disk[10<<10 : 20<<10])
	h.write(disk)
	inc := h.backup(manifest.Incremental).Manifest
	rng.Read(disk[300<<10 : 310<<10])
	h.write(disk)
	inc2 := h.backup(manifest.Incremental).Manifest

	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if _, err := RestoreQCOW2(h.ctx, QCOW2Options{Repo: h.repo, VM: "win11", Manifest: full, Output: base}); err != nil {
		t.Fatal(err)
	}
	ov := filepath.Join(dir, "over.qcow2")
	_, err := RestoreQCOW2(h.ctx, QCOW2Options{Repo: h.repo, VM: "win11", Manifest: inc2, Output: ov,
		Backing: base, BackingFormat: "qcow2", Base: inc, Check: true})
	if err == nil {
		t.Fatal("an overlay on the wrong base passed its check")
	}
	if _, serr := os.Stat(ov); !os.IsNotExist(serr) {
		t.Fatal("the failed overlay was left behind")
	}
}
