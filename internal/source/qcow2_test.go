// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package source

import (
	"bytes"
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests build images with QEMU's own tools and check that the qcow2
// reader returns exactly what "qemu-img convert -O raw" does. They are
// skipped when qemu-img isn't installed; CI installs it.

func needQemu(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed (apt install qemu-utils)", tool)
		}
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// testDisk writes a raw disk with random data, a zero run and holes.
func testDisk(t *testing.T, path string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	rng := rand.New(rand.NewSource(int64(size)))
	for off := 0; off < size; off += 192 << 10 {
		end := min(off+(96<<10), size)
		rng.Read(data[off:end]) // 96 KiB of data, then 96 KiB of zeros
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// qemuRaw converts an image to raw with qemu-img and returns the bytes.
func qemuRaw(t *testing.T, image string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "want.raw")
	run(t, "qemu-img", "convert", "-O", "raw", image, out)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sameAsQemu reads image through Open and compares it with QEMU's view,
// both in one pass and in odd-sized pieces that straddle clusters.
func sameAsQemu(t *testing.T, image, format string) {
	t.Helper()
	want := qemuRaw(t, image)
	src, err := Open(image, format)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.Size() != int64(len(want)) {
		t.Fatalf("size %d, qemu says %d", src.Size(), len(want))
	}
	got := make([]byte, len(want))
	if _, err := src.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if i := firstDiff(got, want); i >= 0 {
		t.Fatalf("whole read differs from qemu at byte %d", i)
	}
	rng := rand.New(rand.NewSource(7))
	for range 200 {
		n := 1 + rng.Intn(70000)
		off := rng.Int63n(int64(len(want)))
		n = min(n, len(want)-int(off))
		piece := make([]byte, n)
		if _, err := src.ReadAt(piece, off); err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", n, off, err)
		}
		if !bytes.Equal(piece, want[off:int(off)+n]) {
			t.Fatalf("ReadAt(%d bytes at %d) differs from qemu", n, off)
		}
	}
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}
	return -1
}

func TestQCOW2MatchesQemu(t *testing.T) {
	needQemu(t)
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.raw")
	testDisk(t, raw, 3<<20+1000) // not a whole number of clusters
	cases := map[string][]string{
		"v3-64k":         {"-o", "cluster_size=65536"},
		"v2-compat":      {"-o", "compat=0.10"},
		"small-clusters": {"-o", "cluster_size=4096"},
		"big-clusters":   {"-o", "cluster_size=2097152"},
		"deflate":        {"-c"},
		"zstd":           {"-c", "-o", "compression_type=zstd"},
		"deflate-4k":     {"-c", "-o", "cluster_size=4096"},
		"preallocated":   {"-o", "preallocation=metadata"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			img := filepath.Join(dir, name+".qcow2")
			run(t, "qemu-img", append(append([]string{"convert", "-O", "qcow2"}, opts...), raw, img)...)
			sameAsQemu(t, img, "")
		})
	}
}

// A chain of overlays, with writes, zeroed ranges (the v3 zero flag) and
// an overlay larger than its backing file.
func TestQCOW2BackingChain(t *testing.T) {
	needQemu(t)
	dir := t.TempDir()
	testDisk(t, filepath.Join(dir, "base.raw"), 2<<20)
	run(t, "qemu-img", "convert", "-O", "qcow2", filepath.Join(dir, "base.raw"), filepath.Join(dir, "base.qcow2"))

	mid := filepath.Join(dir, "mid.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", "base.qcow2", mid)
	run(t, "qemu-io", "-c", "write -P 0xab 100k 200k", "-c", "write -z 1M 128k", mid)

	top := filepath.Join(dir, "top.qcow2")
	// Larger than its backing: the extra space reads as zeros until written.
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", "mid.qcow2", top, "3M")
	run(t, "qemu-io", "-c", "write -P 0x5a 150k 10k", "-c", "write -P 0x11 2600k 64k", "-c", "write -z 0 4k", top)
	sameAsQemu(t, top, "")

	// A raw base, recorded as raw in the overlay's header.
	over := filepath.Join(dir, "over-raw.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "raw", "-b", "base.raw", over)
	run(t, "qemu-io", "-c", "write -P 0x77 64k 64k", over)
	sameAsQemu(t, over, "")
}

func setIncompatibleBit(t *testing.T, path string, bit uint) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [8]byte
	f.ReadAt(b[:], 72)
	b[7-bit/8] |= 1 << (bit % 8)
	f.WriteAt(b[:], 72)
}

func TestQCOW2RefusesUnsafeImages(t *testing.T) {
	needQemu(t)
	dir := t.TempDir()
	testDisk(t, filepath.Join(dir, "d.raw"), 1<<20)
	for name, bit := range map[string]uint{"dirty": 0, "corrupt": 1} {
		img := filepath.Join(dir, name+".qcow2")
		run(t, "qemu-img", "convert", "-O", "qcow2", filepath.Join(dir, "d.raw"), img)
		setIncompatibleBit(t, img, bit)
		if _, err := Open(img, ""); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s image: %v", name, err)
		}
	}
}

// A guest that writes a qcow2 header into its own raw disk must not be
// able to make vmobj treat the disk as qcow2 and follow a backing file of
// the guest's choosing.
func TestFormatIsNotTakenFromContents(t *testing.T) {
	needQemu(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "host-secret")
	os.WriteFile(secret, []byte("not for backups"), 0o600)
	evil := filepath.Join(dir, "evil.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "raw", "-b", secret, evil, "1M")
	// The "guest disk" is a raw image whose contents are that qcow2 file.
	disk := filepath.Join(dir, "guest-disk.img")
	data, _ := os.ReadFile(evil)
	os.WriteFile(disk, append(data, make([]byte, 1<<20)...), 0o600)

	if _, err := Open(disk, ""); !errors.Is(err, ErrAmbiguousFormat) {
		t.Fatalf("Open of a raw disk holding a qcow2 header: %v, want ErrAmbiguousFormat", err)
	}
	src, err := Open(disk, "raw")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	head := make([]byte, 4)
	src.ReadAt(head, 0)
	if string(head) != "QFI\xfb" {
		t.Fatal("--format raw did not read the disk's own bytes")
	}
}

func TestOpenPicksFormat(t *testing.T) {
	needQemu(t)
	dir := t.TempDir()
	raw := filepath.Join(dir, "a.img")
	want := testDisk(t, raw, 256<<10)
	src, err := Open(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(*Raw); !ok {
		t.Fatalf("a.img opened as %T", src)
	}
	src.Close()
	q := filepath.Join(dir, "b.qcow2")
	run(t, "qemu-img", "convert", "-O", "qcow2", raw, q)
	src, err = Open(q, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(*QCOW2); !ok {
		t.Fatalf("b.qcow2 opened as %T", src)
	}
	got := make([]byte, len(want))
	src.ReadAt(got, 0)
	src.Close()
	if !bytes.Equal(got, want) {
		t.Fatal("qcow2 content differs from the raw original")
	}
	if _, err := Open(raw, "vmdk"); err == nil {
		t.Fatal("accepted an unknown format")
	}
}
