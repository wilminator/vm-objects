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
	"fmt"
	"regexp"
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
}

// NewName builds a sortable manifest name such as "20261030T202000Z-full".
func NewName(t time.Time, k Kind) string {
	return t.UTC().Format("20060102T150405Z") + "-" + string(k)
}

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
	extents := int((m.DiskSize + int64(m.ExtentSize) - 1) / int64(m.ExtentSize))
	for _, o := range m.Overlay {
		if o.Index < 0 || o.Index >= extents {
			return fmt.Errorf("manifest %s has an extent index out of range: %d", m.Name, o.Index)
		}
	}
	return nil
}
