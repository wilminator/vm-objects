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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wilminator/vm-objects/internal/atomicfile"
	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
)

// RestoreOptions describe one restore.
type RestoreOptions struct {
	Repo     *repo.Repo
	VM       string
	Manifest string
	Output   string // a raw image file, or a block device such as a zvol
	Force    bool   // allow replacing an existing regular file
	// Progress, when set, is told what the restore is doing: once with
	// PhaseChecking before the backup is checked, then with PhaseWriting
	// as the disk is written (done and total count bytes of the disk), then,
	// for an incremental or differential, with PhaseChanges as the newer
	// extents are laid over it (bytes of those extents). In each writing
	// phase done reaches total at the end.
	Progress func(phase Phase, done, total int64)
}

// Phase names a stage of a restore, for progress output.
type Phase int

const (
	PhaseChecking Phase = iota // checking the backup is complete and readable
	PhaseWriting               // writing the disk
	PhaseChanges               // laying newer extents over it
)

// RestoreStats summarise a restore.
type RestoreStats struct {
	DiskBytes   int64
	BlocksRead  int
	ExtentsRead int
	ZeroBlocks  int
}

// ErrIncomplete means a restore point is missing pieces; nothing was written.
var ErrIncomplete = errors.New("restore point is incomplete")

// Restore writes a manifest's disk to Output.
//
// Safety rules:
//   - Before touching Output, check every piece the manifest needs exists.
//   - A file target is written under a temporary name and renamed into
//     place only after every piece was written and flushed, so a failed or
//     interrupted restore never leaves a partial image under the real name,
//     and --force never destroys the old file before the new one is ready.
//   - A block device is opened exclusively, so Linux refuses one that is
//     mounted or in use.
//   - Every piece is decrypted and its hash checked before it is written.
func Restore(ctx context.Context, o RestoreOptions) (*RestoreStats, error) {
	r := o.Repo
	report := o.Progress
	if report == nil {
		report = func(Phase, int64, int64) {}
	}
	m, err := r.GetManifest(ctx, o.VM, o.Manifest)
	if err != nil {
		return nil, err
	}
	report(PhaseChecking, 0, 0)
	vs, err := Verify(ctx, r, o.VM, o.Manifest, false)
	if err != nil {
		return nil, err
	}
	if len(vs.Missing) > 0 {
		return nil, fmt.Errorf("%w: %d pieces missing, first: %s", ErrIncomplete, len(vs.Missing), vs.Missing[0])
	}
	// Archived objects can't be read yet; say so before touching Output.
	rd, err := CheckReady(ctx, r, o.VM, o.Manifest)
	if err != nil {
		return nil, err
	}
	if !rd.AllReady() {
		return nil, &NotReadyError{Readiness: rd}
	}

	t, err := openTarget(o.Output, m.DiskSize, o.Force)
	if err != nil {
		return nil, err
	}
	stats := &RestoreStats{DiskBytes: m.DiskSize}
	if err := writeDisk(ctx, r, m, t.f, t.sparse, stats, report); err != nil {
		t.abort()
		return nil, err
	}
	if err := t.commit(); err != nil {
		return nil, err
	}
	return stats, nil
}

func writeDisk(ctx context.Context, r *repo.Repo, m *manifest.Manifest, out *os.File, sparse bool, stats *RestoreStats, report func(Phase, int64, int64)) error {
	zeros := make([]byte, m.BlockSize)

	phase, total, done := PhaseWriting, m.DiskSize, int64(0)
	report(phase, 0, total)

	// 1. Base blocks.
	for i, ref := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := int64(i) * int64(m.BlockSize)
		length := min(int64(m.BlockSize), m.DiskSize-off)
		if ref.Zero {
			stats.ZeroBlocks++
			if !sparse {
				if _, err := out.WriteAt(zeros[:length], off); err != nil {
					return err
				}
			}
			done += length
			report(phase, done, total)
			continue
		}
		data, err := r.GetBlock(ctx, ref.Hash)
		if err != nil {
			return err
		}
		if int64(len(data)) != length {
			return fmt.Errorf("block %d has %d bytes, want %d", i, len(data), length)
		}
		if _, err := out.WriteAt(data, off); err != nil {
			return err
		}
		stats.BlocksRead++
		done += length
		report(phase, done, total)
	}

	// 2. Newer extents laid over the base.
	if len(m.Overlay) == 0 {
		return nil
	}
	idx, err := r.ChainIndex(ctx, m.Chain)
	if err != nil {
		return err
	}
	phase, total, done = PhaseChanges, 0, 0
	for _, ov := range m.Overlay {
		off := int64(ov.Index) * int64(m.ExtentSize)
		total += min(int64(m.ExtentSize), m.DiskSize-off)
	}
	report(phase, 0, total)
	for _, ov := range m.Overlay {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := int64(ov.Index) * int64(m.ExtentSize)
		length := min(int64(m.ExtentSize), m.DiskSize-off)
		if ov.Ref.Zero {
			if _, err := out.WriteAt(zeros[:length], off); err != nil {
				return err
			}
			done += length
			report(phase, done, total)
			continue
		}
		loc, ok := idx[ov.Ref.Hash]
		if !ok {
			return fmt.Errorf("extent %d (%s) is missing from chain %s", ov.Index, ov.Ref.Hash, m.Chain)
		}
		data, err := r.GetExtent(ctx, loc, ov.Ref.Hash)
		if err != nil {
			return err
		}
		if int64(len(data)) != length {
			return fmt.Errorf("extent %d has %d bytes, want %d", ov.Index, len(data), length)
		}
		if _, err := out.WriteAt(data, off); err != nil {
			return err
		}
		stats.ExtentsRead++
		done += length
		report(phase, done, total)
	}
	return nil
}

// target is where a restore writes.
type target struct {
	f      *os.File
	sparse bool   // zeros can be skipped (a fresh file)
	tmp    string // temporary file name; empty for a block device
	final  string
}

// openTarget prepares the restore target. Files are written under a
// temporary name next to the final one, created at full size so zero
// regions stay sparse holes. Block devices are written in full, zeros
// included, because old data there must be overwritten.
func openTarget(path string, size int64, force bool) (*target, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.Mode()&fs.ModeDevice != 0:
		// O_EXCL on a block device: Linux refuses it while mounted or open
		// exclusively elsewhere.
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_EXCL, 0)
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
		return &target{f: f, final: path}, nil
	case err == nil && info.Mode().IsRegular():
		if !force {
			return nil, fmt.Errorf("%s already exists; use --force to replace it", path)
		}
	case err == nil:
		return nil, fmt.Errorf("%s is neither a file nor a block device", path)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".vmobj-restore-*")
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return &target{f: f, sparse: true, tmp: f.Name(), final: path}, nil
}

// commit flushes the target and, for a file, renames it into place.
func (t *target) commit() error {
	if err := t.f.Sync(); err != nil {
		t.abort()
		return err
	}
	if err := t.f.Close(); err != nil {
		if t.tmp != "" {
			os.Remove(t.tmp)
		}
		return err
	}
	if t.tmp == "" {
		return nil
	}
	if err := os.Rename(t.tmp, t.final); err != nil {
		os.Remove(t.tmp)
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(t.final))
}

// abort closes the target and removes a temporary file.
func (t *target) abort() {
	t.f.Close()
	if t.tmp != "" {
		os.Remove(t.tmp)
	}
}

// VerifyStats summarise a verification.
type VerifyStats struct {
	Blocks, Extents int
	Missing         []string
	Deep            bool
	// Archived counts pieces a deep check skipped because they are in an
	// archive class and not restored; reading them would fail.
	Archived int
}

// Verify checks that everything a manifest needs is present. With deep,
// it also downloads and checks every piece, which costs money on archive
// storage classes.
func Verify(ctx context.Context, r *repo.Repo, vm, name string, deep bool) (*VerifyStats, error) {
	m, err := r.GetManifest(ctx, vm, name)
	if err != nil {
		return nil, err
	}
	have, err := r.BlockSet(ctx)
	if err != nil {
		return nil, err
	}
	idx, err := r.ChainIndex(ctx, m.Chain)
	if err != nil {
		return nil, err
	}
	vs := &VerifyStats{Deep: deep}
	checked := map[extent.Hash]bool{}
	for i, ref := range m.Blocks {
		if ref.Zero {
			continue
		}
		vs.Blocks++
		if !have[ref.Hash] {
			vs.Missing = append(vs.Missing, fmt.Sprintf("block %d (%s)", i, ref.Hash))
			continue
		}
		if deep && !checked[ref.Hash] {
			if _, err := r.GetBlock(ctx, ref.Hash); errors.Is(err, store.ErrArchived) {
				vs.Archived++
			} else if err != nil {
				vs.Missing = append(vs.Missing, fmt.Sprintf("block %d: %v", i, err))
			}
			checked[ref.Hash] = true
		}
	}
	for _, ov := range m.Overlay {
		if ov.Ref.Zero {
			continue
		}
		vs.Extents++
		loc, ok := idx[ov.Ref.Hash]
		if !ok {
			vs.Missing = append(vs.Missing, fmt.Sprintf("extent %d (%s)", ov.Index, ov.Ref.Hash))
			continue
		}
		if deep && !checked[ov.Ref.Hash] {
			if _, err := r.GetExtent(ctx, loc, ov.Ref.Hash); err != nil {
				vs.Missing = append(vs.Missing, fmt.Sprintf("extent %d: %v", ov.Index, err))
			}
			checked[ov.Ref.Hash] = true
		}
	}
	return vs, nil
}
