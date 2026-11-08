// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package manifest defines a restore point. Every manifest is complete on
// its own: the base blocks of its chain's full, plus every newer extent laid
// over them. Restoring never replays a chain of deltas.
package manifest

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/extent"
)

// Format is the manifest layout version.
const Format = 1

// Kind is the type of backup run that produced a manifest.
type Kind string

// The three backup types.
const (
	Full         Kind = "full"         // 4 MiB blocks; reuses unchanged blocks from earlier fulls
	Incremental  Kind = "incremental"  // extents changed since the previous run
	Differential Kind = "differential" // every extent changed since the chain's full, in fresh packs
)

// ParseKind accepts "full", "incremental" or "differential".
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case Full, Incremental, Differential:
		return k, nil
	}
	return "", fmt.Errorf("unknown backup type %q (use full, incremental or differential)", s)
}

// Overlay replaces one extent of the base with newer content.
type Overlay struct {
	Index int        `json:"i"` // extent number; offset = Index * ExtentSize
	Ref   extent.Ref `json:"r"`
}

// Manifest is one restore point.
type Manifest struct {
	Format     int           `json:"format"`
	VM         string        `json:"vm"`
	Name       string        `json:"name"`
	Kind       Kind          `json:"kind"`
	Created    time.Time     `json:"created"`
	Chain      string        `json:"chain"`  // the chain this belongs to
	Base       string        `json:"base"`   // name of the chain's full
	Parent     string        `json:"parent"` // the previous run, if any
	DiskSize   int64         `json:"diskSize"`
	BlockSize  extent.Length `json:"blockSize"`
	ExtentSize extent.Length `json:"extentSize"`
	Blocks     []extent.Ref  `json:"blocks"`
	Overlay    []Overlay     `json:"overlay,omitempty"`

	// Storage policy this run was made under. Empty in manifests written
	// before profiles existed, and for repositories without classes.
	Profile     string    `json:"profile,omitempty"`
	DataClass   string    `json:"dataClass,omitempty"`  // class of the blocks or packs this run wrote
	RetainUntil time.Time `json:"retainUntil,omitzero"` // keep restorable until at least this time

	// SHA256 is the whole disk's SHA-256, when the backup was made with
	// --hash. A restore's --check compares against it.
	SHA256 string `json:"sha256,omitempty"`
	// Snapshot is set when this backup was read from one of the disk's own
	// named snapshots (not one vmobj took for the run), so the history can
	// be rebuilt as real snapshots on restore. See docs/design/m9-snapshot-history.md.
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// Snapshot describes a named snapshot a backup was read from. Older
// vmobj versions ignore the field, so adding it keeps Format 1.
type Snapshot struct {
	Name string `json:"name"`
	// Created is when the snapshot was first taken: on the original
	// platform, not when it was backed up or last recreated.
	Created     time.Time `json:"created"`
	Description string    `json:"description,omitempty"`
	Platform    string    `json:"platform,omitempty"` // where it was read: zfs, truenas, qnap, libvirt
}

// SameSnapshot reports whether two records describe the same snapshot:
// the same name, taken at the same second.
func SameSnapshot(a, b *Snapshot) bool {
	return a != nil && b != nil && a.Name == b.Name && a.Created.Unix() == b.Created.Unix()
}

// NewName builds a sortable manifest name such as "20261030T202000Z-full".
func NewName(t time.Time, k Kind) string {
	return t.UTC().Format("20060102T150405Z") + "-" + string(k)
}

// CompareNames orders manifest names oldest first. Names sort by their
// timestamp, then by the -2, -3... suffix a backup gets when another of the
// same VM and kind started in the same second, then by kind. Plain text
// order gets the suffixes wrong: "…-incremental-2" sorts before
// "…-incremental", and "-10" before "-2". Names that don't follow the
// pattern sort after the ones that do, by plain text.
func CompareNames(a, b string) int {
	sa, ka, na, oka := splitName(a)
	sb, kb, nb, okb := splitName(b)
	switch {
	case oka && okb:
		if c := cmp.Compare(sa, sb); c != 0 {
			return c
		}
		if c := cmp.Compare(na, nb); c != 0 {
			return c
		}
		return cmp.Compare(ka, kb)
	case oka:
		return -1
	case okb:
		return 1
	}
	return cmp.Compare(a, b)
}

// splitName takes "20261030T202000Z-incremental-3" apart into its stamp,
// its kind and its sequence number (1 when there is no suffix).
func splitName(n string) (stamp, kind string, seq int, ok bool) {
	const width = len("20060102T150405Z")
	if len(n) < width+2 || n[width] != '-' {
		return "", "", 0, false
	}
	if _, err := time.Parse("20060102T150405Z", n[:width]); err != nil {
		return "", "", 0, false
	}
	stamp, kind, seq = n[:width], n[width+1:], 1
	if i := strings.LastIndexByte(kind, '-'); i > 0 {
		if v, err := strconv.Atoi(kind[i+1:]); err == nil && v > 1 {
			kind, seq = kind[:i], v
		}
	}
	return stamp, kind, seq, true
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

var vmName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// CheckVMName allows letters, digits, dot, dash and underscore, so a name
// is always safe to use inside an object key or a file name.
func CheckVMName(name string) error {
	if !vmName.MatchString(name) {
		return fmt.Errorf("VM name %q: use letters, digits, '.', '-' and '_' (max 128)", name)
	}
	return nil
}

// Validate checks that a manifest is internally consistent before use.
func (m *Manifest) Validate() error {
	if m.Format != Format {
		return fmt.Errorf("manifest format %d is not supported", m.Format)
	}
	if m.BlockSize <= 0 || m.ExtentSize <= 0 || m.BlockSize%m.ExtentSize != 0 || m.DiskSize <= 0 {
		return fmt.Errorf("manifest %s has invalid sizes", m.Name)
	}
	wantBlocks := int((m.DiskSize + int64(m.BlockSize) - 1) / int64(m.BlockSize))
	if len(m.Blocks) != wantBlocks {
		return fmt.Errorf("manifest %s lists %d blocks, want %d", m.Name, len(m.Blocks), wantBlocks)
	}
	if m.SHA256 != "" && !sha256Hex.MatchString(m.SHA256) {
		return fmt.Errorf("manifest %s has a malformed sha256", m.Name)
	}
	if m.Snapshot != nil && (m.Snapshot.Name == "" || m.Snapshot.Created.IsZero()) {
		return fmt.Errorf("manifest %s has a snapshot record without a name or date", m.Name)
	}
	extents := int((m.DiskSize + int64(m.ExtentSize) - 1) / int64(m.ExtentSize))
	for _, o := range m.Overlay {
		if o.Index < 0 || o.Index >= extents {
			return fmt.Errorf("manifest %s has an extent index out of range: %d", m.Name, o.Index)
		}
	}
	return nil
}
