// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wilminator/vm-objects/internal/atomicfile"
	"github.com/wilminator/vm-objects/internal/extent"
)

// State is what the host remembers between runs for one VM. It lets an
// incremental compare extent hashes without reading anything back from
// the repository. It holds keyed hashes only, never disk data. Losing it
// just means the next run has to be a full.
type State struct {
	RepoID       string
	VM           string
	DiskSize     int64
	BlockSize    extent.Length
	ExtentSize   extent.Length
	Chain        string
	BaseManifest string       // the chain's full
	LastManifest string       // the most recent run
	Blocks       []extent.Ref // block refs of the chain's full
	Base         []extent.Ref // extent refs at the chain's full
	Current      []extent.Ref // extent refs at the most recent run
	// Locks remembers the Object Lock date this host last saw or set for
	// each object key it relies on, so incrementals needn't ask S3 about
	// every base block. Fulls re-check against S3 and refresh it. A stale
	// entry can only be earlier than the truth (locks never shorten), so
	// the worst it causes is an unneeded extension.
	Locks map[string]time.Time
}

// LoadState reads a state file. A missing file returns (nil, nil).
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&s); err != nil {
		return nil, fmt.Errorf("reading state %s: %w", path, err)
	}
	return &s, nil
}

// lockState takes an exclusive lock next to the state file and returns a
// function that releases it. A second run for the same VM fails at once
// instead of waiting. The lock disappears if the process dies.
func lockState(statePath string) (func(), error) {
	if err := atomicfile.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(statePath+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another backup of this VM is already running (lock %s)", f.Name())
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Save writes the state file atomically.
func (s *State) Save(path string) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(s); err != nil {
		return err
	}
	return atomicfile.Write(path, buf.Bytes(), 0o600)
}
