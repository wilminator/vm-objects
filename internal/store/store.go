// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package store defines where objects live. A backend only moves opaque,
// already-encrypted bytes; it never sees plaintext or keys.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

// Backend stores objects under slash-separated keys such as
// "blocks/ab/abcd...". The local folder backend implements it now; S3
// will implement the same interface, so the engine works with either.
type Backend interface {
	// Put stores data under key, replacing any previous object atomically.
	Put(ctx context.Context, key string, data []byte) error
	// Get returns a whole object.
	Get(ctx context.Context, key string) ([]byte, error)
	// GetRange returns length bytes starting at offset.
	GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error)
	// Exists reports whether key is present.
	Exists(ctx context.Context, key string) (bool, error)
	// List returns all keys that start with prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// CheckKey rejects keys that could escape the repository or are ambiguous.
func CheckKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("invalid object key %q", key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".tmp-") {
			return fmt.Errorf("invalid object key %q", key)
		}
	}
	return nil
}
