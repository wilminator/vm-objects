// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package measure implements "vmobj measure": a read-only scan that records
// a hash for every block and extent of a disk, and reports how much changed
// since the previous run. It writes nothing to the disk and needs no
// repository, so it is safe to run against a live system's snapshot.
package measure

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wilminator/vm-objects/internal/atomicfile"
	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/hashing"
	"github.com/wilminator/vm-objects/internal/scan"
	"github.com/wilminator/vm-objects/internal/source"
)

// HashList is one run's result, saved between runs.
type HashList struct {
	Created    time.Time
	DiskSize   int64
	BlockSize  extent.Length
	ExtentSize extent.Length
	Blocks     []extent.Ref
	Extents    []extent.Ref
}

// Run scans src and returns its hash list.
func Run(ctx context.Context, src source.Source, opts scan.Options) (*HashList, error) {
	opts.Hasher = hashing.Plain()
	hl := &HashList{
		Created:    time.Now().UTC(),
		DiskSize:   src.Size(),
		BlockSize:  opts.BlockSize,
		ExtentSize: opts.ExtentSize,
		Blocks:     make([]extent.Ref, 0, scan.BlockCount(src.Size(), opts.BlockSize)),
		Extents:    make([]extent.Ref, 0, scan.ExtentCount(src.Size(), opts.ExtentSize)),
	}
	err := scan.Run(ctx, src, opts, func(b scan.Block) error {
		hl.Blocks = append(hl.Blocks, b.Ref)
		hl.Extents = append(hl.Extents, b.Extents...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hl, nil
}

// Save writes the hash list to path.
// Go note: encoding/gob is Go's own binary format. It is compact and
// needs no schema code, which suits a file only vmobj itself reads.
func (hl *HashList) Save(path string) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(hl); err != nil {
		return err
	}
	// 0600: plain hashes could confirm whether the disk holds known files.
	return atomicfile.Write(path, buf.Bytes(), 0o600)
}

// Load reads a hash list saved by Save. A missing file returns
// os.ErrNotExist, which callers treat as "first run".
func Load(path string) (*HashList, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hl HashList
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&hl); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return &hl, nil
}

// Stats summarises one hash list on its own.
type Stats struct {
	Blocks, ZeroBlocks   int
	Extents, ZeroExtents int
	DataBytes            int64 // bytes in extents that are not all zeros
}

// Summarise counts zero and non-zero content.
func (hl *HashList) Summarise() Stats {
	s := Stats{Blocks: len(hl.Blocks), Extents: len(hl.Extents)}
	for _, r := range hl.Blocks {
		if r.Zero {
			s.ZeroBlocks++
		}
	}
	for i, r := range hl.Extents {
		if r.Zero {
			s.ZeroExtents++
			continue
		}
		s.DataBytes += extentLen(hl, i)
	}
	return s
}

// Change reports what changed between two runs, at both sizes.
type Change struct {
	Since              time.Time
	ChangedBlocks      int
	ChangedBlockBytes  int64 // what a full at block size would upload
	ChangedExtents     int
	ChangedExtentBytes int64 // what an incremental at extent size would upload
}

// ErrIncompatible means the two runs can't be compared.
var ErrIncompatible = errors.New("previous run used a different disk size or block sizes")

// Compare counts changed blocks and extents between prev and cur.
// A block or extent that became all zeros counts as changed, but stores no data.
func Compare(prev, cur *HashList) (Change, error) {
	if prev.DiskSize != cur.DiskSize || prev.BlockSize != cur.BlockSize || prev.ExtentSize != cur.ExtentSize ||
		len(prev.Blocks) != len(cur.Blocks) || len(prev.Extents) != len(cur.Extents) {
		return Change{}, ErrIncompatible
	}
	c := Change{Since: prev.Created}
	for i := range cur.Blocks {
		if !cur.Blocks[i].Equal(prev.Blocks[i]) {
			c.ChangedBlocks++
			if !cur.Blocks[i].Zero {
				c.ChangedBlockBytes += blockLen(cur, i)
			}
		}
	}
	for i := range cur.Extents {
		if !cur.Extents[i].Equal(prev.Extents[i]) {
			c.ChangedExtents++
			if !cur.Extents[i].Zero {
				c.ChangedExtentBytes += extentLen(cur, i)
			}
		}
	}
	return c, nil
}

func blockLen(hl *HashList, i int) int64 {
	start := int64(i) * int64(hl.BlockSize)
	return min(int64(hl.BlockSize), hl.DiskSize-start)
}

func extentLen(hl *HashList, i int) int64 {
	start := int64(i) * int64(hl.ExtentSize)
	return min(int64(hl.ExtentSize), hl.DiskSize-start)
}
