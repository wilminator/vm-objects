// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
		src  *os.File // copy mode: the complete restore in scratch space
		tmp  string
		want []extent.Ref
		err  error
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
	out, err := existingTarget(o.Output, m.DiskSize, stats)
	if err != nil {
		return err
	}
	defer out.Close()
	if o.Mode == ModeCopy {
		want, err = copyChanged(ctx, o.Repo, m, src, out, stats, report)
	} else {
		want, err = updateInPlace(ctx, o.Repo, m, out, stats, report)
	}
	if err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if o.Check {
		return checkTarget(ctx, o.Repo, m, out, want, stats, report)
	}
	return nil
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
func updateInPlace(ctx context.Context, r *repo.Repo, m *manifest.Manifest, out *os.File, stats *RestoreStats, report func(Phase, int64, int64)) ([]extent.Ref, error) {
	h := r.Hasher()
	bs, es := int64(m.BlockSize), int64(m.ExtentSize)
	perBlock := int(bs / es)
	overlays := make(map[int][]manifest.Overlay)
	for _, ov := range m.Overlay {
		overlays[ov.Index/perBlock] = append(overlays[ov.Index/perBlock], ov)
	}
	var idx map[extent.Hash]repo.Location
	if len(m.Overlay) > 0 {
		var err error
		if idx, err = r.ChainIndex(ctx, m.Chain); err != nil {
			return nil, err
		}
	}
	zeros := make([]byte, bs)
	cur := make([]byte, bs)
	want := make([]extent.Ref, len(m.Blocks))
	report(PhaseUpdating, 0, m.DiskSize)
	var done int64
	for i, ref := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		have := cur[:length]
		if err := readTarget(r, out, have, off); err != nil {
			return nil, err
		}
		stats.Compared += length
		ovs := overlays[i]

		if len(ovs) == 0 && h.Ref(have).Equal(ref) {
			stats.BlocksSame++
			want[i] = ref
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
				return nil, err
			}
			if int64(len(data)) != length {
				return nil, fmt.Errorf("block %d has %d bytes, want %d", i, len(data), length)
			}
			desired = data
			stats.BlocksRead++
		}
		for _, ov := range ovs {
			eo := int64(ov.Index)*es - off
			el := min(es, length-eo)
			if eo < 0 || el <= 0 {
				return nil, fmt.Errorf("extent %d lies outside block %d", ov.Index, i)
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
				return nil, fmt.Errorf("extent %d (%s) is missing from chain %s", ov.Index, ov.Ref.Hash, m.Chain)
			}
			data, err := r.GetExtent(ctx, loc, ov.Ref.Hash)
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != el {
				return nil, fmt.Errorf("extent %d has %d bytes, want %d", ov.Index, len(data), el)
			}
			copy(desired[eo:eo+el], data)
			stats.ExtentsRead++
		}
		if err := writeDiff(r, out, off, have, desired, writeGrain, stats); err != nil {
			return nil, err
		}
		want[i] = h.Ref(desired)
		done += length
		report(PhaseUpdating, done, m.DiskSize)
	}
	return want, nil
}

// copyChanged makes out match src, the complete restore, writing only the
// 4 KiB pieces that differ. Both are local, so it compares bytes directly.
func copyChanged(ctx context.Context, r *repo.Repo, m *manifest.Manifest, src, out *os.File, stats *RestoreStats, report func(Phase, int64, int64)) ([]extent.Ref, error) {
	h := r.Hasher()
	bs := int64(m.BlockSize)
	from, have := make([]byte, bs), make([]byte, bs)
	want := make([]extent.Ref, len(m.Blocks))
	report(PhaseUpdating, 0, m.DiskSize)
	var done int64
	for i := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		if _, err := src.ReadAt(from[:length], off); err != nil {
			return nil, fmt.Errorf("reading the scratch copy: %w", err)
		}
		if err := readTarget(r, out, have[:length], off); err != nil {
			return nil, err
		}
		stats.Compared += length
		if bytes.Equal(from[:length], have[:length]) {
			stats.BlocksSame++
		} else if err := writeDiff(r, out, off, have[:length], from[:length], writeGrain, stats); err != nil {
			return nil, err
		}
		want[i] = h.Ref(from[:length])
		done += length
		report(PhaseUpdating, done, m.DiskSize)
	}
	return want, nil
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
// disk rather than memory.
func checkTarget(ctx context.Context, r *repo.Repo, m *manifest.Manifest, out *os.File, want []extent.Ref, stats *RestoreStats, report func(Phase, int64, int64)) error {
	dropCache(out)
	h := r.Hasher()
	bs := int64(m.BlockSize)
	buf := make([]byte, bs)
	report(PhaseCheckingTarget, 0, m.DiskSize)
	var done int64
	for i := range want {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		if err := readTarget(r, out, buf[:length], off); err != nil {
			return err
		}
		if !h.Ref(buf[:length]).Equal(want[i]) {
			return fmt.Errorf("check after restore: block %d (offset %d) doesn't hold what was written", i, off)
		}
		done += length
		report(PhaseCheckingTarget, done, m.DiskSize)
	}
	stats.Checked = true
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
