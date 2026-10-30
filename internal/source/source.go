// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package source opens a virtual disk for reading, as the guest sees it.
//
// This first version reads raw images and block devices, which covers a
// ZFS zvol (or a zvol snapshot) and any raw disk file. qcow2 and other
// container formats will come through a QEMU/NBD source later; until then,
// expose them as raw with qemu-nbd or convert them with qemu-img.
package source

import (
	"fmt"
	"io"
	"os"
)

// Source is a point-in-time disk the core reads from.
//
// Go note: an interface lists behavior, not data. Anything with ReadAt,
// Size and Close methods is a Source, so the core never needs to know
// whether it is reading a file, a zvol or (later) an NBD export.
type Source interface {
	io.ReaderAt
	Size() int64
	Close() error
}

// Raw reads a raw image file or a block device.
type Raw struct {
	f    *os.File
	size int64
}

// OpenRaw opens path read-only and finds its size. Seeking to the end
// works for both regular files and block devices, where Stat reports 0.
func OpenRaw(path string) (*Raw, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("finding size of %s: %w", path, err)
	}
	if size <= 0 {
		f.Close()
		return nil, fmt.Errorf("%s is empty", path)
	}
	return &Raw{f: f, size: size}, nil
}

// ReadAt reads len(p) bytes at off.
func (r *Raw) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }

// Size is the disk size in bytes.
func (r *Raw) Size() int64 { return r.size }

// Close releases the file.
func (r *Raw) Close() error { return r.f.Close() }
