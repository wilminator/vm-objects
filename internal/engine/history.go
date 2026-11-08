// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/source"
)

// Snapshot history (milestone 9): a disk's named snapshots are backed up
// oldest first, one backup each, with the snapshot's name and original
// date in the manifest; a restore replays them in the same order and has
// the target take a snapshot after each step. The engine knows nothing of
// ZFS or QNAP: a platform lists the snapshots (SnapshotSource) and
// recreates them (HistoryTarget).

// SnapshotSource is one of a disk's named snapshots, as a platform lists it.
type SnapshotSource struct {
	Info manifest.Snapshot
	// Open returns the disk as it was at the snapshot. The engine closes it.
	Open func(ctx context.Context) (source.Source, error)
}

// HistoryOptions describe a history backup. Repo, VM, StatePath, Workers,
// Profile, Class and Now come from the embedded BackupOptions; its Source,
// Kind, SHA256 and Snapshot are set per step.
type HistoryOptions struct {
	BackupOptions
	Snapshots []SnapshotSource // oldest first
	// Hash records each snapshot's whole-disk SHA-256 (one extra read of
	// the disk per snapshot), so the replay can prove every step.
	Hash bool
	// Step, when set, is told about each snapshot as its turn comes:
	// skipped when the repository already has it.
	Step func(i, n int, snap manifest.Snapshot, skipped bool)
	// Wrap, when set, wraps each opened source (for progress output).
	Wrap func(src source.Source) (source.Source, func())
}

// HistoryStep is what happened to one snapshot.
type HistoryStep struct {
	Snapshot manifest.Snapshot
	Manifest string // the backup holding it (existing, when Skipped)
	Skipped  bool   // it was already in the repository
	Stats    *BackupStats
}

// BackupHistory backs up every snapshot in o.Snapshots that the
// repository doesn't hold yet, oldest first: the first as a full when
// this host has no usable full for the VM, the rest as incrementals.
// It can be run again at any time; it only adds what's new.
func BackupHistory(ctx context.Context, o HistoryOptions) ([]HistoryStep, error) {
	have, err := HistoryManifests(ctx, o.Repo, o.VM)
	if err != nil {
		return nil, err
	}
	var steps []HistoryStep
	for i, s := range o.Snapshots {
		if err := ctx.Err(); err != nil {
			return steps, err
		}
		info := s.Info
		if info.Name == "" || info.Created.IsZero() {
			return steps, fmt.Errorf("snapshot %d has no name or date", i+1)
		}
		if m := findSnapshot(have, &info); m != nil {
			steps = append(steps, HistoryStep{Snapshot: info, Manifest: m.Name, Skipped: true})
			if o.Step != nil {
				o.Step(i, len(o.Snapshots), info, true)
			}
			continue
		}
		if o.Step != nil {
			o.Step(i, len(o.Snapshots), info, false)
		}
		st, err := backupSnapshot(ctx, o, s, &info)
		if err != nil {
			return steps, fmt.Errorf("snapshot %s: %w", info.Name, err)
		}
		steps = append(steps, HistoryStep{Snapshot: info, Manifest: st.Manifest, Stats: st})
	}
	return steps, nil
}

func backupSnapshot(ctx context.Context, o HistoryOptions, s SnapshotSource, info *manifest.Snapshot) (*BackupStats, error) {
	src, err := s.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	b := o.BackupOptions
	b.Snapshot = info
	if o.Hash {
		if b.SHA256, err = HashSource(ctx, src, nil); err != nil {
			return nil, fmt.Errorf("hashing: %w", err)
		}
	}
	b.Source = src
	if o.Wrap != nil {
		var done func()
		b.Source, done = o.Wrap(src)
		defer done()
	}
	b.Kind = manifest.Incremental
	st, err := Backup(ctx, b)
	if errors.Is(err, ErrNeedFull) {
		b.Kind = manifest.Full
		st, err = Backup(ctx, b)
	}
	return st, err
}

// HistoryManifests returns a VM's backups that were read from named
// snapshots, in the order the snapshots were taken (oldest first). When
// the same snapshot was backed up more than once (a copy made again, say),
// the oldest backup of it is used.
func HistoryManifests(ctx context.Context, r *repo.Repo, vm string) ([]*manifest.Manifest, error) {
	names, err := r.ListManifests(ctx, vm)
	if err != nil {
		return nil, err
	}
	var out []*manifest.Manifest
	for _, n := range names {
		m, err := r.GetManifest(ctx, vm, n)
		if err != nil {
			return nil, err
		}
		if m.Snapshot == nil || findSnapshot(out, m.Snapshot) != nil {
			continue
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b *manifest.Manifest) int {
		if c := a.Snapshot.Created.Compare(b.Snapshot.Created); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out, nil
}

func findSnapshot(ms []*manifest.Manifest, s *manifest.Snapshot) *manifest.Manifest {
	for _, m := range ms {
		if manifest.SameSnapshot(m.Snapshot, s) {
			return m
		}
	}
	return nil
}

// HistoryTarget is where a replayed history goes: a zvol on TrueNAS, and
// (later) a chain of qcow2 overlays on QNAP.
type HistoryTarget interface {
	// Output is the file or block device the next step restores into. It
	// must already exist and be at least the disk's size.
	Output() string
	// Has reports whether the target already holds this snapshot, from an
	// earlier replay that stopped part-way.
	Has(ctx context.Context, snap manifest.Snapshot) (bool, error)
	// Recreate turns the target's current contents into a snapshot named
	// and dated as snap, as closely as the platform allows.
	Recreate(ctx context.Context, snap manifest.Snapshot) error
}

// ReplayOptions describe a history restore.
type ReplayOptions struct {
	Repo   *repo.Repo
	VM     string
	Target HistoryTarget
	// Staged, when set, is a folder (on another filesystem than the
	// target) for staged restores: each step's changes wait in a journal
	// there until the whole result is checked.
	Staged string
	// WrittenOnly reads back only what each step wrote, and checks the
	// whole disk after the last step alone. Faster; the snapshots in
	// between are then trusted, not proven. Not with Staged.
	WrittenOnly bool
	// Step, when set, is told about each snapshot as its turn comes.
	Step func(i, n int, m *manifest.Manifest, skipped bool)
	// Progress is passed on to each step's restore.
	Progress func(phase Phase, done, total int64)
}

// ReplayStep is what happened to one snapshot of the history.
type ReplayStep struct {
	Snapshot manifest.Snapshot
	Manifest string
	Skipped  bool // the target had it already
	SHA256   string
	Proven   bool // the whole disk was checked against the backup's SHA-256
	Stats    *RestoreStats
}

// ErrNoHistory means the VM has no backups made from named snapshots.
var ErrNoHistory = errors.New("no backups from named snapshots; back them up with backup --history first")

// Replay rebuilds a disk's snapshot history in o.Target: each backup in
// order is restored over the previous one, writing only what changed,
// checked, and then snapshotted. So each new snapshot holds only what
// really changed, like the original's. Snapshots the target already has
// are skipped, so a replay that stopped can simply be run again.
func Replay(ctx context.Context, o ReplayOptions) ([]ReplayStep, error) {
	if o.Staged != "" && o.WrittenOnly {
		return nil, errors.New("a staged replay checks every step in full; WrittenOnly doesn't apply")
	}
	ms, err := HistoryManifests(ctx, o.Repo, o.VM)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, ErrNoHistory
	}
	var steps []ReplayStep
	prev := "" // the backup the target is known to hold; empty after a skip
	last := len(ms) - 1
	for i, m := range ms {
		if err := ctx.Err(); err != nil {
			return steps, err
		}
		snap := *m.Snapshot
		has, err := o.Target.Has(ctx, snap)
		if err != nil {
			return steps, err
		}
		if has {
			steps = append(steps, ReplayStep{Snapshot: snap, Manifest: m.Name, Skipped: true})
			if o.Step != nil {
				o.Step(i, len(ms), m, true)
			}
			// Whatever the target holds now, it isn't proven to be this
			// backup (a later step may have stopped part-way), so the
			// next step compares the whole disk.
			prev = ""
			continue
		}
		if o.Step != nil {
			o.Step(i, len(ms), m, false)
		}
		ro := RestoreOptions{
			Repo: o.Repo, VM: o.VM, Manifest: m.Name, Output: o.Target.Output(),
			Mode: ModeInPlace, Check: true, Base: prev, Progress: o.Progress,
		}
		full := true
		if o.Staged != "" {
			ro.Mode = ModeStaged
			ro.Journal = filepath.Join(o.Staged, o.VM+".vmobj-journal")
		} else if o.WrittenOnly && i < last {
			ro.CheckWritten, full = true, false
		}
		if full && m.SHA256 != "" {
			ro.ExpectSHA256 = m.SHA256
		}
		st, err := Restore(ctx, ro)
		if err != nil {
			return steps, fmt.Errorf("restoring %s (snapshot %s): %w; nothing was snapshotted, so run again to continue", m.Name, snap.Name, err)
		}
		if err := o.Target.Recreate(ctx, snap); err != nil {
			return steps, fmt.Errorf("recreating snapshot %s: %w", snap.Name, err)
		}
		steps = append(steps, ReplayStep{
			Snapshot: snap, Manifest: m.Name, SHA256: st.SHA256,
			Proven: ro.ExpectSHA256 != "", Stats: st,
		})
		prev = m.Name
	}
	return steps, nil
}
