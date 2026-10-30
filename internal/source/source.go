// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package source opens a virtual disk for reading, as the guest sees it.
//
// It reads raw images, block devices (a ZFS zvol or zvol snapshot) and
// qcow2 images with their backing chains. Open picks the right reader by
// looking at the file.
package source

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// Open opens a disk as the guest sees it. format is "raw", "qcow2", or ""
// to decide safely:
//
//   - Block devices (zvols and the like) are raw.
//   - Files named *.qcow2 or *.qcow are qcow2; any other file is raw.
//   - A file that would be read as raw but starts with the qcow2 magic is
//     refused until the format is given explicitly.
//
// The format is never taken from the file's contents alone, because a
// raw disk's contents belong to the guest. A guest that writes a qcow2
// header into its own disk must not be able to make the host follow a
// "backing file" of its choosing (the classic QEMU format-probing hole).
func Open(path, format string) (Source, error) { return open(path, format, 0) }

func open(path, format string, depth int) (Source, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if format == "" {
		switch {
		case fi.Mode()&os.ModeDevice != 0:
			format = "raw"
		case hasExt(path, ".qcow2", ".qcow"):
			format = "qcow2"
		default:
			is, err := IsQCOW2(path)
			if err != nil {
				return nil, err
			}
			if is {
				return nil, fmt.Errorf("%w: %s", ErrAmbiguousFormat, path)
			}
			format = "raw"
		}
	}
	switch format {
	case "raw":
		return OpenRaw(path)
	case "qcow2":
		return openQCOW2(path, depth)
	default:
		return nil, fmt.Errorf("unknown disk format %q (want raw or qcow2)", format)
	}
}

// ErrAmbiguousFormat means a file has the qcow2 magic number but nothing
// else says it is qcow2, so vmobj won't trust it.
var ErrAmbiguousFormat = errors.New("starts like a qcow2 image but isn't named like one; " +
	"pass --format qcow2 if it really is, or --format raw to read it as a raw disk")

func hasExt(path string, exts ...string) bool {
	e := strings.ToLower(filepath.Ext(path))
	for _, x := range exts {
		if e == x {
			return true
		}
	}
	return false
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
