// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package hashing wraps BLAKE3 in the two forms vmobj uses: plain (for the
// measure command, which stores nothing remotely) and keyed (for repository
// content IDs, so the bucket owner cannot tell which blocks match known data).
package hashing

import (
	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/zeebo/blake3"
)

// Hasher computes content hashes. The zero value is a plain BLAKE3 hasher.
type Hasher struct {
	key   [32]byte
	keyed bool
}

// Plain returns an unkeyed BLAKE3 hasher.
func Plain() Hasher { return Hasher{} }

// Keyed returns a BLAKE3 hasher in keyed mode.
func Keyed(key [32]byte) Hasher { return Hasher{key: key, keyed: true} }

// Sum hashes b.
func (h Hasher) Sum(b []byte) extent.Hash {
	if !h.keyed {
		return extent.Hash(blake3.Sum256(b))
	}
	// NewKeyed only fails when the key is not 32 bytes, which the array
	// type rules out, so the error is impossible here.
	k, _ := blake3.NewKeyed(h.key[:])
	_, _ = k.Write(b)
	var out extent.Hash
	copy(out[:], k.Sum(nil))
	return out
}

// Ref hashes b, or marks it as zeros when every byte is zero.
func (h Hasher) Ref(b []byte) extent.Ref {
	if extent.IsAllZero(b) {
		return extent.Ref{Zero: true}
	}
	return extent.Ref{Hash: h.Sum(b)}
}
