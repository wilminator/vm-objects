// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package target

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wilminator/vm-objects/internal/source"
)

// The writer is checked three ways: QEMU's own consistency check
// (qemu-img check), QEMU's reading of the guest disk (qemu-img compare
// against the expected raw image), and vmobj's reader.

func qemuImg(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	out, err := exec.Command("qemu-img", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// checkImage proves path reads as want, by QEMU and by vmobj.
func checkImage(t *testing.T, path string, want []byte) {
	t.Helper()
	qemuImg(t, "check", path)
	raw := path + ".expected.raw"
	if err := os.WriteFile(raw, want, 0o600); err != nil {
		t.Fatal(err)
	}
	qemuImg(t, "compare", "-f", "qcow2", "-F", "raw", path, raw)
	q, err := source.OpenQCOW2(path)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	got := make([]byte, len(want))
	if _, err := q.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("vmobj's reader sees different data")
	}
}

func write(t *testing.T, q *QCOW2, want []byte, off int, p []byte) {
	t.Helper()
	if _, err := q.WriteAt(p, int64(off)); err != nil {
		t.Fatal(err)
	}
	copy(want[off:], p)
}

func random(rng *rand.Rand, n int) []byte { b := make([]byte, n); rng.Read(b); return b }

func TestQCOW2Standalone(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(1))
	size := 10<<20 + 3072 // not a whole number of clusters
	want := make([]byte, size)
	q, err := CreateQCOW2(filepath.Join(dir, "a.qcow2"), int64(size), QCOW2Options{})
	if err != nil {
		t.Fatal(err)
	}
	write(t, q, want, 0, random(rng, 200<<10))      // whole clusters and a part
	write(t, q, want, 1<<20+100, random(rng, 5000)) // inside one cluster, unaligned
	write(t, q, want, 3<<20-10, random(rng, 20))    // across a cluster boundary
	write(t, q, want, 5<<20, make([]byte, 64<<10))  // zeros: no cluster needed
	write(t, q, want, size-1000, random(rng, 1000)) // the short last cluster
	write(t, q, want, 100<<10, random(rng, 4096))   // over an allocated cluster
	// It reads back what was written before Close, too.
	got := make([]byte, size)
	if _, err := q.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read before close: %v", err)
	}
	if q.Allocated() > 8 {
		t.Fatalf("%d clusters allocated; zeros and rewrites must not take more", q.Allocated())
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	checkImage(t, filepath.Join(dir, "a.qcow2"), want)
}

// An overlay holds only what was written, reads the rest through the
// backing file, and can zero a cluster the backing file has data in.
func TestQCOW2Overlay(t *testing.T) {
	for _, bformat := range []string{"qcow2", "raw"} {
		t.Run(bformat, func(t *testing.T) {
			dir := t.TempDir()
			rng := rand.New(rand.NewSource(2))
			size := 4 << 20
			base := random(rng, size)
			bpath := filepath.Join(dir, "base."+bformat)
			if bformat == "raw" {
				os.WriteFile(bpath, base, 0o600)
			} else {
				b, err := CreateQCOW2(bpath, int64(size), QCOW2Options{})
				if err != nil {
					t.Fatal(err)
				}
				b.WriteAt(base, 0)
				if err := b.Close(); err != nil {
					t.Fatal(err)
				}
			}
			want := bytes.Clone(base)
			opath := filepath.Join(dir, "base.1794103287") // named the way QNAP names its overlays
			q, err := CreateQCOW2(opath, int64(size), QCOW2Options{Backing: bpath, BackingFormat: bformat})
			if err != nil {
				t.Fatal(err)
			}
			write(t, q, want, 70<<10, random(rng, 3000))    // partial: the rest comes from the base
			write(t, q, want, 1<<20, make([]byte, 128<<10)) // two clusters zeroed over data
			write(t, q, want, 2<<20+5, make([]byte, 100))   // partial zeros over data: a real cluster
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
			// QEMU must know the backing file's format and find it by a
			// relative name.
			info := qemuImg(t, "info", "--backing-chain", opath)
			if !strings.Contains(info, "backing file: base."+bformat) || !strings.Contains(info, "backing file format: "+bformat) {
				t.Fatalf("qemu-img info:\n%s", info)
			}
			checkImage(t, opath, want)
			if fi, _ := os.Stat(opath); fi.Size() > 1<<20 {
				t.Fatalf("overlay is %d bytes; it should hold only the few clusters written", fi.Size())
			}
		})
	}
}

// Small clusters make a disk need several L2 tables and refcount blocks.
func TestQCOW2ManyTables(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(3))
	size := 24 << 20
	want := make([]byte, size)
	q, err := CreateQCOW2(filepath.Join(dir, "m.qcow2"), int64(size), QCOW2Options{ClusterBits: 12})
	if err != nil {
		t.Fatal(err)
	}
	// 4 KiB clusters: one L2 table covers 2 MiB, one refcount block 2048 clusters.
	write(t, q, want, 0, random(rng, 12<<20))
	write(t, q, want, 20<<20, random(rng, 9000))
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	checkImage(t, filepath.Join(dir, "m.qcow2"), want)
}

func TestQCOW2Refusals(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.qcow2")
	os.WriteFile(p, nil, 0o600)
	if _, err := CreateQCOW2(p, 1<<20, QCOW2Options{}); err == nil {
		t.Fatal("overwrote an existing file")
	}
	big := filepath.Join(dir, "big.raw")
	os.WriteFile(big, make([]byte, 2<<20), 0o600)
	if _, err := CreateQCOW2(filepath.Join(dir, "y"), 1<<20, QCOW2Options{Backing: big, BackingFormat: "raw"}); err == nil {
		t.Fatal("accepted a backing file larger than the image")
	}
	if _, err := CreateQCOW2(filepath.Join(dir, "z"), 1<<20, QCOW2Options{Backing: big}); err == nil {
		t.Fatal("accepted a backing file without a format")
	}
	if _, err := CreateQCOW2(filepath.Join(dir, "v"), 1<<20+100, QCOW2Options{}); err == nil {
		t.Fatal("accepted a size QEMU would read as another")
	}
	q, _ := CreateQCOW2(filepath.Join(dir, "w"), 1<<20, QCOW2Options{})
	if _, err := q.WriteAt([]byte{1}, 1<<20); err == nil {
		t.Fatal("wrote past the end")
	}
	q.Abort()
	if _, err := os.Stat(filepath.Join(dir, "w")); !os.IsNotExist(err) {
		t.Fatal("Abort left the file")
	}
}
