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
	"sort"
	"sync"

	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
)

// Readiness says whether every object a restore needs can be read now.
// Objects in Glacier Flexible Retrieval or Deep Archive must first be
// restored (copied back temporarily), which takes minutes to two days.
type Readiness struct {
	Objects   int // distinct objects the restore reads
	Ready     int
	Restoring int // restore requested, not finished
	// Pending are archived objects with no restore requested yet.
	Pending      []string
	PendingBytes int64
	// Classes holds the archive classes involved, which set the wait.
	Classes []policy.Class
}

// AllReady reports whether the restore can start.
func (rd *Readiness) AllReady() bool { return rd.Ready == rd.Objects }

// Wait estimates how long until everything is readable at a tier.
func (rd *Readiness) Wait(t policy.Tier) string {
	d, c := policy.Slowest(rd.Classes, t)
	if d == 0 {
		return "none"
	}
	ci, _ := policy.Info(c)
	return fmt.Sprintf("about %s (set by %s)", d, ci.Name)
}

// NotReadyError is returned by Restore when archived objects need a
// restore request first. It carries the details for the caller to show.
type NotReadyError struct{ Readiness *Readiness }

func (e *NotReadyError) Error() string {
	rd := e.Readiness
	return fmt.Sprintf("%d of %d objects are archived (%d waiting for a restore request, %d restoring)",
		rd.Objects-rd.Ready, rd.Objects, len(rd.Pending), rd.Restoring)
}

// neededKeys lists every object a manifest's restore reads.
func neededKeys(ctx context.Context, r *repo.Repo, vm, name string) ([]string, error) {
	m, err := r.GetManifest(ctx, vm, name)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, ref := range m.Blocks {
		if !ref.Zero {
			seen[repo.BlockKey(ref.Hash)] = true
		}
	}
	if len(m.Overlay) > 0 {
		idx, err := r.ChainIndex(ctx, m.Chain)
		if err != nil {
			return nil, err
		}
		for _, ov := range m.Overlay {
			if ov.Ref.Zero {
				continue
			}
			if loc, ok := idx[ov.Ref.Hash]; ok {
				seen[loc.Pack] = true
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// CheckReady looks at the storage state of every object a restore needs.
// Backends without storage classes are always ready and cost nothing to
// check; on S3 this makes one HEAD request per object.
func CheckReady(ctx context.Context, r *repo.Repo, vm, name string) (*Readiness, error) {
	keys, err := neededKeys(ctx, r, vm, name)
	if err != nil {
		return nil, err
	}
	rd := &Readiness{Objects: len(keys)}
	if _, ok := r.Backend.(store.Statter); !ok {
		rd.Ready = len(keys)
		return rd, nil
	}
	infos := make([]store.ObjectInfo, len(keys))
	err = forEach(ctx, len(keys), func(i int) error {
		info, err := store.Stat(ctx, r.Backend, keys[i])
		if err != nil {
			return fmt.Errorf("%s: %w", keys[i], err)
		}
		infos[i] = info
		return nil
	})
	if err != nil {
		return nil, err
	}
	classes := map[policy.Class]bool{}
	for i, info := range infos {
		switch {
		case info.Readable():
			rd.Ready++
			continue
		case info.Restoring:
			rd.Restoring++
		default:
			rd.Pending = append(rd.Pending, keys[i])
			rd.PendingBytes += info.Size
		}
		classes[policy.Class(info.Class)] = true
	}
	for c := range classes {
		rd.Classes = append(rd.Classes, c)
	}
	sort.Slice(rd.Classes, func(i, j int) bool { return rd.Classes[i] < rd.Classes[j] })
	return rd, nil
}

// RequestRestores asks the backend to restore every pending object for
// days days at tier t, and returns how many requests were made.
func RequestRestores(ctx context.Context, r *repo.Repo, rd *Readiness, days int, t policy.Tier) (int, error) {
	rs, ok := r.Backend.(store.Restorer)
	if !ok {
		return 0, errors.New("this repository's storage has no archive classes to restore from")
	}
	err := forEach(ctx, len(rd.Pending), func(i int) error {
		return rs.RequestRestore(ctx, rd.Pending[i], days, string(t))
	})
	if err != nil {
		return 0, err
	}
	return len(rd.Pending), nil
}

// forEach runs fn(0..n-1) on up to 16 goroutines and returns the first
// error. Requests to S3 are mostly waiting on the network, so running
// several at once makes checking thousands of objects fast.
//
// Go note: the buffered channel works as a semaphore: sending takes a
// slot, receiving gives it back, and a full channel makes the loop wait.
func forEach(ctx context.Context, n int, fn func(i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		slots    = make(chan struct{}, 16)
	)
	for i := 0; i < n; i++ {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(i); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// group runs functions in the background with a limit on how many run at
// once, and remembers the first error. Go stops accepting work once one
// has failed, so a scan can stop early.
type group struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	slots  chan struct{}
	mu     sync.Mutex
	err    error
}

func newGroup(ctx context.Context, limit int) *group {
	ctx, cancel := context.WithCancel(ctx)
	return &group{ctx: ctx, cancel: cancel, slots: make(chan struct{}, limit)}
}

// Go waits for a free slot, then runs fn in the background.
func (g *group) Go(fn func(ctx context.Context) error) error {
	select {
	case g.slots <- struct{}{}:
	case <-g.ctx.Done():
		return g.firstErr()
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() { <-g.slots }()
		if err := fn(g.ctx); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				g.cancel()
			}
			g.mu.Unlock()
		}
	}()
	return nil
}

func (g *group) firstErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	return g.ctx.Err()
}

// Wait waits for everything started and returns the first error.
func (g *group) Wait() error {
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancel()
	return g.err
}
