// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/source"
	vtarget "github.com/wilminator/vm-objects/internal/target"
)

// QCOW2Options describe a restore into a new qcow2 image.
type QCOW2Options struct {
	Repo     *repo.Repo
	VM       string
	Manifest string
	Output   string // the new image; must not exist

	// Backing, when set, makes the image an overlay on this file (qcow2
	// or raw, as BackingFormat says): only what differs from it is
	// written. That is a QNAP Virtualization Station snapshot.
	Backing       string
	BackingFormat string
	// Base, with Backing, is the backup the backing chain holds. Blocks
	// that are the same in Base and Manifest are then left out without
	// reading the backing file. Only use it when the chain was checked to
	// hold Base (the previous step of a replay).
	Base string

	// Check reads the finished image back, through its whole chain with
	// vmobj's qcow2 reader, and proves every block. It also computes the
	// disk's SHA-256, compared with ExpectSHA256, or else with the one
	// the backup recorded (--hash), when there is one.
	Check        bool
	ExpectSHA256 string

	Progress func(phase Phase, done, total int64)
}

// RestoreQCOW2 restores a backup as a qcow2 image: a standalone one
// (sparse: zero regions take no space), or an overlay holding only what
// differs from its backing file. The image is complete and valid only once
// this returns nil; on failure (a failed check included) it is removed.
func RestoreQCOW2(ctx context.Context, o QCOW2Options) (*RestoreStats, error) {
	report := func(Phase, int64, int64) {}
	if o.Progress != nil {
		report = o.Progress
	}
	m, err := o.Repo.GetManifest(ctx, o.VM, o.Manifest)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if o.Base != "" && o.Backing == "" {
		return nil, errors.New("a base backup needs a backing file (the image holding it)")
	}
	stats := &RestoreStats{DiskBytes: m.DiskSize}
	report(PhaseChecking, 0, 1)
	if err := checkComplete(ctx, o.Repo, o.VM, m); err != nil {
		return nil, err
	}
	report(PhaseChecking, 1, 1)
	rd, err := CheckReady(ctx, o.Repo, o.VM, o.Manifest)
	if err != nil {
		return nil, err
	}
	if !rd.AllReady() {
		return nil, &NotReadyError{Readiness: rd}
	}
	q, err := vtarget.CreateQCOW2(o.Output, m.DiskSize, vtarget.QCOW2Options{Backing: o.Backing, BackingFormat: o.BackingFormat})
	if err != nil {
		return nil, err
	}
	if o.Backing == "" {
		err = writeDisk(ctx, o.Repo, m, q, true, stats, report)
	} else {
		err = writeOverlay(ctx, o, m, q, stats, report)
	}
	if err != nil {
		q.Abort()
		return nil, err
	}
	if err := q.Close(); err != nil {
		q.Abort()
		return nil, err
	}
	if !o.Check {
		return stats, nil
	}
	expect := o.ExpectSHA256
	if expect == "" {
		expect = m.SHA256
	}
	err = checkImage(ctx, o.Repo, m, o.Output, stats, report)
	if err == nil && expect != "" && !strings.EqualFold(expect, stats.SHA256) {
		err = fmt.Errorf("check after restore: the image's SHA-256 is %s, expected %s", stats.SHA256, expect)
	}
	if err != nil {
		// A wrong image must not be left looking like a good one (or, as
		// an overlay, be taken for a snapshot).
		os.Remove(o.Output)
		return stats, fmt.Errorf("%w; %s was removed", err, o.Output)
	}
	return stats, nil
}

// checkComplete makes sure every piece the restore needs is in the
// repository before anything is written.
func checkComplete(ctx context.Context, r *repo.Repo, vm string, m *manifest.Manifest) error {
	vs, err := Verify(ctx, r, vm, m.Name, false)
	if err != nil {
		return err
	}
	if len(vs.Missing) > 0 {
		return fmt.Errorf("%w: %d pieces missing, first: %s", ErrIncomplete, len(vs.Missing), vs.Missing[0])
	}
	return nil
}

// writeOverlay writes into q only what differs from its backing chain.
// Each block that may differ is read through q (so from the backing
// file), its wanted contents worked out, and the 4 KiB pieces that differ
// written; the writer turns each into a whole cluster, as QEMU would.
func writeOverlay(ctx context.Context, o QCOW2Options, m *manifest.Manifest, q *vtarget.QCOW2, stats *RestoreStats, report func(Phase, int64, int64)) error {
	r := o.Repo
	var base *manifest.Manifest
	if o.Base != "" {
		b, err := r.GetManifest(ctx, o.VM, o.Base)
		if err != nil {
			return fmt.Errorf("base backup: %w", err)
		}
		if b.DiskSize != m.DiskSize || b.BlockSize != m.BlockSize || b.ExtentSize != m.ExtentSize {
			return fmt.Errorf("base backup %s has another disk size or layout", o.Base)
		}
		base = b
	}
	overlays := overlaysByBlock(m)
	var baseOverlays map[int][]manifest.Overlay
	if base != nil {
		baseOverlays = overlaysByBlock(base)
	}
	var idx map[extent.Hash]repo.Location
	if len(m.Overlay) > 0 {
		var err error
		if idx, err = r.ChainIndex(ctx, m.Chain); err != nil {
			return err
		}
	}
	bs := int64(m.BlockSize)
	have := make([]byte, bs)
	report(PhaseUpdating, 0, m.DiskSize)
	var done int64
	for i := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		done += length
		if base != nil && sameBlock(overlays, baseOverlays, m, base, i) {
			stats.BlocksSkipped++
			report(PhaseUpdating, done, m.DiskSize)
			continue
		}
		if _, err := q.ReadAt(have[:length], off); err != nil {
			return fmt.Errorf("reading the backing file at offset %d: %w", off, err)
		}
		stats.Compared += length
		desired, err := desiredBlock(ctx, r, m, i, overlays[i], have[:length], idx, stats)
		if err != nil {
			return err
		}
		if err := writeDiff(r, q, off, have[:length], desired, writeGrain, stats); err != nil {
			return err
		}
		report(PhaseUpdating, done, m.DiskSize)
	}
	return nil
}

// checkImage reads a finished image back through a fresh reader (not the
// writer that made it) and checks every block against the backup: whole
// blocks by their ID, blocks with newer extents piece by piece. It
// computes the disk's SHA-256 as it goes.
func checkImage(ctx context.Context, r *repo.Repo, m *manifest.Manifest, path string, stats *RestoreStats, report func(Phase, int64, int64)) error {
	img, err := source.Open(path, "qcow2")
	if err != nil {
		return fmt.Errorf("check after restore: %w", err)
	}
	defer img.Close()
	if img.Size() != m.DiskSize {
		return fmt.Errorf("check after restore: the image is %d bytes, the backup %d", img.Size(), m.DiskSize)
	}
	h := r.Hasher()
	overlays := overlaysByBlock(m)
	bs := int64(m.BlockSize)
	buf := make([]byte, bs)
	sum := sha256.New()
	report(PhaseCheckingTarget, 0, m.DiskSize)
	for i, ref := range m.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := int64(i) * bs
		length := min(bs, m.DiskSize-off)
		if _, err := img.ReadAt(buf[:length], off); err != nil {
			return fmt.Errorf("check after restore: reading offset %d: %w", off, err)
		}
		sum.Write(buf[:length])
		if ovs := overlays[i]; len(ovs) > 0 {
			if err := checkByParts(ctx, r, m, ovs, i, buf[:length]); err != nil {
				return err
			}
		} else if !h.Ref(buf[:length]).Equal(ref) {
			return fmt.Errorf("check after restore: block %d (offset %d) doesn't hold what it should", i, off)
		}
		stats.CheckedBytes += length
		report(PhaseCheckingTarget, off+length, m.DiskSize)
	}
	stats.SHA256 = hex.EncodeToString(sum.Sum(nil))
	stats.Checked = true
	return nil
}
