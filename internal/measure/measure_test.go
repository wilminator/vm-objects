// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package measure

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/wilminator/vm-objects/internal/scan"
	"github.com/wilminator/vm-objects/internal/source"
)

// writeImage creates a test disk image. Go note: functions whose names
// start with Test and take *testing.T are run by "go test".
func writeImage(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func measure(t *testing.T, path string, opts scan.Options) *HashList {
	t.Helper()
	src, err := source.OpenRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	hl, err := Run(context.Background(), src, opts)
	if err != nil {
		t.Fatal(err)
	}
	return hl
}

func TestMeasureCountsChanges(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.raw")
	opts := scan.Options{BlockSize: 64 << 10, ExtentSize: 4 << 10, Workers: 3}

	// 1 MiB plus a partial block: 16 full blocks and one of 1000 bytes.
	data := make([]byte, 1<<20+1000)
	rand.New(rand.NewSource(1)).Read(data[:512<<10]) // second half stays zero
	writeImage(t, img, data)
	first := measure(t, img, opts)

	st := first.Summarise()
	if st.Blocks != 17 || st.ZeroBlocks != 9 {
		t.Fatalf("blocks=%d zero=%d, want 17 and 9", st.Blocks, st.ZeroBlocks)
	}
	if st.DataBytes != 512<<10 {
		t.Fatalf("data bytes = %d, want %d", st.DataBytes, 512<<10)
	}

	// Change one byte in block 2 and one byte in the zero area (block 10).
	data[2*(64<<10)+5] ^= 0xFF
	data[10*(64<<10)+100] = 7
	writeImage(t, img, data)
	second := measure(t, img, opts)

	ch, err := Compare(first, second)
	if err != nil {
		t.Fatal(err)
	}
	if ch.ChangedBlocks != 2 || ch.ChangedExtents != 2 {
		t.Fatalf("changed blocks=%d extents=%d, want 2 and 2", ch.ChangedBlocks, ch.ChangedExtents)
	}
	if ch.ChangedExtentBytes != 2*(4<<10) || ch.ChangedBlockBytes != 2*(64<<10) {
		t.Fatalf("changed bytes: extents=%d blocks=%d", ch.ChangedExtentBytes, ch.ChangedBlockBytes)
	}

	// Saving and loading round-trips.
	state := filepath.Join(dir, "state", "disk.hashes")
	if err := second.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(state)
	if err != nil {
		t.Fatal(err)
	}
	if ch2, err := Compare(loaded, second); err != nil || ch2.ChangedExtents != 0 {
		t.Fatalf("reloaded list differs: %+v %v", ch2, err)
	}
}

func TestCompareRejectsDifferentSizes(t *testing.T) {
	a := &HashList{DiskSize: 10, BlockSize: 4, ExtentSize: 2}
	b := &HashList{DiskSize: 10, BlockSize: 8, ExtentSize: 2}
	if _, err := Compare(a, b); err != ErrIncompatible {
		t.Fatalf("got %v, want ErrIncompatible", err)
	}
}
