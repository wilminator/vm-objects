// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package scan reads a disk block by block and hashes each block and the
// smaller extents inside it.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/hashing"
	"github.com/wilminator/vm-objects/internal/source"
)

// Options control a scan.
type Options struct {
	BlockSize  extent.Length // large blocks, for example 4 MiB
	ExtentSize extent.Length // small extents inside each block, for example 64 KiB
	Workers    int           // blocks hashed at the same time; 0 means one per CPU
	Hasher     hashing.Hasher
}

// Block is the result for one block, in disk order.
type Block struct {
	Index   int
	Offset  extent.Offset
	Length  extent.Length // shorter than BlockSize only for the last block
	Ref     extent.Ref    // the whole block
	Extents []extent.Ref  // one per extent inside the block
	Data    []byte        // the block's bytes; only valid during the callback
}

// Validate checks that the sizes fit together.
func (o Options) Validate() error {
	if o.BlockSize <= 0 || o.ExtentSize <= 0 {
		return errors.New("block and extent sizes must be positive")
	}
	if o.BlockSize%o.ExtentSize != 0 {
		return fmt.Errorf("block size %d is not a multiple of extent size %d", o.BlockSize, o.ExtentSize)
	}
	return nil
}

// ExtentCount is how many extents cover a disk of the given size.
func ExtentCount(diskSize int64, extentSize extent.Length) int {
	return int((diskSize + int64(extentSize) - 1) / int64(extentSize))
}

// BlockCount is how many blocks cover a disk of the given size.
func BlockCount(diskSize int64, blockSize extent.Length) int {
	return int((diskSize + int64(blockSize) - 1) / int64(blockSize))
}

// Run reads src from start to end and calls fn once per block, in order.
//
// It works in batches: read up to Workers blocks one after another, hash
// them all at once on separate goroutines, then hand them to fn in order.
// Go note: a goroutine is a lightweight thread started with the "go"
// keyword; sync.WaitGroup waits until a group of them has finished.
func Run(ctx context.Context, src source.Source, opts Options, fn func(Block) error) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	size := src.Size()
	total := BlockCount(size, opts.BlockSize)

	bufs := make([][]byte, workers)
	for i := range bufs {
		bufs[i] = make([]byte, opts.BlockSize)
	}
	batch := make([]Block, workers)

	for first := 0; first < total; first += workers {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(workers, total-first)

		// Read the batch sequentially; disks are fastest read in order.
		for i := 0; i < n; i++ {
			idx := first + i
			off := int64(idx) * int64(opts.BlockSize)
			length := min(int64(opts.BlockSize), size-off)
			buf := bufs[i][:length]
			if err := readFull(src, buf, off); err != nil {
				return fmt.Errorf("reading block %d at offset %d: %w", idx, off, err)
			}
			batch[i] = Block{Index: idx, Offset: extent.Offset(off), Length: extent.Length(length), Data: buf}
		}

		// Hash the batch in parallel.
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(b *Block) {
				defer wg.Done()
				hashBlock(b, opts)
			}(&batch[i])
		}
		wg.Wait()

		// Hand results over in disk order.
		for i := 0; i < n; i++ {
			if err := fn(batch[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func hashBlock(b *Block, opts Options) {
	b.Ref = opts.Hasher.Ref(b.Data)
	count := int((int64(b.Length) + int64(opts.ExtentSize) - 1) / int64(opts.ExtentSize))
	b.Extents = make([]extent.Ref, count)
	if b.Ref.Zero {
		for i := range b.Extents {
			b.Extents[i] = extent.Ref{Zero: true}
		}
		return
	}
	for i := 0; i < count; i++ {
		start := int64(i) * int64(opts.ExtentSize)
		end := min(start+int64(opts.ExtentSize), int64(b.Length))
		b.Extents[i] = opts.Hasher.Ref(b.Data[start:end])
	}
}

// readFull reads exactly len(buf) bytes at off. ReaderAt may return io.EOF
// together with a complete read at the end of the disk; that is success.
func readFull(r io.ReaderAt, buf []byte, off int64) error {
	n, err := r.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
