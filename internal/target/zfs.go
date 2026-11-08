// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package target holds the places a replayed snapshot history can go
// (engine.HistoryTarget): a ZFS zvol now, a chain of qcow2 overlays later.
package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/source"
)

// ZFS user properties vmobj sets on a snapshot it recreates. ZFS gives a
// new snapshot today's date and won't change it, and a snapshot name can't
// hold every character another platform allows, so the originals are kept
// here. source.ListZFSSnapshots reads vmobj:created back, so backing the
// zvol up again keeps the original dates.
const (
	PropCreated     = "vmobj:created"     // RFC 3339, UTC
	PropName        = "vmobj:name"        // the original name, when it had to change
	PropDescription = "vmobj:description" // free text, when there is one
)

// ZFS replays a history into a zvol.
type ZFS struct {
	Runner source.ZFSRunner // default: source.ExecZFS
	Zvol   string           // pool/path/name
	DevDir string           // default: /dev/zvol
}

// ZvolSpec says how to create a zvol.
type ZvolSpec struct {
	Size        int64  // bytes
	BlockSize   string // volblocksize, e.g. "16K"; empty: the pool's default
	Sparse      bool   // no reservation
	Compression string // empty: inherited
}

// LikeZvol reads a ZvolSpec from an existing zvol: its size, block size,
// compression and whether it is sparse.
func LikeZvol(ctx context.Context, r source.ZFSRunner, zvol string) (ZvolSpec, error) {
	r = runner(r)
	get := func(prop string, parsable bool) (string, error) {
		args := []string{"get", "-H", "-o", "value"}
		if parsable {
			args = append(args, "-p")
		}
		out, err := r.Run(ctx, append(args, prop, zvol)...)
		return strings.TrimSpace(out), err
	}
	var s ZvolSpec
	size, err := get("volsize", true)
	if err != nil {
		return s, err
	}
	if s.Size, err = strconv.ParseInt(size, 10, 64); err != nil {
		return s, fmt.Errorf("volsize of %s: %q", zvol, size)
	}
	if s.BlockSize, err = get("volblocksize", true); err != nil {
		return s, err
	}
	if s.Compression, err = get("compression", false); err != nil {
		return s, err
	}
	res, err := get("refreservation", false)
	if err != nil {
		return s, err
	}
	s.Sparse = res == "none"
	return s, nil
}

// Create makes the zvol (which must not exist) and waits for its device.
func (z *ZFS) Create(ctx context.Context, spec ZvolSpec) error {
	if spec.Size <= 0 {
		return errors.New("the new zvol needs a size")
	}
	args := []string{"create", "-V", strconv.FormatInt(spec.Size, 10)}
	if spec.Sparse {
		args = append(args, "-s")
	}
	if spec.BlockSize != "" {
		args = append(args, "-o", "volblocksize="+spec.BlockSize)
	}
	if spec.Compression != "" {
		args = append(args, "-o", "compression="+spec.Compression)
	}
	if _, err := runner(z.Runner).Run(ctx, append(args, z.Zvol)...); err != nil {
		return err
	}
	return z.WaitDevice(ctx, 30*time.Second)
}

// Exists reports whether the zvol is there already.
func (z *ZFS) Exists(ctx context.Context) bool {
	_, err := runner(z.Runner).Run(ctx, "list", "-H", "-o", "name", z.Zvol)
	return err == nil
}

// WaitDevice waits for the zvol's device node, which udev makes shortly
// after the zvol appears.
func (z *ZFS) WaitDevice(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(z.Output()); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within %s", z.Output(), limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Output is the zvol's device.
func (z *ZFS) Output() string {
	dir := z.DevDir
	if dir == "" {
		dir = "/dev/zvol"
	}
	return filepath.Join(dir, z.Zvol)
}

// Has reports whether the zvol has this snapshot already. A snapshot of
// the same name with another date is a different one, and an error: two
// names that differ only in characters ZFS doesn't allow end up the same.
func (z *ZFS) Has(ctx context.Context, snap manifest.Snapshot) (bool, error) {
	full := z.Zvol + "@" + source.ZFSSnapshotName(snap.Name)
	r := runner(z.Runner)
	if _, err := r.Run(ctx, "list", "-H", "-o", "name", "-t", "snapshot", full); err != nil {
		return false, nil
	}
	out, err := r.Run(ctx, "get", "-H", "-o", "value", PropCreated, full)
	if err != nil {
		return false, err
	}
	got := strings.TrimSpace(out)
	if got == "-" || got == "" {
		return false, fmt.Errorf("%s already exists and wasn't made by a vmobj replay; rename or remove it, or replay into a new zvol", full)
	}
	t, err := time.Parse(time.RFC3339, got)
	if err != nil || t.Unix() != snap.Created.Unix() {
		return false, fmt.Errorf("%s already exists but is a different snapshot (%s %s, this one %s)", full, PropCreated, got, created(snap))
	}
	return true, nil
}

// Recreate snapshots the zvol, with the original date (and, where they
// had to change or exist, the name and description) as properties. One
// zfs command sets them with the snapshot, so it never exists without them.
func (z *ZFS) Recreate(ctx context.Context, snap manifest.Snapshot) error {
	name := source.ZFSSnapshotName(snap.Name)
	args := []string{"snapshot", "-o", PropCreated + "=" + created(snap)}
	if name != snap.Name {
		args = append(args, "-o", PropName+"="+snap.Name)
	}
	if snap.Description != "" {
		args = append(args, "-o", PropDescription+"="+snap.Description)
	}
	_, err := runner(z.Runner).Run(ctx, append(args, z.Zvol+"@"+name)...)
	return err
}

func created(s manifest.Snapshot) string { return s.Created.UTC().Format(time.RFC3339) }

func runner(r source.ZFSRunner) source.ZFSRunner {
	if r == nil {
		return source.ExecZFS{}
	}
	return r
}
