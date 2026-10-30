// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package keys manages the master key file and the keys derived from it.
//
// The master key is 256 random bits stored as hex in a file readable only
// by its owner. It never appears on the command line, in logs or in the
// repository. Separate keys for encryption and for content IDs are derived
// from it with HKDF, so one key is never used for two purposes.
package keys

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Set holds the derived keys. Go note: fixed-size arrays ([32]byte) are
// values, so copying a Set copies the keys; there is no shared slice.
type Set struct {
	Data   [32]byte // AES-256-GCM key for sealing objects
	ID     [32]byte // BLAKE3 key for content IDs
	Config [32]byte // HMAC key that authenticates the repository's config.json
	// KeyID names the key without revealing it, so a repository can refuse
	// a wrong key file instead of failing on every object.
	KeyID string
}

// Generate creates a new key file. It refuses to overwrite an existing
// file, because replacing the key makes every backup made with it unreadable.
func Generate(path string) error {
	var master [32]byte
	if _, err := rand.Read(master[:]); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite a key", path)
		}
		return err
	}
	if _, err := f.WriteString(hex.EncodeToString(master[:]) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads a key file and derives the key set. It refuses files that
// other users can read.
func Load(path string) (Set, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Set{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Set{}, fmt.Errorf("%s is readable by other users (mode %v); run: chmod 400 %s", path, info.Mode().Perm(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Set{}, err
	}
	master, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(master) != 32 {
		return Set{}, fmt.Errorf("%s does not hold a 64-character hex key", path)
	}
	return Derive(master)
}

// Derive turns master key material into the key set.
func Derive(master []byte) (Set, error) {
	var s Set
	data, err := hkdf.Key(sha256.New, master, nil, "vmobj v1 data", 32)
	if err != nil {
		return s, err
	}
	id, err := hkdf.Key(sha256.New, master, nil, "vmobj v1 content-id", 32)
	if err != nil {
		return s, err
	}
	cfg, err := hkdf.Key(sha256.New, master, nil, "vmobj v1 config-mac", 32)
	if err != nil {
		return s, err
	}
	name, err := hkdf.Key(sha256.New, master, nil, "vmobj v1 key-id", 8)
	if err != nil {
		return s, err
	}
	copy(s.Data[:], data)
	copy(s.ID[:], id)
	copy(s.Config[:], cfg)
	s.KeyID = hex.EncodeToString(name)
	return s, nil
}
