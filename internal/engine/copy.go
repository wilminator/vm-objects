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
	"time"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
)

// Copying backups between repositories
//
// A copy moves only what the destination lacks, so copying the same
// backup twice does nothing the second time, and copying a chain's
// incrementals one after another moves each changed extent once.
//
// When both repositories use the same key, blocks travel as their stored
// encrypted bytes (each checked on the way). When the keys differ, content
// IDs differ too (they are keyed), so every block and extent is decrypted,
// re-hashed with the destination key and sealed again, and the manifest's
// references are translated. Both repositories must use the same block and
// extent sizes.
//
// The destination's own policy applies: retention and storage class come
// from the profile given for the copy, measured from when the backup was
// originally made, and a destination with Object Lock locks everything
// the copied backup needs. A backup whose retention would already have
// run out at the destination is skipped.

// CopyOptions describe a copy.
type CopyOptions struct {
	From, To *repo.Repo
	// Profile sets retention and class at the destination. Nil copies
	// without a policy (allowed only when the destination has no Object Lock).
	Profile *policy.Profile
	Class   policy.Class
	Now     func() time.Time
	// Progress, when set, is told how far each backup's copy has got:
	// PhaseWriting over its base blocks (bytes of disk covered), then
	// PhaseChanges over its newer extents, as in RestoreOptions. name is
	// "vm/manifest".
	Progress func(name string, phase Phase, done, total int64)
}

// CopyStats summarise a copy.
type CopyStats struct {
	Manifests      int // copied
	Records        int // VM records copied
	BlocksCopied   int
	BlocksPresent  int
	ExtentsCopied  int
	ExtentsPresent int
	BytesWritten   int64
	LocksExtended  int
	Skipped        []string // with the reason
}

// ErrIncompatible means two repositories can't share backups.
var ErrIncompatible = errors.New("repositories use different block or extent sizes")

func (o CopyOptions) check() error {
	if o.From.Config.BlockSize != o.To.Config.BlockSize || o.From.Config.ExtentSize != o.To.Config.ExtentSize {
		return fmt.Errorf("%w (%d/%d here, %d/%d there)", ErrIncompatible,
			o.From.Config.BlockSize, o.From.Config.ExtentSize, o.To.Config.BlockSize, o.To.Config.ExtentSize)
	}
	if o.From.Config.RepoID == o.To.Config.RepoID {
		return errors.New("source and destination are the same repository")
	}
	if o.To.Config.ObjectLock && o.Profile == nil {
		return ErrPolicyNeeded
	}
	return nil
}

func (o CopyOptions) now() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}
	return o.Now().UTC()
}

// destPolicy works out retention and placement at the destination. ok is
// false when the backup would already be expired there.
func (o CopyOptions) destPolicy(created time.Time, k manifest.Kind) (retain time.Time, class policy.Class, ok bool) {
	class = o.Class
	if o.Profile == nil {
		return time.Time{}, class, true
	}
	rule := o.Profile.Rule(k)
	if class == "" {
		class = rule.Class
	}
	retain = created.Add(rule.Retain)
	return retain, class, retain.After(o.now())
}

// CopyManifest copies one backup of one disk series.
func CopyManifest(ctx context.Context, o CopyOptions, vm, name string, st *CopyStats) error {
	if err := o.check(); err != nil {
		return err
	}
	if ok, err := o.To.Backend.Exists(ctx, repo.ManifestKey(vm, name)); err != nil {
		return err
	} else if ok {
		st.Skipped = append(st.Skipped, vm+"/"+name+": already there")
		return nil
	}
	m, err := o.From.GetManifest(ctx, vm, name)
	if err != nil {
		return err
	}
	retain, class, ok := o.destPolicy(m.Created, m.Kind)
	if !ok {
		st.Skipped = append(st.Skipped, fmt.Sprintf("%s/%s: its retention would have ended %s", vm, name, retain.Format("2006-01-02")))
		return nil
	}
	// Archived source data must be restored before it can be copied.
	rd, err := CheckReady(ctx, o.From, vm, name)
	if err != nil {
		return err
	}
	if !rd.AllReady() {
		return &NotReadyError{Readiness: rd}
	}

	lk, err := newLocks(o.To, o.Profile, o.now(), retain, nil)
	if err != nil {
		return err
	}
	put := store.PutOptions{Class: string(class), LockUntil: lk.newObject()}
	if _, ok := o.To.Backend.(store.OptionPutter); !ok {
		class, put.Class = "", ""
	}
	sameKey := o.From.Config.KeyID == o.To.Config.KeyID
	// Blocks and extents are translated separately: an extent can have
	// the same content (so the same hash) as a short final block, and
	// still needs its own copy in a pack.
	translate := map[extent.Hash]extent.Hash{}    // blocks
	translateExt := map[extent.Hash]extent.Hash{} // extents
	var reused []string

	report := func(Phase, int64, int64) {}
	if o.Progress != nil {
		label := vm + "/" + name
		report = func(p Phase, done, total int64) { o.Progress(label, p, done, total) }
	}

	// Base blocks.
	have, err := o.To.BlockSet(ctx)
	if err != nil {
		return err
	}
	var done int64
	report(PhaseWriting, 0, m.DiskSize)
	for i, ref := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := int64(i) * int64(m.BlockSize)
		done = min(off+int64(m.BlockSize), m.DiskSize)
		if ref.Zero {
			report(PhaseWriting, done, m.DiskSize)
			continue
		}
		report(PhaseWriting, off, m.DiskSize)
		if _, done := translate[ref.Hash]; done {
			continue
		}
		if sameKey {
			translate[ref.Hash] = ref.Hash
			if have[ref.Hash] {
				st.BlocksPresent++
				reused = append(reused, repo.BlockKey(ref.Hash))
				continue
			}
			sealed, err := o.From.GetSealedBlock(ctx, ref.Hash)
			if err != nil {
				return err
			}
			if err := o.To.PutSealedBlock(ctx, ref.Hash, sealed, put); err != nil {
				return err
			}
			st.BytesWritten += int64(len(sealed))
		} else {
			data, err := o.From.GetBlock(ctx, ref.Hash)
			if err != nil {
				return err
			}
			h2 := o.To.Hasher().Sum(data)
			translate[ref.Hash] = h2
			if have[h2] {
				st.BlocksPresent++
				reused = append(reused, repo.BlockKey(h2))
				continue
			}
			n, err := o.To.PutBlock(ctx, h2, data, put)
			if err != nil {
				return err
			}
			st.BytesWritten += int64(n)
		}
		have[translate[ref.Hash]] = true
		st.BlocksCopied++
	}
	report(PhaseWriting, m.DiskSize, m.DiskSize)

	// Overlay extents, into packs of the same chain at the destination.
	overlay := make([]manifest.Overlay, len(m.Overlay))
	if len(m.Overlay) > 0 {
		srcIdx, err := o.From.ChainIndex(ctx, m.Chain)
		if err != nil {
			return err
		}
		dstIdx, err := o.To.ChainIndex(ctx, m.Chain)
		if err != nil {
			return err
		}
		pw := o.To.NewPackWriter(m.Chain, put)
		reusedPacks := map[string]bool{}
		changes := int64(len(m.Overlay)) * int64(m.ExtentSize)
		report(PhaseChanges, 0, changes)
		for i, ov := range m.Overlay {
			if err := ctx.Err(); err != nil {
				return err
			}
			report(PhaseChanges, int64(i)*int64(m.ExtentSize), changes)
			overlay[i] = ov
			if ov.Ref.Zero {
				continue
			}
			loc, ok := srcIdx[ov.Ref.Hash]
			if !ok {
				return fmt.Errorf("%s/%s: extent %s is missing from the source", vm, name, ov.Ref.Hash)
			}
			h2, known := translateExt[ov.Ref.Hash]
			if !known {
				data, err := o.From.GetExtent(ctx, loc, ov.Ref.Hash)
				if err != nil {
					return err
				}
				h2 = o.To.Hasher().Sum(data)
				translateExt[ov.Ref.Hash] = h2
				if dl, ok := dstIdx[h2]; ok {
					reusedPacks[dl.Pack] = true
					st.ExtentsPresent++
				} else if _, ok := pw.Index[h2]; !ok {
					if err := pw.Add(ctx, h2, data); err != nil {
						return err
					}
					st.ExtentsCopied++
				}
			}
			overlay[i].Ref.Hash = h2
		}
		if err := pw.Flush(ctx); err != nil {
			return err
		}
		report(PhaseChanges, changes, changes)
		st.BytesWritten += pw.Written
		for p := range reusedPacks {
			reused = append(reused, p, repo.TOCForPack(p))
		}
	}

	// Everything the copied manifest relies on must outlast it.
	if lk.enabled {
		if err := lk.ensureAll(ctx, append(reused, repo.ConfigKey), true); err != nil {
			return err
		}
		st.LocksExtended += lk.Extended
	}
	out := *m
	out.Blocks = make([]extent.Ref, len(m.Blocks))
	for i, ref := range m.Blocks {
		out.Blocks[i] = ref
		if !ref.Zero {
			out.Blocks[i].Hash = translate[ref.Hash]
		}
	}
	out.Overlay = overlay
	out.RetainUntil, out.DataClass = retain, string(class)
	if o.Profile != nil {
		out.Profile = o.Profile.Name
	}
	if err := o.To.PutManifest(ctx, &out, store.PutOptions{LockUntil: lk.manifestLock()}); err != nil {
		return err
	}
	st.Manifests++
	return nil
}

// CopyRecord copies a whole-VM backup: each disk's manifest, then the
// record itself, last.
func CopyRecord(ctx context.Context, o CopyOptions, vm, name string, st *CopyStats) error {
	if err := o.check(); err != nil {
		return err
	}
	if ok, err := o.To.Backend.Exists(ctx, repo.VMRecordKey(vm, name)); err != nil {
		return err
	} else if ok {
		st.Skipped = append(st.Skipped, "VM record "+vm+"/"+name+": already there")
		return nil
	}
	rec, err := o.From.GetVMRecord(ctx, vm, name)
	if err != nil {
		return err
	}
	retain, _, ok := o.destPolicy(rec.Created, rec.Kind)
	if !ok {
		st.Skipped = append(st.Skipped, fmt.Sprintf("VM record %s/%s: its retention would have ended %s", vm, name, retain.Format("2006-01-02")))
		return nil
	}
	for _, d := range rec.Disks {
		if err := CopyManifest(ctx, o, d.Stream, d.Manifest, st); err != nil {
			return fmt.Errorf("disk %s: %w", d.Target, err)
		}
		if ok, err := o.To.Backend.Exists(ctx, repo.ManifestKey(d.Stream, d.Manifest)); err != nil || !ok {
			return fmt.Errorf("disk %s was not copied, so the VM record can't be", d.Target)
		}
	}
	out := *rec
	out.RetainUntil = retain
	var lockUntil time.Time
	if o.Profile != nil {
		out.Profile = o.Profile.Name
		if o.To.Config.ObjectLock {
			lockUntil = retain.Add(o.Profile.Margin)
		}
	}
	if err := o.To.PutVMRecord(ctx, &out, store.PutOptions{LockUntil: lockUntil}); err != nil {
		return err
	}
	st.Records++
	return nil
}

// CopyAll copies every backup the destination lacks: all of a VM's (or
// every VM's) disk backups, oldest first, then the VM records.
func CopyAll(ctx context.Context, o CopyOptions, onlyVM string) (*CopyStats, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	st := &CopyStats{}
	streams, err := o.From.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range streams {
		if onlyVM != "" && s != onlyVM && !isDiskOf(s, onlyVM) {
			continue
		}
		names, err := o.From.ListManifests(ctx, s)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if err := CopyManifest(ctx, o, s, n, st); err != nil {
				return st, fmt.Errorf("%s/%s: %w", s, n, err)
			}
		}
	}
	vms, err := o.From.ListRecordVMs(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		if onlyVM != "" && v != onlyVM {
			continue
		}
		names, err := o.From.ListVMRecords(ctx, v)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if err := CopyRecord(ctx, o, v, n, st); err != nil {
				return st, fmt.Errorf("VM record %s/%s: %w", v, n, err)
			}
		}
	}
	return st, nil
}

// isDiskOf reports whether a series name is one of a VM's disks.
func isDiskOf(stream, vm string) bool {
	return len(stream) > len(vm)+1 && stream[:len(vm)] == vm && stream[len(vm)] == '.'
}
