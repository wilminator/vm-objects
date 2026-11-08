// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// Snapshot history for libvirt VMs (milestone 9). libvirt's external
// snapshots, and QNAP Virtualization Station's, are qcow2 overlays: when a
// snapshot is taken, the disk gets a new top file whose backing file is
// the one it used until then. So each snapshot's contents are the layer
// under the overlay it created, read with everything below it. Those
// lower layers never change while an overlay points at them, so they can
// be read even while the VM runs.

// DiskSnapshots lists one disk's external snapshots, oldest first, from
// libvirt's snapshot records, or, for a VM read from a definition file
// (QNAP, shut off), from the record files kept next to it.
func (l *Libvirt) DiskSnapshots(ctx context.Context, vm *VM, d vmdef.Disk) ([]DiskSnapshot, error) {
	if d.Type == "block" && strings.HasPrefix(d.Path, "/dev/zvol/") {
		return nil, ErrZvolSnapshots
	}
	if d.Type != "file" || d.Format != "qcow2" {
		return nil, fmt.Errorf("%s: snapshot history needs a qcow2 disk image (or a zvol), not a %s %s disk", d.Path, d.Format, d.Type)
	}
	records, err := l.snapshotRecords(ctx, vm)
	if err != nil {
		return nil, err
	}
	return ExternalSnapshots(d, records, l.Name())
}

// snapshotRecords reads the VM's snapshot definitions.
func (l *Libvirt) snapshotRecords(ctx context.Context, vm *VM) ([]*vmdef.Snapshot, error) {
	if l.DefinitionFile != "" {
		// .<uuid>.meta/<uuid>.xml has its snapshots in .<uuid>.meta/snapshot/.
		return readSnapshotDir(filepath.Join(filepath.Dir(l.DefinitionFile), "snapshot"))
	}
	dom := vm.UUID
	if dom == "" {
		dom = vm.Name
	}
	out, err := l.Virsh.Run(ctx, "snapshot-list", dom, "--name")
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	var recs []*vmdef.Snapshot
	for _, name := range strings.Fields(out) {
		x, err := l.Virsh.Run(ctx, "snapshot-dumpxml", dom, name)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", name, err)
		}
		s, err := vmdef.ParseSnapshot([]byte(x))
		if err != nil {
			return nil, err
		}
		recs = append(recs, s)
	}
	return recs, nil
}

// readSnapshotDir reads every *.xml snapshot record in dir. A missing
// folder means no snapshots.
func readSnapshotDir(dir string) ([]*vmdef.Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []*vmdef.Snapshot
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		s, err := vmdef.ParseSnapshot(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		recs = append(recs, s)
	}
	return recs, nil
}

// ExternalSnapshots matches snapshot records to a qcow2 disk's backing
// chain. A record whose overlay for this disk is layer k of the chain
// describes the disk as layer k+1 (and below) holds it. Records for other
// disks, or whose overlay isn't in the chain any more (a reverted or
// deleted branch), are left out.
func ExternalSnapshots(d vmdef.Disk, records []*vmdef.Snapshot, platformName string) ([]DiskSnapshot, error) {
	chain, err := source.QCOW2Chain(d.Path)
	if err != nil {
		return nil, err
	}
	var out []DiskSnapshot
	for _, rec := range records {
		overlay, ok := rec.Overlays[d.Target]
		if !ok {
			continue
		}
		k := slices.IndexFunc(chain, func(l source.Layer) bool { return sameFile(l.Path, overlay) })
		if k < 0 {
			continue
		}
		if k+1 >= len(chain) {
			return nil, fmt.Errorf("snapshot %s: its overlay %s has no backing file", rec.Name, overlay)
		}
		layer := chain[k+1]
		out = append(out, DiskSnapshot{
			Info: manifest.Snapshot{Name: rec.Name, Created: rec.Created, Description: rec.Description, Platform: platformName},
			Open: func(context.Context) (source.Source, error) { return source.Open(layer.Path, layer.Format) },
		})
	}
	slices.SortStableFunc(out, func(a, b DiskSnapshot) int { return a.Info.Created.Compare(b.Info.Created) })
	return out, nil
}

// sameFile reports whether two paths name the same file, following
// symbolic links (QNAP's /share/X is a link to /share/CACHEDEV1_DATA/X).
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}
