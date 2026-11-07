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
	// LockUntil, when set, locks the object in Governance mode until then:
	// it can't be deleted or overwritten-in-place, and its lock can't be
	// shortened, without a bypass permission vmobj never uses.
	LockUntil time.Time
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
	// LockUntil is the Object Lock retain-until date; zero when unlocked.
	LockUntil time.Time
	// VersionID identifies this version in a versioned bucket.
	VersionID string
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

// Locker manages Object Lock on the current version of objects.
type Locker interface {
	// LockingEnabled reports whether the bucket has Object Lock turned on.
	LockingEnabled(ctx context.Context) (bool, error)
	// ExtendLock moves an object's lock later. It never shortens a lock:
	// asking for an earlier date than the current one is an error.
	ExtendLock(ctx context.Context, key string, until time.Time) error
}

// Version is one stored version of a key in a versioned bucket.
type Version struct {
	Key          string
	VersionID    string
	Latest       bool
	DeleteMarker bool
	Modified     time.Time
	Size         int64
}

// Versioned gives access to every version of every key. Buckets with
// Object Lock are always versioned: a plain delete only hides an object
// behind a delete marker, and the data stays until each version is
// deleted by ID once its lock has run out.
type Versioned interface {
	ListVersions(ctx context.Context, prefix string) ([]Version, error)
	StatVersion(ctx context.Context, key, versionID string) (ObjectInfo, error)
	// DeleteVersion removes one version permanently. A locked version is
	// refused with ErrLocked; vmobj never asks to bypass a lock.
	DeleteVersion(ctx context.Context, key, versionID string) error
}

// ErrLocked means an object version is protected by Object Lock.
var ErrLocked = errors.New("object version is locked")

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
