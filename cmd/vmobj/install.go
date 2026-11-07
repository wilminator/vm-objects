// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wilminator/vm-objects/internal/atomicfile"
)

// vmobj install copies the running binary into a folder. Its main use is
// getting vmobj out of the container image, which has no shell to run cp:
//
//	docker run --rm --user "$(id -u):$(id -g)" -v "$PWD":/out ghcr.io/wilminator/vmobj install /out
//
// The copy is written under a temporary name and renamed into place, so a
// binary that is running, or an install that fails partway, never leaves
// a broken file behind.

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	name := fs.String("name", "vmobj", "file name to install as")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: vmobj install [--name NAME] DIR")
		fmt.Fprintln(os.Stderr, "Copies this vmobj binary to DIR/NAME (mode 0755), replacing any file already there.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("install needs exactly one folder")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this program's own file: %w", err)
	}
	res, err := installSelf(self, fs.Arg(0), *name)
	if err != nil {
		return err
	}
	switch {
	case res.unchanged:
		fmt.Printf("%s is already this vmobj %s; nothing to do.\n", res.path, version)
	case res.replaced:
		fmt.Printf("Installed vmobj %s to %s (replaced the file that was there).\n", version, res.path)
	default:
		fmt.Printf("Installed vmobj %s to %s.\n", version, res.path)
	}
	return nil
}

type installResult struct {
	path      string
	replaced  bool // a different file was there before
	unchanged bool // the same bytes were already there; nothing written
}

// installSelf copies the file at self to dir/name.
func installSelf(self, dir, name string) (installResult, error) {
	if name == "" || name != filepath.Base(name) {
		return installResult{}, fmt.Errorf("--name must be a plain file name, not %q", name)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return installResult{}, fmt.Errorf("%s: %w (is the folder mounted?)", dir, err)
	}
	if !info.IsDir() {
		return installResult{}, fmt.Errorf("%s is not a folder", dir)
	}
	dest := filepath.Join(dir, name)
	res := installResult{path: dest}

	// Resolve links so that installing onto the running file itself is
	// noticed, not attempted.
	realSelf, err := filepath.EvalSymlinks(self)
	if err != nil {
		return res, err
	}
	data, err := os.ReadFile(realSelf)
	if err != nil {
		return res, fmt.Errorf("reading this program's own file: %w", err)
	}
	if existing, err := os.Stat(dest); err == nil {
		if !existing.Mode().IsRegular() {
			return res, fmt.Errorf("%s exists and is not a regular file", dest)
		}
		if realDest, err := filepath.EvalSymlinks(dest); err == nil && realDest == realSelf {
			res.unchanged = true
			return res, nil
		}
		old, err := os.ReadFile(dest)
		if err == nil && bytes.Equal(old, data) {
			res.unchanged = true
			return res, nil
		}
		res.replaced = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	if err := atomicfile.Write(dest, data, 0o755); err != nil {
		return res, fmt.Errorf("writing %s: %w", dest, err)
	}
	return res, nil
}
