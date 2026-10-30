// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scan

import (
	"bytes"
	"context"
	"testing"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/hashing"
)

// memSource is an in-memory disk for tests.
type memSource struct{ *bytes.Reader }

func (m memSource) Size() int64  { return m.Reader.Size() }
func (m memSource) Close() error { return nil }

func TestBlocksInOrderWithShortTail(t *testing.T) {
	data := make([]byte, 10*1024+300) // 10 KiB plus a short tail
	for i := range data {
		data[i] = byte(i % 251)
	}
	opts := Options{BlockSize: 2048, ExtentSize: 512, Workers: 3, Hasher: hashing.Plain()}
	var got []Block
	err := Run(context.Background(), memSource{bytes.NewReader(data)}, opts, func(b Block) error {
		b.Data = nil // not valid after the callback
		got = append(got, b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d blocks, want 6", len(got))
	}
	for i, b := range got {
		if b.Index != i || b.Offset != extent.Offset(i*2048) {
			t.Fatalf("block %d out of order: %+v", i, b)
		}
	}
	last := got[5]
	if last.Length != 300 || len(last.Extents) != 1 {
		t.Fatalf("tail block: length %d, %d extents", last.Length, len(last.Extents))
	}
	want := opts.Hasher.Ref(data[10*1024:])
	if !last.Extents[0].Equal(want) {
		t.Fatal("tail extent hash does not cover exactly the tail bytes")
	}
	if ExtentCount(int64(len(data)), 512) != 21 || BlockCount(int64(len(data)), 2048) != 6 {
		t.Fatal("ExtentCount or BlockCount wrong")
	}
}

func TestRejectsBadSizes(t *testing.T) {
	src := memSource{bytes.NewReader(make([]byte, 100))}
	for _, o := range []Options{{BlockSize: 0, ExtentSize: 1}, {BlockSize: 1000, ExtentSize: 300}} {
		if err := Run(context.Background(), src, o, func(Block) error { return nil }); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
}
