// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
)

// The lock rule
//
// A backup is only as safe as the oldest object it points at. So when a
// repository uses Object Lock, every run makes sure that each object its
// manifest needs is locked at least until the new backup's keep-until date
// plus a margin ("need"). Objects that fall short are handled one of two
// ways:
//
//   - Refresh: during a full, a reused base block older than the profile's
//     refresh age is uploaded again. The fresh copy gets a fresh lock, and
//     the old version is left to expire and be pruned.
//   - Extend: otherwise the existing lock is moved later.
//
// New and extended locks run to "set" = keep-until + one full interval +
// margin. The extra full interval means the runs between now and the next
// full usually find their objects already covered, so extensions are rare.
//
// The manifest itself is locked until keep-until plus the margin, and it is
// written last, so a manifest never exists while an object it needs is
// unprotected.

// ErrPolicyNeeded means a locked repository was given no profile, so
// there is no retention to lock for.
var ErrPolicyNeeded = errors.New("this repository uses Object Lock; a backup needs a profile to know how long to lock for")

// locks applies the lock rule for one run. The zero value (enabled false)
// does nothing, which is how repositories without Object Lock behave.
type locks struct {
	enabled    bool
	backend    store.Backend
	locker     store.Locker
	now        time.Time
	need       time.Time // reused objects must be locked at least this long
	set        time.Time // new and extended locks run to here
	manifest   time.Time // the manifest's own lock
	refreshAge time.Duration
	// cache is the per-host record of lock dates (State.Locks).
	mu    sync.Mutex
	cache map[string]time.Time

	Extended  int // locks moved later this run
	Refreshed int // objects uploaded again because of their age
}

// newLocks works out this run's dates from the profile.
func newLocks(r *repo.Repo, prof *policy.Profile, started, retainUntil time.Time, cache map[string]time.Time) (*locks, error) {
	if !r.Config.ObjectLock {
		return &locks{}, nil
	}
	if prof == nil {
		return nil, ErrPolicyNeeded
	}
	l, ok := r.Backend.(store.Locker)
	if !ok {
		return nil, repo.ErrNoLocking
	}
	if cache == nil {
		cache = map[string]time.Time{}
	}
	return &locks{
		enabled: true, backend: r.Backend, locker: l, now: started,
		need:       retainUntil.Add(prof.Margin),
		set:        retainUntil.Add(prof.LookAhead() + prof.Margin),
		manifest:   retainUntil.Add(prof.Margin),
		refreshAge: prof.RefreshAge,
		cache:      cache,
	}, nil
}

// newObject returns the lock date for an object written this run.
func (l *locks) newObject() time.Time {
	if !l.enabled {
		return time.Time{}
	}
	return l.set
}

// manifestLock returns the lock date for this run's manifest.
func (l *locks) manifestLock() time.Time {
	if !l.enabled {
		return time.Time{}
	}
	return l.manifest
}

// wrote records a lock this run set on a new object.
func (l *locks) wrote(key string) {
	if !l.enabled {
		return
	}
	l.mu.Lock()
	l.cache[key] = l.set
	l.mu.Unlock()
}

// cached reports whether this host's record already shows key covered.
func (l *locks) cached(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.cache[key].Before(l.need)
}

// Decision is what to do with a reused object.
type decision int

const (
	keep    decision = iota // already locked long enough
	extend                  // lock moved later
	refresh                 // caller must upload it again
)

// ensure makes key's lock last until at least need. With verify, it asks
// the storage even when the cache says the lock is fine (fulls do this).
// With mayRefresh, an object older than the refresh age is reported as
// needing a fresh upload instead of an extension; the caller then writes
// it again with newObject's lock and calls wrote.
func (l *locks) ensure(ctx context.Context, key string, verify, mayRefresh bool) (decision, error) {
	if !l.enabled {
		return keep, nil
	}
	if !verify && l.cached(key) {
		return keep, nil
	}
	info, err := store.Stat(ctx, l.backend, key)
	if err != nil {
		return keep, fmt.Errorf("checking the lock on %s: %w", key, err)
	}
	if mayRefresh && l.refreshAge > 0 && !info.Modified.IsZero() && l.now.Sub(info.Modified) >= l.refreshAge {
		l.mu.Lock()
		l.Refreshed++
		l.mu.Unlock()
		return refresh, nil
	}
	if !info.LockUntil.Before(l.need) {
		l.mu.Lock()
		l.cache[key] = info.LockUntil
		l.mu.Unlock()
		return keep, nil
	}
	if err := l.locker.ExtendLock(ctx, key, l.set); err != nil {
		return keep, err
	}
	l.mu.Lock()
	l.cache[key] = l.set
	l.Extended++
	l.mu.Unlock()
	return extend, nil
}

// ensureAll applies ensure (without refresh) to many keys concurrently.
func (l *locks) ensureAll(ctx context.Context, keys []string, verify bool) error {
	if !l.enabled || len(keys) == 0 {
		return nil
	}
	return forEach(ctx, len(keys), func(i int) error {
		_, err := l.ensure(ctx, keys[i], verify, false)
		return err
	})
}
