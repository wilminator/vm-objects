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
	"slices"
	"strings"
	"syscall"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/metrics"
	"github.com/wilminator/vm-objects/internal/repo"
)

// Mode is how a restore writes its target.
type Mode string

const (
	// ModeRename writes a new file under a temporary name and renames it
	// over the target when complete (a block device is written whole, in
	// place). The target is never left part old, part new. The default.
	ModeRename Mode = "rename"
	// ModeCopy restores into a temporary file in a scratch folder first,
	// then updates the existing target from it, writing only the extents
	// that differ. The target keeps its inode, owner and permissions, and
	// can be on another filesystem or a block device.
	ModeCopy Mode = "copy"
	// ModeInPlace updates the existing target directly: it reads each
	// block, hashes it, and fetches and writes only what differs. It needs
	// no scratch space and downloads the least.
	ModeInPlace Mode = "in-place"
)

// ParseMode checks a --mode value.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case "", ModeRename:
		return ModeRename, nil
	case ModeCopy, ModeInPlace:
		return m, nil
	}
	return "", fmt.Errorf("unknown restore mode %q: use rename, copy or in-place", s)
}

// existingTarget opens a target that copy and in-place modes update. It
// must already exist. A block device is opened exclusively, so Linux
// refuses one that is mounted or in use, and must hold at least the disk;
// a file is resized to exactly the disk's size.
func existingTarget(path string, size int64, stats *RestoreStats) (*os.File, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s doesn't exist; copy and in-place modes update an existing file or device (use --mode rename to create a new file)", path)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case info.Mode()&fs.ModeDevice != 0:
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_EXCL, 0)
		if err != nil {
			return nil, fmt.Errorf("opening %s exclusively (is it mounted or in use?): %w", path, err)
		}
		devSize, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			f.Close()
			return nil, err
		}
		if devSize < size {
			f.Close()
			return nil, fmt.Errorf("%s holds %d bytes, but the disk needs %d", path, devSize, size)
		}
		stats.TargetExtra = devSize - size
		return f, nil
	case info.Mode().IsRegular():
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		if info.Size() != size {
			if err := f.Truncate(size); err != nil {
				f.Close()
				return nil, fmt.Errorf("resizing %s to %d bytes: %w", path, size, err)
			}
			stats.ResizedFrom = info.Size()
			stats.Resized = true
		}
		return f, nil
	}
	return nil, fmt.Errorf("%s is neither a file nor a block device", path)
}

// restoreUpdate runs a copy or in-place restore, after the backup has been
// checked complete and readable.
func restoreUpdate(ctx context.Context, o RestoreOptions, m *manifest.Manifest, stats *RestoreStats, report func(Phase, int64, int64)) error {
	if m.ExtentSize <= 0 || m.BlockSize%m.ExtentSize != 0 {
		return fmt.Errorf("block size %d is not a multiple of extent size %d", m.BlockSize, m.ExtentSize)
	}
	var (
		src *os.File // copy mode: the complete restore in scratch space
		tmp string
		err error
	)
	if o.Mode == ModeCopy {
		scratch := o.Scratch
		if scratch == "" {
			scratch = filepath.Dir(o.Output)
			if info, err := os.Stat(o.Output); err == nil && info.Mode()&fs.ModeDevice != 0 {
				return errors.New("copy mode onto a block device needs --scratch DIR, a folder with room for the whole disk")
			}
		}
		src, err = os.CreateTemp(scratch, ".vmobj-restore-copy-*")
		if err != nil {
			return err
		}
		tmp = src.Name()
		defer func() {
			src.Close()
			os.Remove(tmp)
		}()
		if err := src.Truncate(m.DiskSize); err != nil {
			return err
		}
		if err := writeDisk(ctx, o.Repo, m, src, true, stats, report); err != nil {
			return err
		}
	}
	var base *manifest.Manifest
	if o.Base != "" {
		if o.Mode != ModeInPlace {
			return errors.New("--base works with --mode in-place")
		}
		if base, err = o.Repo.GetManifest(ctx, o.VM, o.Base); err != nil {
			return fmt.Errorf("--base: %w", err)
		}
		if base.DiskSize != m.DiskSize || base.BlockSize != m.BlockSize || base.ExtentSize != m.ExtentSize {
			return fmt.Errorf("--base %s doesn't fit: its disk, block or extent size differs from %s's", o.Base, o.Manifest)
		}
	}
	if o.CheckWritten && !o.Check {
		return errors.New("--check-written needs --check")
	}
	if o.ExpectSHA256 != "" && (!o.Check || o.CheckWritten) {
		return errors.New("--expect-sha256 needs a full --check")
	}
	out, err := existingTarget(o.Output, m.DiskSize, stats)
	if err != nil {
		return err
	}
	defer out.Close()
	var u update
	if o.Mode == ModeCopy {
		u, err = copyChanged(ctx, o.Repo, m, src, out, stats, report)
	} else {
		u, err = updateInPlace(ctx, o.Repo, m, base, out, stats, report)
	}
	if err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if !o.Check {
		return nil
	}
	if err := checkTarget(ctx, o.Repo, m, out, u, o.CheckWritten, stats, report); err != nil {
		return err
	}
	if o.ExpectSHA256 != "" && !strings.EqualFold(o.ExpectSHA256, stats.SHA256) {
		return fmt.Errorf("check after restore: the disk's SHA-256 is %s, expected %s", stats.SHA256, o.ExpectSHA256)
	}
	return nil
}

// update records what an update did to each block, for checkTarget.
type update struct {
	want    []extent.Ref // what each block should hash to, where known
	known   []bool       // want[i] is set (false: a block skipped thanks to --base that has newer extents over it)
	written []bool       // the update wrote to block i
}

func newUpdate(n int) update {
	return update{want: make([]extent.Ref, n), known: make([]bool, n), written: make([]bool, n)}
}

// sameBlock reports whether block i has the same content in a and b by
// their manifests alone: the same full block and the same newer extents
// over it. (It can say no for blocks that are in fact equal; those are
// then compared the usual way.)
func sameBlock(a, b map[int][]manifest.Overlay, am, bm *manifest.Manifest, i int) bool {
	if !am.Blocks[i].Equal(bm.Blocks[i]) {
		return false
	}
	x, y := a[i], b[i]
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if x[k].Index != y[k].Index || !x[k].Ref.Equal(y[k].Ref) {
			return false
		}
	}
	return true
}

// overlaysByBlock groups a manifest's newer extents by the block they fall in.
func overlaysByBlock(m *manifest.Manifest) map[int][]manifest.Overlay {
	perBlock := int(int64(m.BlockSize) / int64(m.ExtentSize))
	by := make(map[int][]manifest.Overlay)
	for _, ov := range m.Overlay {
		by[ov.Index/perBlock] = append(by[ov.Index/perBlock], ov)
	}
	for _, ovs := range by {
		slices.SortFunc(ovs, func(a, b manifest.Overlay) int { return a.Index - b.Index })
	}
	return by
}

// updateInPlace makes out hold m's disk, touching only what differs. For
// each 4 MiB block it reads what is there and hashes it with the
// repository's key:
//
//   - A block the restore point takes whole from its full backup matches
//     when the hash equals the block's ID. Otherwise the block is fetched.
//   - A block with newer extents laid over it is checked extent by extent:
//     a newer extent already in place is not fetched. The rest of the block
//     can only be judged against the full's block, which is fetched.
//
// Only the 4 KiB pieces whose bytes differ are written, coalesced into
// runs. It returns the ref of each block as written, for checkTarget.
//
// With base (the backup the target is known to hold), blocks the same in
// base and m are skipped without reading the target.
func updateInPlace(ctx context.Context, r *repo.Repo, m, base *manifest.Manifest, out *os.File, stats *RestoreStats, report func(Phase, int64, int64)) (update, error) {
	h := r.Hasher()
	bs, es := int64(m.BlockSize), int64(m.ExtentSize)
	overlays := overlaysByBlock(m)
	var baseOverlays map[int][]manifest.Overlay
	if base != nil {
		baseOverlays = overlaysByBlock(base)
	}
	var idx map[extent.Hash]repo.Location
	u := newUpdate(len(m.Blocks))
	if len(m.Overlay) > 0 {
		var err error
		if idx, err = r.ChainIndex(ctx, m.Chain); err != nil {
			return u, err
		}
	}
	zeros := make([]byte, bs)
	cur := make([]byte, bs)
	want := u.want
	report(PhaseUpdating, 0, m.DiskSize)
	var done int64
	for i, ref := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return u, err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		ovs := overlays[i]
		if base != nil && sameBlock(overlays, baseOverlays, m, base, i) {
			stats.BlocksSkipped++
			stats.SkippedBytes += length
			if len(ovs) == 0 {
				want[i], u.known[i] = ref, true
			}
			done += length
			report(PhaseUpdating, done, m.DiskSize)
			continue
		}
		have := cur[:length]
		if err := readTarget(r, out, have, off); err != nil {
			return u, err
		}
		stats.Compared += length

		if len(ovs) == 0 && h.Ref(have).Equal(ref) {
			stats.BlocksSame++
			want[i], u.known[i] = ref, true
			done += length
			report(PhaseUpdating, done, m.DiskSize)
			continue
		}

		// What the block must hold: the full's block, with newer extents over it.
		var desired []byte
		if ref.Zero {
			desired = make([]byte, length)
		} else {
			data, err := r.GetBlock(ctx, ref.Hash)
			if err != nil {
				return u, err
			}
			if int64(len(data)) != length {
				return u, fmt.Errorf("block %d has %d bytes, want %d", i, len(data), length)
			}
			desired = data
			stats.BlocksRead++
		}
		for _, ov := range ovs {
			eo := int64(ov.Index)*es - off
			el := min(es, length-eo)
			if eo < 0 || el <= 0 {
				return u, fmt.Errorf("extent %d lies outside block %d", ov.Index, i)
			}
			if h.Ref(have[eo : eo+el]).Equal(ov.Ref) {
				copy(desired[eo:eo+el], have[eo:eo+el])
				stats.ExtentsSame++
				continue
			}
			if ov.Ref.Zero {
				copy(desired[eo:eo+el], zeros[:el])
				continue
			}
			loc, ok := idx[ov.Ref.Hash]
			if !ok {
				return u, fmt.Errorf("extent %d (%s) is missing from chain %s", ov.Index, ov.Ref.Hash, m.Chain)
			}
			data, err := r.GetExtent(ctx, loc, ov.Ref.Hash)
			if err != nil {
				return u, err
			}
			if int64(len(data)) != el {
				return u, fmt.Errorf("extent %d has %d bytes, want %d", ov.Index, len(data), el)
			}
			copy(desired[eo:eo+el], data)
			stats.ExtentsRead++
		}
		before := stats.Writes
		if err := writeDiff(r, out, off, have, desired, writeGrain, stats); err != nil {
			return u, err
		}
		want[i], u.known[i] = h.Ref(desired), true
		u.written[i] = stats.Writes > before
		done += length
		report(PhaseUpdating, done, m.DiskSize)
	}
	return u, nil
}

// copyChanged makes out match src, the complete restore, writing only the
// 4 KiB pieces that differ. Both are local, so it compares bytes directly.
func copyChanged(ctx context.Context, r *repo.Repo, m *manifest.Manifest, src, out *os.File, stats *RestoreStats, report func(Phase, int64, int64)) (update, error) {
	h := r.Hasher()
	bs := int64(m.BlockSize)
	from, have := make([]byte, bs), make([]byte, bs)
	u := newUpdate(len(m.Blocks))
	report(PhaseUpdating, 0, m.DiskSize)
	var done int64
	for i := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return u, err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		if _, err := src.ReadAt(from[:length], off); err != nil {
			return u, fmt.Errorf("reading the scratch copy: %w", err)
		}
		if err := readTarget(r, out, have[:length], off); err != nil {
			return u, err
		}
		stats.Compared += length
		if bytes.Equal(from[:length], have[:length]) {
			stats.BlocksSame++
		} else {
			if err := writeDiff(r, out, off, have[:length], from[:length], writeGrain, stats); err != nil {
				return u, err
			}
			u.written[i] = true
		}
		u.want[i], u.known[i] = h.Ref(from[:length]), true
		done += length
		report(PhaseUpdating, done, m.DiskSize)
	}
	return u, nil
}

// writeGrain is the size of the pieces writeDiff compares and writes. It is
// finer than an extent so that copy-on-write storage such as ZFS, whose
// blocks are often 4 to 16 KiB, rewrites (and keeps in snapshots) only what
// really changed.
const writeGrain = 4 << 10

// writeDiff writes the pieces of desired that differ from have, the
// target's current bytes at off, comparing grain bytes at a time.
// Neighbouring changed pieces go out as one write.
func writeDiff(r *repo.Repo, out *os.File, off int64, have, desired []byte, grain int64, stats *RestoreStats) error {
	n := int64(len(desired))
	runStart := int64(-1)
	flush := func(end int64) error {
		if runStart < 0 {
			return nil
		}
		if _, err := timedWriteAt(r, out, desired[runStart:end], off+runStart); err != nil {
			return err
		}
		stats.Written += end - runStart
		stats.Writes++
		runStart = -1
		return nil
	}
	for e := int64(0); e < n; e += grain {
		end := min(e+grain, n)
		if bytes.Equal(have[e:end], desired[e:end]) {
			if err := flush(e); err != nil {
				return err
			}
			continue
		}
		stats.PiecesWritten++
		if runStart < 0 {
			runStart = e
		}
	}
	return flush(n)
}

// checkTarget reads the target back after the update and compares each
// block's hash with what was meant to be written. The page cache is
// dropped first, where the system allows it, so the read comes from the
// disk rather than memory. A full check also computes the SHA-256 of the
// whole disk as it goes; writtenOnly reads back only the blocks written.
func checkTarget(ctx context.Context, r *repo.Repo, m *manifest.Manifest, out *os.File, u update, writtenOnly bool, stats *RestoreStats, report func(Phase, int64, int64)) error {
	dropCache(out)
	h := r.Hasher()
	bs := int64(m.BlockSize)
	buf := make([]byte, bs)
	sum := sha256.New()
	var overlays map[int][]manifest.Overlay
	total := m.DiskSize
	if writtenOnly {
		total = 0
		for i, w := range u.written {
			if w {
				total += min(bs, m.DiskSize-int64(i)*bs)
			}
		}
	}
	report(PhaseCheckingTarget, 0, total)
	var done int64
	for i := range u.want {
		if err := ctx.Err(); err != nil {
			return err
		}
		if writtenOnly && !u.written[i] {
			continue
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		if err := readTarget(r, out, buf[:length], off); err != nil {
			return err
		}
		if !writtenOnly {
			sum.Write(buf[:length])
		}
		if u.known[i] {
			if !h.Ref(buf[:length]).Equal(u.want[i]) {
				return fmt.Errorf("check after restore: block %d (offset %d) doesn't hold what was written", i, off)
			}
		} else {
			if overlays == nil {
				overlays = overlaysByBlock(m)
			}
			if err := checkByParts(ctx, r, m, overlays[i], i, buf[:length]); err != nil {
				return err
			}
		}
		done += length
		stats.CheckedBytes += length
		report(PhaseCheckingTarget, done, total)
	}
	if !writtenOnly {
		stats.SHA256 = hex.EncodeToString(sum.Sum(nil))
	}
	stats.Checked = true
	return nil
}

// checkByParts checks block i, which has newer extents over it and was
// skipped thanks to --base, so no hash of the whole block is known: each
// newer extent must match its ref, and the rest must match the full's
// block, which is fetched.
func checkByParts(ctx context.Context, r *repo.Repo, m *manifest.Manifest, ovs []manifest.Overlay, i int, have []byte) error {
	h := r.Hasher()
	bs, es := int64(m.BlockSize), int64(m.ExtentSize)
	off := int64(i) * bs
	length := int64(len(have))
	covered := make([]bool, (length+es-1)/es)
	for _, ov := range ovs {
		eo := int64(ov.Index)*es - off
		el := min(es, length-eo)
		if eo < 0 || el <= 0 {
			return fmt.Errorf("extent %d lies outside block %d", ov.Index, i)
		}
		if !h.Ref(have[eo : eo+el]).Equal(ov.Ref) {
			return fmt.Errorf("check after restore: extent %d (offset %d) doesn't hold what it should", ov.Index, off+eo)
		}
		covered[eo/es] = true
	}
	var full []byte
	ref := m.Blocks[i]
	for k, c := range covered {
		if c {
			continue
		}
		eo := int64(k) * es
		el := min(es, length-eo)
		if full == nil {
			if ref.Zero {
				full = make([]byte, length)
			} else {
				data, err := r.GetBlock(ctx, ref.Hash)
				if err != nil {
					return err
				}
				full = data
			}
		}
		if int64(len(full)) < eo+el || !bytes.Equal(have[eo:eo+el], full[eo:eo+el]) {
			return fmt.Errorf("check after restore: block %d (offset %d) doesn't hold what it should", i, off+eo)
		}
	}
	return nil
}

// readTarget reads exactly len(p) bytes of the target at off.
func readTarget(r *repo.Repo, f *os.File, p []byte, off int64) error {
	done := r.Metrics.Start(metrics.TargetRead)
	n, err := f.ReadAt(p, off)
	done(int64(n))
	if err != nil && !(errors.Is(err, io.EOF) && n == len(p)) {
		return fmt.Errorf("reading the target at offset %d: %w", off, err)
	}
	return nil
}
