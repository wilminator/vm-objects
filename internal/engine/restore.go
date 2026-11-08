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
	"github.com/wilminator/vm-objects/internal/metrics"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
)

// RestoreOptions describe one restore.
type RestoreOptions struct {
	Repo     *repo.Repo
	VM       string
	Manifest string
	Output   string // a raw image file, or a block device such as a zvol
	Force    bool   // allow replacing an existing regular file (rename mode)
	Mode     Mode   // rename (default), copy or in-place; see ModeRename
	// NoResume starts a rename-mode restore afresh, removing what an
	// earlier, interrupted run of it left behind, instead of continuing it.
	NoResume bool
	Scratch  string // copy mode: folder for the temporary full restore (default: the target's folder)
	Check    bool   // copy and in-place: read the target back afterwards and compare
	// Base (in-place only) names a backup of the same disk that the target
	// is known to hold already, for example because the previous step of a
	// replay just checked it. Blocks that are the same in Base and Manifest
	// are then skipped without reading the target. Only use it when the
	// target really does hold Base: a block that differs from Base but is
	// the same in both backups is left wrong (a full --check catches it).
	Base string
	// Journal (staged mode) is where the changes wait until they are
	// proven. Default for a file target: next to it, as NAME.vmobj-journal.
	// A block device needs one given, on another filesystem.
	Journal string
	// CheckWritten, with Check, reads back only the blocks this restore
	// wrote, instead of the whole target. No SHA-256 is computed then.
	CheckWritten bool
	// ExpectSHA256, with a full Check, fails the restore when the whole
	// disk's SHA-256 (computed during the check) differs from it.
	ExpectSHA256 string
	// Progress, when set, is told what the restore is doing: once with
	// PhaseChecking before the backup is checked, then with PhaseWriting
	// as the disk is written (done and total count bytes of the disk; newer
	// extents are merged into each block as it is written). Copy and in-place
	// modes then report PhaseUpdating as the target is compared (bytes of
	// the disk), and PhaseCheckingTarget for --check. In each phase done
	// reaches total at the end.
	Progress func(phase Phase, done, total int64)
}

// Phase names a stage of a restore, for progress output.
type Phase int

const (
	PhaseChecking       Phase = iota // checking the backup is complete and readable
	PhaseWriting                     // writing the disk
	PhaseChanges                     // vmobj copy: newer extents (a restore merges them as each block is written)
	PhaseUpdating                    // copy and in-place: comparing the target and writing what differs
	PhaseCheckingTarget              // --check: reading the target back
	PhaseApplying                    // staged: writing the journal into the target
)

// RestoreStats summarise a restore.
type RestoreStats struct {
	DiskBytes   int64
	BlocksRead  int // blocks fetched from the repository
	ExtentsRead int // newer extents fetched from the repository
	ZeroBlocks  int

	// Copy and in-place modes.
	Compared int64 // bytes of the target read and compared
	// Resumed is set when a rename-mode restore continued the partial file
	// an interrupted run left; ResumedFrom is that file.
	Resumed       bool
	ResumedFrom   string
	Written       int64 // bytes written to the target
	PiecesWritten int   // 4 KiB pieces written (the disk's last maybe shorter)
	Writes        int   // write calls; neighbouring pieces go out together
	BlocksSame    int   // blocks already right, left alone
	ExtentsSame   int   // newer extents already in place, not fetched
	Resized       bool  // a target file was resized to the disk's size
	ResizedFrom   int64
	TargetExtra   int64 // bytes of a block device past the disk, left as they were
	Checked       bool  // --check read the target back and it matched

	// With Base: blocks the same in both backups, skipped without reading.
	BlocksSkipped int
	SkippedBytes  int64
	// A full check: the SHA-256 of the whole disk as read back (hex), and
	// how many bytes the check read. CheckWritten reads only what was written.
	SHA256       string
	CheckedBytes int64

	// Staged mode.
	JournalBytes int64 // bytes of changes the journal held
	JournalFile  int64 // the journal file's size (sealed, compressed)
	Repaired     int   // blocks the check found wrong in the target and fetched whole
	Applied      int64 // bytes written to the target from the journal
	Finished     bool  // an interrupted apply was finished from an existing journal
	Discarded    bool  // an unfinished journal from an earlier run was thrown away
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

	stats := &RestoreStats{DiskBytes: m.DiskSize}
	switch o.Mode {
	case ModeCopy, ModeInPlace, ModeStaged:
		if err := restoreUpdate(ctx, o, m, stats, report); err != nil {
			return nil, err
		}
		return stats, nil
	case "", ModeRename:
		if o.Check {
			return nil, errors.New("--check works with --mode copy or in-place")
		}
	default:
		return nil, fmt.Errorf("unknown restore mode %q", o.Mode)
	}
	t, err := openTarget(o.Output, m.DiskSize, o.Force, !o.NoResume)
	if err != nil {
		return nil, err
	}
	if t.resumed {
		// Continue what an interrupted run wrote: every block is read
		// back and compared with the backup, and only the ones that
		// differ are fetched. Whatever the partial file holds, the
		// result is the backup.
		stats.Resumed, stats.ResumedFrom = true, t.tmp
		_, err = updateInPlace(ctx, r, m, nil, t.f, t.f, stats, report)
	} else {
		err = writeDisk(ctx, r, m, t.f, t.sparse, stats, report)
	}
	if err != nil {
		return nil, t.keep(err)
	}
	if err := t.commit(); err != nil {
		return nil, err
	}
	return stats, nil
}

func writeDisk(ctx context.Context, r *repo.Repo, m *manifest.Manifest, out io.WriterAt, sparse bool, stats *RestoreStats, report func(Phase, int64, int64)) error {
	bs, es := int64(m.BlockSize), int64(m.ExtentSize)
	zeros := make([]byte, bs)
	overlays := overlaysByBlock(m)
	var idx map[extent.Hash]repo.Location
	if len(m.Overlay) > 0 {
		var err error
		if idx, err = r.ChainIndex(ctx, m.Chain); err != nil {
			return err
		}
	}
	buf := make([]byte, bs)

	// One pass, block by block: the newest data for every part of the block
	// is gathered in memory (the full's block, with the newer extents laid
	// over it) and written once. A full block entirely covered by newer
	// extents isn't fetched at all.
	report(PhaseWriting, 0, m.DiskSize)
	var done int64
	for i, ref := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		ovs := overlays[i]
		block := buf[:length]

		covered := make([]bool, (length+es-1)/es)
		for _, ov := range ovs {
			eo := int64(ov.Index)*es - off
			if eo < 0 || eo >= length {
				return fmt.Errorf("extent %d lies outside block %d", ov.Index, i)
			}
			covered[eo/es] = true
		}
		all := len(ovs) == len(covered)

		switch {
		case ref.Zero && len(ovs) == 0:
			stats.ZeroBlocks++
			if !sparse {
				if _, err := timedWriteAt(r, out, zeros[:length], off); err != nil {
					return err
				}
			}
			done += length
			report(PhaseWriting, done, m.DiskSize)
			continue
		case ref.Zero || all:
			if ref.Zero {
				stats.ZeroBlocks++
			}
			copy(block, zeros[:length])
		default:
			data, err := r.GetBlock(ctx, ref.Hash)
			if err != nil {
				return err
			}
			if int64(len(data)) != length {
				return fmt.Errorf("block %d has %d bytes, want %d", i, len(data), length)
			}
			copy(block, data)
			stats.BlocksRead++
		}
		for _, ov := range ovs {
			eo := int64(ov.Index)*es - off
			el := min(es, length-eo)
			if ov.Ref.Zero {
				copy(block[eo:eo+el], zeros[:el])
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
			if int64(len(data)) != el {
				return fmt.Errorf("extent %d has %d bytes, want %d", ov.Index, len(data), el)
			}
			copy(block[eo:eo+el], data)
			stats.ExtentsRead++
		}
		// On a sparse file, a zero block with a few newer extents gets only
		// its non-zero pieces written, so the rest stays unallocated.
		if ref.Zero && sparse {
			if err := writeNonZero(r, out, block, off, es); err != nil {
				return err
			}
		} else if _, err := timedWriteAt(r, out, block, off); err != nil {
			return err
		}
		done += length
		report(PhaseWriting, done, m.DiskSize)
	}
	return nil
}

// writeNonZero writes the extent-sized pieces of block that aren't all zero.
func writeNonZero(r *repo.Repo, out io.WriterAt, block []byte, off, es int64) error {
	n := int64(len(block))
	for e := int64(0); e < n; e += es {
		end := min(e+es, n)
		if extent.IsAllZero(block[e:end]) {
			continue
		}
		if _, err := timedWriteAt(r, out, block[e:end], off+e); err != nil {
			return err
		}
	}
	return nil
}

// target is where a restore writes.
type target struct {
	f       *os.File
	sparse  bool   // zeros can be skipped (a fresh file)
	tmp     string // the partial file; empty for a block device
	final   string
	resumed bool // tmp was left by an interrupted run and is being continued
}

// PartialName is where a rename-mode restore writes until it is complete:
// a hidden file next to the output. An interrupted restore leaves it, and
// running the same restore again continues it.
func PartialName(output string) string {
	return filepath.Join(filepath.Dir(output), "."+filepath.Base(output)+".vmobj-partial")
}

// openTarget prepares the restore target. Files are written under a
// temporary name next to the final one, created at full size so zero
// regions stay sparse holes. Block devices are written in full, zeros
// included, because old data there must be overwritten.
func openTarget(path string, size int64, force, resume bool) (*target, error) {
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
	partial := PartialName(path)
	if pi, err := os.Stat(partial); err == nil {
		if !pi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is in the way and isn't a file", partial)
		}
		f, err := os.OpenFile(partial, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		if err := lockFile(f); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: another restore is writing it (%w)", partial, err)
		}
		if resume && pi.Size() == size {
			return &target{f: f, tmp: partial, final: path, resumed: true}, nil
		}
		// A different size (another backup) or --no-resume: start over in
		// the same file, from empty.
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, err
		}
		return &target{f: f, sparse: true, tmp: partial, final: path}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: another restore is writing it (%w)", partial, err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(partial)
		return nil, err
	}
	return &target{f: f, sparse: true, tmp: partial, final: path}, nil
}

// ResumeHint is added to the error of a restore that stopped part-way.
const ResumeHint = "run the same restore again to continue it (only what isn't there yet is fetched), or add --no-resume to start over"

// keep closes the target after a failure, leaving a partial file for a
// later run to continue, and says so in the error.
func (t *target) keep(err error) error {
	t.f.Sync()
	t.f.Close()
	if t.tmp == "" {
		return err
	}
	return fmt.Errorf("%w\nThe restore so far is kept in %s; %s", err, t.tmp, ResumeHint)
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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

// timedWriteAt writes to the restore target, timed as "restore write".
func timedWriteAt(r *repo.Repo, out io.WriterAt, p []byte, off int64) (int, error) {
	done := r.Metrics.Start(metrics.OutputWrite)
	n, err := out.WriteAt(p, off)
	done(int64(n))
	return n, err
}
