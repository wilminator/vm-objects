// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package engine runs backups, restores and verification.
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/scan"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/store"
)

// BackupOptions describe one backup run.
type BackupOptions struct {
	Repo      *repo.Repo
	Source    source.Source
	VM        string
	Kind      manifest.Kind
	StatePath string // per-VM state file on this host
	Workers   int
	Now       func() time.Time // for tests; defaults to time.Now

	// Profile sets this run's storage class and retention. Nil means no
	// profile: data goes to the backend's default class and the manifest
	// records no retention.
	Profile *policy.Profile
	// Class, when set, overrides the profile's class for this run's data.
	Class policy.Class
}

// placement works out where this run's data goes and how long it is kept.
func (o BackupOptions) placement(started time.Time) (store.PutOptions, *manifest.Manifest) {
	m := &manifest.Manifest{}
	class := o.Class
	if o.Profile != nil {
		rule := o.Profile.Rule(o.Kind)
		if class == "" {
			class = rule.Class
		}
		m.Profile = o.Profile.Name
		m.RetainUntil = started.Add(rule.Retain)
	}
	// Record a class only where the storage really has classes, so a local
	// folder's manifests don't claim Deep Archive.
	if _, ok := o.Repo.Backend.(store.OptionPutter); !ok {
		class = ""
	}
	m.DataClass = string(class)
	return store.PutOptions{Class: string(class)}, m
}

// BackupStats summarise a run.
type BackupStats struct {
	Manifest       string
	DiskBytes      int64
	ZeroBlocks     int
	BlocksStored   int // base blocks written this run
	BlocksReused   int // base blocks already in the repository
	ExtentsChanged int // overlay extents in the new manifest
	ExtentsStored  int // extents written into packs this run
	ExtentsReused  int // extents found in earlier packs of the chain
	Packs          int
	BytesWritten   int64 // sealed bytes sent to the backend
	LocksExtended  int   // Object Lock: existing locks moved later
	BlocksRefresh  int   // Object Lock: old blocks uploaded again
}

// ErrNeedFull means an incremental or differential can't run yet.
var ErrNeedFull = errors.New("no usable full backup for this VM on this host; run a full first")

// Backup runs one backup and writes its manifest last.
func Backup(ctx context.Context, o BackupOptions) (*BackupStats, error) {
	if err := manifest.CheckVMName(o.VM); err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// One run per VM at a time on this host. Two overlapping runs could
	// otherwise roll the state file back or pick the same manifest name.
	unlock, err := lockState(o.StatePath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	st, err := LoadState(o.StatePath)
	if err != nil {
		return nil, err
	}
	if st != nil && (st.RepoID != o.Repo.Config.RepoID || st.VM != o.VM) {
		return nil, fmt.Errorf("state file %s belongs to another repository or VM", o.StatePath)
	}
	started := o.Now().UTC() // names and dates the restore point
	if o.Kind == manifest.Full {
		return backupFull(ctx, o, st, started)
	}
	if !st.usableFor(o.Source.Size(), o.Repo.Config) {
		return nil, ErrNeedFull
	}
	return backupOverlay(ctx, o, st, started)
}

// usableFor reports whether the state matches this disk and repository and
// is internally whole, so an incremental can rely on it. A damaged state
// file asks for a full instead of crashing.
func (s *State) usableFor(diskSize int64, cfg repo.Config) bool {
	if s == nil || s.DiskSize != diskSize || s.BlockSize != cfg.BlockSize || s.ExtentSize != cfg.ExtentSize {
		return false
	}
	return len(s.Blocks) == scan.BlockCount(diskSize, cfg.BlockSize) &&
		len(s.Base) == scan.ExtentCount(diskSize, cfg.ExtentSize) &&
		s.Chain != "" && s.BaseManifest != ""
}

// uniqueName picks a manifest name not yet used. It is called just before
// the manifest is written, while the per-VM lock is held.
func uniqueName(ctx context.Context, r *repo.Repo, vm string, now time.Time, k manifest.Kind) (string, error) {
	base := manifest.NewName(now, k)
	name := base
	for i := 2; ; i++ {
		ok, err := r.Backend.Exists(ctx, repo.ManifestKey(vm, name))
		if err != nil {
			return "", err
		}
		if !ok {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

func (o BackupOptions) scanOptions() scan.Options {
	return scan.Options{
		BlockSize:  o.Repo.Config.BlockSize,
		ExtentSize: o.Repo.Config.ExtentSize,
		Workers:    o.Workers,
		Hasher:     o.Repo.Hasher(),
	}
}

// backupFull stores 4 MiB blocks. A block whose content is already in the
// repository (from an earlier full) is reused, so fulls after the first
// only upload blocks that changed.
func backupFull(ctx context.Context, o BackupOptions, prev *State, started time.Time) (*BackupStats, error) {
	r := o.Repo
	have, err := r.BlockSet(ctx)
	if err != nil {
		return nil, err
	}
	put, policyFields := o.placement(started)
	var cache map[string]time.Time
	if prev != nil {
		cache = prev.Locks
	}
	lk, err := newLocks(r, o.Profile, started, policyFields.RetainUntil, cache)
	if err != nil {
		return nil, err
	}
	put.LockUntil = lk.newObject()
	stats := &BackupStats{DiskBytes: o.Source.Size()}
	size := o.Source.Size()
	blocks := make([]extent.Ref, 0, scan.BlockCount(size, r.Config.BlockSize))
	extents := make([]extent.Ref, 0, scan.ExtentCount(size, r.Config.ExtentSize))

	// Lock checks on reused blocks run in the background, 16 at a time,
	// while the scan carries on. Go note: the mutex guards stats, which
	// both the scan callback and the background checks update.
	var mu sync.Mutex
	checks := newGroup(ctx, 16)
	err = scan.Run(ctx, o.Source, o.scanOptions(), func(b scan.Block) error {
		blocks = append(blocks, b.Ref)
		extents = append(extents, b.Extents...)
		switch {
		case b.Ref.Zero:
			stats.ZeroBlocks++
		case have[b.Ref.Hash]:
			stats.BlocksReused++
			if !lk.enabled {
				return nil
			}
			// The scan reuses b.Data for the next block, so a check that
			// may need to upload again keeps its own copy.
			h, data := b.Ref.Hash, bytes.Clone(b.Data)
			return checks.Go(func(ctx context.Context) error {
				d, err := lk.ensure(ctx, repo.BlockKey(h), true, true)
				if err != nil || d != refresh {
					return err
				}
				n, err := r.PutBlock(ctx, h, data, put)
				if err != nil {
					return err
				}
				lk.wrote(repo.BlockKey(h))
				mu.Lock()
				stats.BytesWritten += int64(n)
				mu.Unlock()
				return nil
			})
		default:
			n, err := r.PutBlock(ctx, b.Ref.Hash, b.Data, put)
			if err != nil {
				return err
			}
			lk.wrote(repo.BlockKey(b.Ref.Hash))
			have[b.Ref.Hash] = true
			mu.Lock()
			stats.BlocksStored++
			stats.BytesWritten += int64(n)
			mu.Unlock()
		}
		return nil
	})
	if werr := checks.Wait(); err == nil {
		err = werr
	}
	if err != nil {
		return nil, err
	}
	// The repository's config is needed by every restore.
	if err := lk.ensureAll(ctx, []string{repo.ConfigKey}, true); err != nil {
		return nil, err
	}
	stats.LocksExtended, stats.BlocksRefresh = lk.Extended, lk.Refreshed

	name, err := uniqueName(ctx, r, o.VM, started, manifest.Full)
	if err != nil {
		return nil, err
	}
	stats.Manifest = name
	m := &manifest.Manifest{
		Format: manifest.Format, VM: o.VM, Name: name, Kind: manifest.Full,
		Created: started, Chain: repo.NewID(), Base: name,
		DiskSize: size, BlockSize: r.Config.BlockSize, ExtentSize: r.Config.ExtentSize,
		Blocks:  blocks,
		Profile: policyFields.Profile, DataClass: policyFields.DataClass, RetainUntil: policyFields.RetainUntil,
	}
	if prev != nil {
		m.Parent = prev.LastManifest
	}
	if err := r.PutManifest(ctx, m, store.PutOptions{LockUntil: lk.manifestLock()}); err != nil {
		return nil, err
	}
	st := &State{
		RepoID: r.Config.RepoID, VM: o.VM, DiskSize: size,
		BlockSize: r.Config.BlockSize, ExtentSize: r.Config.ExtentSize,
		Chain: m.Chain, BaseManifest: name, LastManifest: name,
		Blocks: blocks, Base: extents, Current: extents,
		Locks: lk.cache,
	}
	return stats, st.Save(o.StatePath)
}

// backupOverlay runs an incremental or differential. Both record every
// extent that differs from the chain's full, so each manifest is complete.
// An incremental stores only extents not already in the chain's packs; a
// differential stores them all again in fresh packs, so older incremental
// packs can expire.
func backupOverlay(ctx context.Context, o BackupOptions, st *State, started time.Time) (*BackupStats, error) {
	r := o.Repo
	stats := &BackupStats{DiskBytes: o.Source.Size()}
	// known maps each extent already in the chain's packs to its pack.
	known := map[extent.Hash]string{}
	if o.Kind == manifest.Incremental {
		idx, err := r.ChainIndex(ctx, st.Chain)
		if err != nil {
			return nil, err
		}
		for h, loc := range idx {
			known[h] = loc.Pack
		}
	}
	put, policyFields := o.placement(started)
	lk, err := newLocks(r, o.Profile, started, policyFields.RetainUntil, st.Locks)
	if err != nil {
		return nil, err
	}
	put.LockUntil = lk.newObject()
	reusedPacks := map[string]bool{}
	pw := r.NewPackWriter(st.Chain, put)
	extSize := int64(r.Config.ExtentSize)
	current := make([]extent.Ref, 0, len(st.Base))
	var overlay []manifest.Overlay

	err = scan.Run(ctx, o.Source, o.scanOptions(), func(b scan.Block) error {
		first := int(int64(b.Offset) / extSize)
		for j, ref := range b.Extents {
			i := first + j
			current = append(current, ref)
			if ref.Equal(st.Base[i]) {
				continue // same as the full; the base block covers it
			}
			overlay = append(overlay, manifest.Overlay{Index: i, Ref: ref})
			stats.ExtentsChanged++
			if ref.Zero {
				continue // zeros are recorded, never stored
			}
			if pack, ok := known[ref.Hash]; ok {
				if pack != "" {
					reusedPacks[pack] = true
				}
				stats.ExtentsReused++
				continue
			}
			start := int64(j) * extSize
			end := min(start+extSize, int64(b.Length))
			if err := pw.Add(ctx, ref.Hash, b.Data[start:end]); err != nil {
				return err
			}
			known[ref.Hash] = "" // in a pack written this run, locked as it is written
			stats.ExtentsStored++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := pw.Flush(ctx); err != nil {
		return nil, err
	}
	stats.Packs = pw.Packs
	stats.BytesWritten = pw.Written
	for _, loc := range pw.Index {
		lk.wrote(loc.Pack)
		lk.wrote(repo.TOCForPack(loc.Pack))
	}

	// Everything the manifest points at must outlast it: the full's base
	// blocks, packs reused from earlier runs (with their TOCs) and the
	// repository config. This host's cache usually answers without asking
	// the storage; the next full re-checks everything.
	if lk.enabled {
		need := []string{repo.ConfigKey}
		seen := map[string]bool{}
		for _, ref := range st.Blocks {
			if k := repo.BlockKey(ref.Hash); !ref.Zero && !seen[k] {
				seen[k] = true
				need = append(need, k)
			}
		}
		for p := range reusedPacks {
			need = append(need, p, repo.TOCForPack(p))
		}
		if err := lk.ensureAll(ctx, need, false); err != nil {
			return nil, err
		}
		stats.LocksExtended = lk.Extended
	}

	name, err := uniqueName(ctx, r, o.VM, started, o.Kind)
	if err != nil {
		return nil, err
	}
	stats.Manifest = name
	m := &manifest.Manifest{
		Format: manifest.Format, VM: o.VM, Name: name, Kind: o.Kind,
		Created: started, Chain: st.Chain, Base: st.BaseManifest, Parent: st.LastManifest,
		DiskSize: st.DiskSize, BlockSize: st.BlockSize, ExtentSize: st.ExtentSize,
		Blocks: st.Blocks, Overlay: overlay,
		Profile: policyFields.Profile, DataClass: policyFields.DataClass, RetainUntil: policyFields.RetainUntil,
	}
	if err := r.PutManifest(ctx, m, store.PutOptions{LockUntil: lk.manifestLock()}); err != nil {
		return nil, err
	}
	st.Current = current
	st.LastManifest = name
	if lk.enabled {
		st.Locks = lk.cache
	}
	return stats, st.Save(o.StatePath)
}
