// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package atomicfile writes files so a crash leaves either the old file or
// the new one, never a half-written mix.
package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Write stores data at path: write a temporary file in the same folder,
// flush it to disk, rename it over the target, then flush the folder so
// the rename itself survives a power loss.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Go note: defer runs when the function returns, whichever way it
	// returns. Removing a file that was already renamed is a harmless no-op.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// MkdirAll creates dir and any missing parents, flushing each parent after
// a new entry is added to it. Without that, a power loss can drop a newly
// created folder even though files inside it were flushed.
func MkdirAll(dir string, perm os.FileMode) error {
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return &fs.PathError{Op: "mkdir", Path: dir, Err: errors.New("not a directory")}
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := MkdirAll(parent, perm); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return SyncDir(parent)
}

// SyncDir flushes a folder's entries to disk.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
