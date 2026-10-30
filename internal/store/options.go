// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"context"
	"errors"
	"time"
)

// Optional backend features. A backend implements only the ones it can;
// the local folder implements none of them, and callers fall back to plain
// behaviour.
//
// Go note: to ask whether a value has an optional interface, use a type
// assertion: `if p, ok := b.(OptionPutter); ok { ... }`. ok is false when
// the backend lacks the methods, and nothing panics.

// PutOptions are per-object storage settings.
type PutOptions struct {
	// Class is the storage class ("DEEP_ARCHIVE", ...). Empty means the
	// backend's default, which is Standard on S3.
	Class string
}

// OptionPutter stores objects with PutOptions.
type OptionPutter interface {
	PutWith(ctx context.Context, key string, data []byte, opts PutOptions) error
}

// ObjectInfo describes a stored object without downloading it.
type ObjectInfo struct {
	Size     int64
	Modified time.Time
	Class    string // "" or "STANDARD" for Standard
	// Archived objects (Glacier Flexible Retrieval, Deep Archive) must be
	// restored before they can be read.
	Archived bool
	// Restoring is true while a restore request is still running.
	Restoring bool
	// RestoredUntil is when a finished restore's temporary copy expires.
	RestoredUntil time.Time
}

// Readable reports whether the object can be downloaded now.
func (i ObjectInfo) Readable() bool {
	return !i.Archived || (!i.Restoring && !i.RestoredUntil.IsZero())
}

// Statter reports an object's storage details.
type Statter interface {
	Stat(ctx context.Context, key string) (ObjectInfo, error)
}

// Restorer asks for a temporary readable copy of an archived object.
type Restorer interface {
	// RequestRestore keeps the copy for days days, using a retrieval tier
	// ("Expedited", "Standard" or "Bulk"). Asking again while a restore is
	// already running is not an error.
	RequestRestore(ctx context.Context, key string, days int, tier string) error
}

// ErrArchived is returned when reading an archived object that hasn't been
// restored yet.
var ErrArchived = errors.New("object is archived and must be restored before it can be read")

// PutWith stores data with opts when the backend supports options, and
// with a plain Put otherwise.
func PutWith(ctx context.Context, b Backend, key string, data []byte, opts PutOptions) error {
	if p, ok := b.(OptionPutter); ok {
		return p.PutWith(ctx, key, data, opts)
	}
	return b.Put(ctx, key, data)
}

// Stat returns an object's details. Backends without storage classes
// report every existing object as readable Standard storage.
func Stat(ctx context.Context, b Backend, key string) (ObjectInfo, error) {
	if s, ok := b.(Statter); ok {
		return s.Stat(ctx, key)
	}
	ok, err := b.Exists(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{}, nil
}
