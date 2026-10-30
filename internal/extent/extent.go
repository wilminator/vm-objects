// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package extent holds the small value types the rest of vmobj is built on.
//
// Go note: "type Offset int64" creates a new named type whose underlying
// type is int64. Go will not silently convert between Offset and Length,
// even though both are int64 underneath, so passing a length where an
// offset belongs is a compile error. That is the safeguard we chose Go for.
package extent

import (
	"encoding/hex"
	"fmt"
)

// Offset is a byte position on a virtual disk, as the guest sees it.
type Offset int64

// Length is a size in bytes.
type Length int64

// Hash identifies the contents of a block or extent. Inside a repository it
// is a keyed BLAKE3 hash, so it reveals nothing to someone without the key.
type Hash [32]byte

// String returns the hash as 64 lowercase hex characters.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// MarshalText lets encoding/json write a Hash as a hex string.
// Go note: any type with this method satisfies encoding.TextMarshaler, and
// the JSON encoder uses it automatically. Interfaces in Go are satisfied
// implicitly; there is no "implements" keyword.
func (h Hash) MarshalText() ([]byte, error) {
	out := make([]byte, hex.EncodedLen(len(h)))
	hex.Encode(out, h[:])
	return out, nil
}

// UnmarshalText reads a Hash back from hex.
func (h *Hash) UnmarshalText(text []byte) error {
	parsed, err := ParseHash(string(text))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// ParseHash converts 64 hex characters into a Hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if hex.DecodedLen(len(s)) != len(h) {
		return h, fmt.Errorf("hash %q: want %d hex characters, got %d", s, 2*len(h), len(s))
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("hash %q: %w", s, err)
	}
	return h, nil
}

// IsAllZero reports whether every byte in b is zero. Runs of zeros are
// recorded as a marker in manifests and never stored.
func IsAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Ref describes one piece of disk content: either a run of zeros, or data
// identified by its hash.
type Ref struct {
	Zero bool `json:"z,omitempty"`
	Hash Hash `json:"h"`
}

// Equal reports whether two refs describe the same content.
func (r Ref) Equal(o Ref) bool {
	if r.Zero || o.Zero {
		return r.Zero == o.Zero
	}
	return r.Hash == o.Hash
}
