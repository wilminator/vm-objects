// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/repo"
)

// restoreStaged updates an existing target without touching it until the
// result is proven:
//
//  1. The target is opened read-only. Blocks that differ from the backup
//     (all blocks, or with --base only those that differ between the two
//     backups) are compared, and the 4 KiB pieces to change go to a
//     journal, sealed with the repository's key.
//  2. Every block of the target with the journal laid over it is checked
//     against the backup, and the whole disk's SHA-256 computed. Blocks
//     found wrong (the target wasn't what --base said, say) are fetched
//     whole into the journal and checked again. Nothing has been written
//     to the target yet; if this fails, the target is as it was.
//  3. The journal is committed (synced, with a commit record), then
//     applied to the target, read back, and removed.
//
// If step 3 is interrupted, running the same command again finds the
// committed journal and finishes applying it. An uncommitted journal from
// an earlier run is discarded, since the target was never touched.
func restoreStaged(ctx context.Context, o RestoreOptions, m *manifest.Manifest, stats *RestoreStats, report func(Phase, int64, int64)) error {
	if o.CheckWritten {
		return errors.New("--check-written doesn't apply to staged mode: it always checks the whole result before writing")
	}
	info, err := os.Stat(o.Output)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s doesn't exist; staged mode updates an existing file or device (use --mode rename to create a new file)", o.Output)
	}
	if err != nil {
		return err
	}
	device := info.Mode()&fs.ModeDevice != 0
	if !device && !info.Mode().IsRegular() {
		return fmt.Errorf("%s is neither a file nor a block device", o.Output)
	}
	path := o.Journal
	if path == "" {
		if device {
			return errors.New("staged mode onto a block device needs --journal PATH, a file on another filesystem with room for the changes")
		}
		path = o.Output + ".vmobj-journal"
	}

	// The target, read-only for now.
	in, err := os.Open(o.Output)
	if err != nil {
		return err
	}
	defer in.Close()
	size, err := in.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	switch {
	case device && size < m.DiskSize:
		return fmt.Errorf("%s holds %d bytes, but the disk needs %d", o.Output, size, m.DiskSize)
	case device:
		stats.TargetExtra = size - m.DiskSize
	case size != m.DiskSize:
		return fmt.Errorf("%s is %d bytes, the disk %d; staged mode doesn't resize (use --mode in-place or rename)", o.Output, size, m.DiskSize)
	}

	hdr := journalHeader{VM: o.VM, Manifest: m.Name, Base: o.Base, DiskSize: m.DiskSize, Target: o.Output}
	bs := int64(m.BlockSize)

	// A journal from an earlier run.
	if _, err := os.Stat(path); err == nil {
		j, err := openJournal(o.Repo, path, bs)
		if err != nil {
			return err
		}
		if j.header.VM != hdr.VM || j.header.Manifest != hdr.Manifest || j.header.DiskSize != hdr.DiskSize || j.header.Target != hdr.Target {
			j.close()
			return fmt.Errorf("%s is a journal for restoring %s/%s to %s; finish that restore, or remove the journal", path, j.header.VM, j.header.Manifest, j.header.Target)
		}
		if j.commit != nil {
			in.Close()
			stats.Finished = true
			stats.JournalBytes, stats.JournalFile = j.bytes(), j.end
			stats.SHA256, stats.Checked = j.commit.SHA256, true
			return applyJournal(ctx, o, j, device, stats, report)
		}
		if err := j.remove(); err != nil {
			return err
		}
		stats.Discarded = true
	}

	base, err := loadBase(ctx, o, m)
	if err != nil {
		return err
	}
	if base != nil {
		if err := checkJournalRoom(path, m, base); err != nil {
			return err
		}
	}
	j, err := createJournal(o.Repo, path, hdr, bs)
	if err != nil {
		return fmt.Errorf("creating the journal: %w", err)
	}
	keep := true // the journal stays on failure until it is safe to drop
	defer func() {
		if !keep {
			j.remove()
		} else {
			j.close()
		}
	}()

	// 1. Find the changes, into the journal.
	u, err := updateInPlace(ctx, o.Repo, m, base, in, j, stats, report)
	if err != nil {
		keep = false // never applied: the target is untouched
		return err
	}

	// 2. Prove the result.
	bad, err := checkStaged(ctx, o.Repo, m, in, j, u, stats, report)
	if err != nil {
		keep = false
		return err
	}
	if len(bad) > 0 {
		if err := repairStaged(ctx, o.Repo, m, in, j, &u, bad, stats); err != nil {
			keep = false
			return err
		}
		if bad, err = checkStaged(ctx, o.Repo, m, in, j, u, stats, report); err != nil || len(bad) > 0 {
			keep = false
			if err == nil {
				err = fmt.Errorf("block %d still doesn't match after fetching it whole", bad[0])
			}
			return fmt.Errorf("check before applying: %w (the target was not changed)", err)
		}
	}
	if o.ExpectSHA256 != "" && !strings.EqualFold(o.ExpectSHA256, stats.SHA256) {
		keep = false
		return fmt.Errorf("check before applying: the disk's SHA-256 would be %s, expected %s (the target was not changed)", stats.SHA256, o.ExpectSHA256)
	}
	stats.JournalBytes = j.bytes()

	// 3. Commit and apply.
	if err := j.commitWith(stats.SHA256); err != nil {
		keep = false
		return fmt.Errorf("committing the journal: %w (the target was not changed)", err)
	}
	stats.JournalFile = j.end
	in.Close()
	keep = true // committed: from here, a failure leaves the journal to finish with
	return applyJournal(ctx, o, j, device, stats, report)
}

// checkJournalRoom refuses a staged restore whose changes, at most every
// block that differs between base and m, can't fit where the journal goes.
func checkJournalRoom(path string, m, base *manifest.Manifest) error {
	free, ok := freeSpace(filepath.Dir(path))
	if !ok {
		return nil
	}
	a, b := overlaysByBlock(m), overlaysByBlock(base)
	var need int64
	for i := range m.Blocks {
		if !sameBlock(a, b, m, base, i) {
			need += min(int64(m.BlockSize), m.DiskSize-int64(i)*int64(m.BlockSize))
		}
	}
	if need+(256<<20) > free {
		return fmt.Errorf("%w: up to %d bytes of changes, %d free in %s. Put the journal elsewhere with --journal, or restore whole: --mode rename to a new file, or into a new zvol, then swap", ErrJournalFull, need, free, filepath.Dir(path))
	}
	return nil
}

// checkStaged reads every block of the target, lays the journal over it,
// and checks it against what the backup holds, computing the SHA-256 of
// the whole result. It returns the blocks that don't match.
func checkStaged(ctx context.Context, r *repo.Repo, m *manifest.Manifest, in *os.File, j *journal, u update, stats *RestoreStats, report func(Phase, int64, int64)) ([]int, error) {
	h := r.Hasher()
	bs := int64(m.BlockSize)
	buf := make([]byte, bs)
	sum := sha256.New()
	overlays := overlaysByBlock(m)
	var bad []int
	stats.CheckedBytes = 0
	report(PhaseCheckingTarget, 0, m.DiskSize)
	for i := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		b := buf[:length]
		if err := readTarget(r, in, b, off); err != nil {
			return nil, err
		}
		if err := j.overlay(i, b, off); err != nil {
			return nil, err
		}
		sum.Write(b)
		ok := true
		if u.known[i] {
			ok = h.Ref(b).Equal(u.want[i])
		} else if err := checkByParts(ctx, r, m, overlays[i], i, b); err != nil {
			ok = false
		}
		if !ok {
			bad = append(bad, i)
		}
		stats.CheckedBytes += length
		report(PhaseCheckingTarget, stats.CheckedBytes, m.DiskSize)
	}
	stats.SHA256 = hex.EncodeToString(sum.Sum(nil))
	stats.Checked = len(bad) == 0
	return bad, nil
}

// repairStaged fetches each bad block whole and journals all of it, so it
// no longer depends on what the target holds there.
func repairStaged(ctx context.Context, r *repo.Repo, m *manifest.Manifest, in *os.File, j *journal, u *update, bad []int, stats *RestoreStats) error {
	var idx map[extent.Hash]repo.Location
	if len(m.Overlay) > 0 {
		var err error
		if idx, err = r.ChainIndex(ctx, m.Chain); err != nil {
			return err
		}
	}
	overlays := overlaysByBlock(m)
	bs := int64(m.BlockSize)
	for _, i := range bad {
		off := int64(i) * bs
		// Nothing is taken from the target: every newer extent is fetched too.
		desired, err := desiredBlock(ctx, r, m, i, overlays[i], nil, idx, stats)
		if err != nil {
			return err
		}
		if _, err := j.WriteAt(desired, off); err != nil {
			return err
		}
		u.want[i], u.known[i] = r.Hasher().Ref(desired), true
		stats.Repaired++
	}
	return nil
}

// applyJournal writes a committed journal's runs into the target, syncs,
// reads them back, and removes the journal.
func applyJournal(ctx context.Context, o RestoreOptions, j *journal, device bool, stats *RestoreStats, report func(Phase, int64, int64)) error {
	flags := os.O_RDWR
	if device {
		flags |= syscall.O_EXCL
	}
	out, err := os.OpenFile(o.Output, flags, 0)
	if err != nil {
		return fmt.Errorf("opening %s to apply the journal (is it in use?): %w; the journal %s is kept, run the same command again to finish", o.Output, err, j.path)
	}
	defer out.Close()
	total := j.bytes()
	stats.Writes = 0
	report(PhaseApplying, 0, total)
	var done int64
	for i := range j.runs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w; the journal %s is kept, run the same command again to finish", err, j.path)
		}
		data, err := j.run(i)
		if err != nil {
			return err
		}
		if _, err := timedWriteAt(o.Repo, out, data, j.runs[i].off); err != nil {
			return fmt.Errorf("applying the journal: %w; it is kept at %s, run the same command again to finish", err, j.path)
		}
		stats.Writes++
		done += int64(len(data))
		report(PhaseApplying, done, total)
	}
	if err := out.Sync(); err != nil {
		return err
	}
	stats.Applied = done
	stats.Written = done
	// Read back what was applied. Later runs win where runs overlap, so
	// compare against the final bytes of each block touched.
	dropCache(out)
	for b := range j.byBlk {
		off := int64(b) * j.bs
		length := min(j.bs, j.header.DiskSize-off)
		want := make([]byte, length)
		if err := readTarget(o.Repo, out, want, off); err != nil {
			return err
		}
		have := bytes.Clone(want)
		if err := j.overlay(b, want, off); err != nil {
			return err
		}
		if !bytes.Equal(have, want) {
			return fmt.Errorf("after applying the journal, block %d doesn't hold what it should; the journal %s is kept", b, j.path)
		}
	}
	return j.remove()
}
