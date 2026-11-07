// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package engine runs backups, restores and verification.
package engine

import (
	"context"
	"errors"
	"fmt"
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
	stats := &BackupStats{DiskBytes: o.Source.Size()}
	size := o.Source.Size()
	blocks := make([]extent.Ref, 0, scan.BlockCount(size, r.Config.BlockSize))
	extents := make([]extent.Ref, 0, scan.ExtentCount(size, r.Config.ExtentSize))

	err = scan.Run(ctx, o.Source, o.scanOptions(), func(b scan.Block) error {
		blocks = append(blocks, b.Ref)
		extents = append(extents, b.Extents...)
		switch {
		case b.Ref.Zero:
			stats.ZeroBlocks++
		case have[b.Ref.Hash]:
			stats.BlocksReused++
		default:
			n, err := r.PutBlock(ctx, b.Ref.Hash, b.Data, put)
			if err != nil {
				return err
			}
			have[b.Ref.Hash] = true
			stats.BlocksStored++
			stats.BytesWritten += int64(n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

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
	if err := r.PutManifest(ctx, m); err != nil {
		return nil, err
	}
	st := &State{
		RepoID: r.Config.RepoID, VM: o.VM, DiskSize: size,
		BlockSize: r.Config.BlockSize, ExtentSize: r.Config.ExtentSize,
		Chain: m.Chain, BaseManifest: name, LastManifest: name,
		Blocks: blocks, Base: extents, Current: extents,
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
	known := map[extent.Hash]bool{}
	if o.Kind == manifest.Incremental {
		idx, err := r.ChainIndex(ctx, st.Chain)
		if err != nil {
			return nil, err
		}
		for h := range idx {
			known[h] = true
		}
	}
	put, policyFields := o.placement(started)
	pw := r.NewPackWriter(st.Chain, put)
	extSize := int64(r.Config.ExtentSize)
	current := make([]extent.Ref, 0, len(st.Base))
	var overlay []manifest.Overlay

	err := scan.Run(ctx, o.Source, o.scanOptions(), func(b scan.Block) error {
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
			if known[ref.Hash] {
				stats.ExtentsReused++
				continue
			}
			start := int64(j) * extSize
			end := min(start+extSize, int64(b.Length))
			if err := pw.Add(ctx, ref.Hash, b.Data[start:end]); err != nil {
				return err
			}
			known[ref.Hash] = true
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
	if err := r.PutManifest(ctx, m); err != nil {
		return nil, err
	}
	st.Current = current
	st.LastManifest = name
	return stats, st.Save(o.StatePath)
}
