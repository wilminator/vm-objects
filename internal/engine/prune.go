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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
)

// PruneOptions control a prune.
type PruneOptions struct {
	Repo  *repo.Repo
	Now   time.Time
	Apply bool // false: report only
	// KeepLast backups of each VM are kept even when expired, so a VM
	// whose backups stopped never loses its last restore point. At least 1.
	KeepLast int
	// Grace protects objects written recently, which may belong to a
	// backup still running (its manifest isn't written yet). At least 1h.
	Grace time.Duration
}

// DefaultGrace is how recent an object must be to be left alone.
const DefaultGrace = 48 * time.Hour

// PruneReport says what a prune found and did.
type PruneReport struct {
	Applied          bool
	Manifests        int      // manifests examined
	ExpiredManifests []string // "vm/name", deleted when Applied
	KeptExpired      []string // expired but kept by KeepLast
	Unreferenced     int      // data objects no kept backup needs
	UnreferencedSize int64
	OldVersions      int // superseded versions of live objects (Object Lock repositories)
	Deleted          int // object versions removed (Applied only)
	DeletedSize      int64
	Recent           int // skipped: written within the grace period
	// Hidden counts objects a kept backup needs whose current version is a
	// delete marker (someone deleted them outside vmobj). Prune keeps
	// their data; "vmobj verify" will report them missing until the marker
	// is removed.
	Hidden int
	// Locked lists objects that would go but are still locked, with the
	// date each can go; a later prune removes them.
	Locked map[string]time.Time
}

// Prune removes backups past their keep-until date and the data only they
// used. It never deletes a locked object version, never bypasses a lock,
// and never deletes an object written within the grace period.
func Prune(ctx context.Context, o PruneOptions) (*PruneReport, error) {
	r := o.Repo
	if o.KeepLast < 1 {
		return nil, errors.New("prune: KeepLast must be at least 1")
	}
	if o.Grace < time.Hour {
		return nil, errors.New("prune: the grace period must be at least an hour")
	}
	rep := &PruneReport{Applied: o.Apply, Locked: map[string]time.Time{}}

	// 1. Whole-VM records first: a kept record keeps its disks' manifests.
	protected := map[string]bool{} // "stream/manifest"
	var expiredRecords []string
	recordDisks := map[string][]manifest.VMDisk{}
	recordVMs, err := r.ListRecordVMs(ctx)
	if err != nil {
		return nil, err
	}
	for _, vm := range recordVMs {
		names, err := r.ListVMRecords(ctx, vm)
		if err != nil {
			return nil, err
		}
		for i, name := range names {
			rec, err := r.GetVMRecord(ctx, vm, name)
			if err != nil {
				return nil, fmt.Errorf("prune stops rather than guess: %w", err)
			}
			if !rec.RetainUntil.IsZero() && o.Now.After(rec.RetainUntil) && i < len(names)-o.KeepLast {
				rep.ExpiredManifests = append(rep.ExpiredManifests, "vm record "+vm+"/"+name)
				expiredRecords = append(expiredRecords, repo.VMRecordKey(vm, name))
				recordDisks[repo.VMRecordKey(vm, name)] = rec.Disks
				continue
			}
			for _, d := range rec.Disks {
				protected[d.Stream+"/"+d.Manifest] = true
			}
		}
	}
	// Expired records go before anything they name. One that can't go yet
	// still protects its disks' manifests.
	removedRecords, err := removeKeys(ctx, r, o, rep, expiredRecords, false)
	if err != nil {
		return nil, err
	}
	for _, k := range expiredRecords {
		if !removedRecords[k] {
			for _, d := range recordDisks[k] {
				protected[d.Stream+"/"+d.Manifest] = true
			}
		}
	}

	// 2. Decide which manifests stay, and gather what they need.
	vms, err := r.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	liveBlocks := map[string]bool{}
	livePacks := map[string]bool{}
	chainIdx := map[string]map[string]string{} // chain -> extent hash -> pack key
	var expiredKeys []string
	expiredManifests := map[string]*manifest.Manifest{}
	for _, vm := range vms {
		names, err := r.ListManifests(ctx, vm)
		if err != nil {
			return nil, err
		}
		slices.SortStableFunc(names, manifest.CompareNames) // oldest first
		for i, name := range names {
			m, err := r.GetManifest(ctx, vm, name)
			if err != nil {
				return nil, fmt.Errorf("prune stops rather than guess: %w", err)
			}
			rep.Manifests++
			newest := i >= len(names)-o.KeepLast || protected[vm+"/"+name]
			if expired(m, o.Now) {
				if !newest {
					rep.ExpiredManifests = append(rep.ExpiredManifests, vm+"/"+name)
					expiredKeys = append(expiredKeys, repo.ManifestKey(vm, name))
					expiredManifests[repo.ManifestKey(vm, name)] = m
					continue
				}
				rep.KeptExpired = append(rep.KeptExpired, vm+"/"+name)
			}
			if err := markLive(ctx, r, m, liveBlocks, livePacks, chainIdx); err != nil {
				return nil, err
			}
		}
	}
	live := func(key string) bool {
		switch {
		case strings.HasPrefix(key, "blocks/"):
			return liveBlocks[key]
		case strings.HasSuffix(key, ".pack"):
			return livePacks[key]
		case strings.HasSuffix(key, ".toc"):
			return livePacks[strings.TrimSuffix(key, ".toc")+".pack"]
		}
		return true // anything unfamiliar is left alone
	}

	// 3. Expired manifests go before data, so no manifest ever names data
	// that is already gone. One that can't go yet (too recent, still
	// locked, or a failed delete) still needs its data, so it counts as kept.
	removed, err := removeKeys(ctx, r, o, rep, expiredKeys, false)
	if err != nil {
		return nil, err
	}
	for _, k := range expiredKeys {
		if removed[k] {
			continue
		}
		if err := markLive(ctx, r, expiredManifests[k], liveBlocks, livePacks, chainIdx); err != nil {
			return nil, err
		}
	}

	// 4. Data no kept manifest needs.
	if v, ok := r.Backend.(store.Versioned); ok {
		if err := pruneVersions(ctx, v, o, rep, live); err != nil {
			return nil, err
		}
		return rep, nil
	}
	var dead []string
	for _, prefix := range []string{"blocks/", "packs/"} {
		keys, err := r.Backend.List(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			if !live(k) {
				dead = append(dead, k)
			}
		}
	}
	rep.Unreferenced = len(dead)
	tocsFirst(dead, func(i int) string { return dead[i] })
	_, err = removeKeys(ctx, r, o, rep, dead, true)
	return rep, err
}

// tocsFirst orders pack TOCs before everything else, so an interrupted
// prune never leaves a TOC naming a pack that is already gone.
func tocsFirst[T any](items []T, key func(i int) string) {
	sort.SliceStable(items, func(i, j int) bool {
		return strings.HasSuffix(key(i), ".toc") && !strings.HasSuffix(key(j), ".toc")
	})
}

func expired(m *manifest.Manifest, now time.Time) bool {
	return !m.RetainUntil.IsZero() && now.After(m.RetainUntil)
}

// markLive records every object a manifest's restore reads.
func markLive(ctx context.Context, r *repo.Repo, m *manifest.Manifest, blocks, packs map[string]bool, chains map[string]map[string]string) error {
	for _, ref := range m.Blocks {
		if !ref.Zero {
			blocks[repo.BlockKey(ref.Hash)] = true
		}
	}
	if len(m.Overlay) == 0 {
		return nil
	}
	idx, ok := chains[m.Chain]
	if !ok {
		full, err := r.ChainIndex(ctx, m.Chain)
		if err != nil {
			return err
		}
		idx = make(map[string]string, len(full))
		for h, loc := range full {
			idx[h.String()] = loc.Pack
		}
		chains[m.Chain] = idx
	}
	for _, ov := range m.Overlay {
		if ov.Ref.Zero {
			continue
		}
		p, ok := idx[ov.Ref.Hash.String()]
		if !ok {
			return fmt.Errorf("prune stops: %s/%s needs extent %s, which no pack holds; run verify", m.VM, m.Name, ov.Ref.Hash)
		}
		packs[p] = true
	}
	return nil
}

// removeKeys deletes whole keys: every version on versioned storage. data
// says whether the keys are data objects, whose sizes are reported.
//
// It returns the keys that are gone (or, in a dry run, would be): a key
// with any version left behind is not.
func removeKeys(ctx context.Context, r *repo.Repo, o PruneOptions, rep *PruneReport, keys []string, data bool) (map[string]bool, error) {
	v, versioned := r.Backend.(store.Versioned)
	removed := map[string]bool{}
	for _, k := range keys {
		if !versioned {
			info, err := store.Stat(ctx, r.Backend, k)
			if errors.Is(err, store.ErrNotFound) {
				removed[k] = true
				continue
			}
			if err != nil {
				return nil, err
			}
			// An unknown modification time counts as recent: when in
			// doubt, keep.
			if info.Modified.IsZero() || o.Now.Sub(info.Modified) < o.Grace {
				rep.Recent++
				continue
			}
			if data {
				rep.UnreferencedSize += info.Size
			}
			if o.Apply {
				if err := r.Backend.Delete(ctx, k); err != nil {
					return nil, err
				}
				rep.Deleted++
				rep.DeletedSize += info.Size
			}
			removed[k] = true
			continue
		}
		versions, err := v.ListVersions(ctx, k)
		if err != nil {
			return nil, err
		}
		all := true
		for _, ver := range versions {
			if ver.Key != k {
				continue // ListVersions takes a prefix; skip longer keys
			}
			gone, err := removeVersion(ctx, v, o, rep, ver)
			if err != nil {
				return nil, err
			}
			all = all && gone
		}
		if all {
			removed[k] = true
		}
	}
	return removed, nil
}

// pruneVersions walks every version of data objects once. Versions of dead
// keys go; superseded versions of live keys (left by lock refreshes and
// re-uploads) go too. For each live key the newest real version always
// stays, even when a delete marker hides it.
func pruneVersions(ctx context.Context, v store.Versioned, o PruneOptions, rep *PruneReport, live func(string) bool) error {
	for _, prefix := range []string{"blocks/", "packs/", "manifests/", "vms/"} {
		versions, err := v.ListVersions(ctx, prefix)
		if err != nil {
			return err
		}
		// The version to keep for each key: its newest real version.
		keep := map[string]store.Version{}
		for _, ver := range versions {
			if ver.DeleteMarker {
				continue
			}
			if k, ok := keep[ver.Key]; !ok || ver.Modified.After(k.Modified) {
				keep[ver.Key] = ver
			}
		}
		tocsFirst(versions, func(i int) string { return versions[i].Key })
		counted := map[string]bool{}
		for _, ver := range versions {
			isLive := live(ver.Key)
			if prefix == "manifests/" || prefix == "vms/" {
				// Manifests and records were handled already; only their old
				// versions are tidied here.
				isLive = true
			}
			switch {
			case isLive && ver.DeleteMarker:
				// Leave markers on live keys alone; report hidden objects.
				if ver.Latest && prefix != "manifests/" && prefix != "vms/" {
					rep.Hidden++
				}
				continue
			case isLive && ver.VersionID == keep[ver.Key].VersionID:
				continue // the object a kept backup reads
			case isLive:
				rep.OldVersions++
			case !counted[ver.Key]:
				counted[ver.Key] = true
				rep.Unreferenced++
			}
			if !ver.DeleteMarker {
				rep.UnreferencedSize += ver.Size
			}
			if _, err := removeVersion(ctx, v, o, rep, ver); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeVersion deletes one version when it is old enough and unlocked.
// It reports whether the version is gone (or, in a dry run, would be).
func removeVersion(ctx context.Context, v store.Versioned, o PruneOptions, rep *PruneReport, ver store.Version) (bool, error) {
	// An unknown modification time counts as recent: when in doubt, keep.
	if ver.Modified.IsZero() || o.Now.Sub(ver.Modified) < o.Grace {
		rep.Recent++
		return false, nil
	}
	if !ver.DeleteMarker {
		info, err := v.StatVersion(ctx, ver.Key, ver.VersionID)
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if o.Now.Before(info.LockUntil) {
			if t, ok := rep.Locked[ver.Key]; !ok || info.LockUntil.After(t) {
				rep.Locked[ver.Key] = info.LockUntil
			}
			return false, nil
		}
	}
	if !o.Apply {
		return true, nil
	}
	err := v.DeleteVersion(ctx, ver.Key, ver.VersionID)
	if errors.Is(err, store.ErrLocked) {
		// The storage's clock disagrees with ours; leave it for next time.
		rep.Locked[ver.Key] = o.Now
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rep.Deleted++
	rep.DeletedSize += ver.Size
	return true, nil
}
