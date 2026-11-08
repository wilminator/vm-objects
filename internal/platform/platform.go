// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package platform is where vmobj learns about a VM from whatever runs it:
// generic libvirt, QNAP Virtualization Station, TrueNAS, and later others.
// The engine asks a Platform to describe a VM (its disks, firmware files
// and definition) and backs up what it gets, so the repository format
// stays the same whichever platform made a backup.
//
// A platform offers optional abilities as extra interfaces, such as
// Quiescer; the engine uses the ones it finds.
package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// VM is a platform's description of one VM, taken at one moment.
type VM struct {
	Name    string // the platform's name for it
	UUID    string
	Running bool
	// Definition is what restoring needs to recreate the VM on this
	// platform. It is stored as is; only the same platform's module
	// interprets it. DefinitionType names its format.
	Definition     []byte
	DefinitionType string // e.g. "libvirt-xml"
	Disks          []vmdef.Disk
	Files          []manifest.VMFile // NVRAM, TPM state
	Skipped        []string          // devices that can't be backed up, with why
}

// Platform describes VMs.
type Platform interface {
	// Name is the platform's short name, recorded in each VM backup.
	Name() string
	// Describe looks up a VM by the platform's name or UUID for it.
	Describe(ctx context.Context, vm string) (*VM, error)
}

// Quiescer freezes a running guest's filesystems (through a guest agent)
// so a snapshot taken meanwhile is consistent, and thaws them again.
type Quiescer interface {
	Freeze(ctx context.Context, vm string) error
	Thaw(ctx context.Context, vm string) error
}

// DiskSnapshot is one of a disk's named snapshots (milestone 9).
type DiskSnapshot struct {
	Info manifest.Snapshot
	// Open returns the disk as it was at the snapshot; the caller closes it.
	Open func(ctx context.Context) (source.Source, error)
}

// Snapshotter lists a disk's named snapshots, oldest first. A platform
// whose snapshots aren't plain ZFS ones (QNAP's qcow2 overlays) offers it;
// for zvol disks the engine reads ZFS snapshots itself.
type Snapshotter interface {
	DiskSnapshots(ctx context.Context, vm *VM, disk vmdef.Disk) ([]DiskSnapshot, error)
}

// ErrZvolSnapshots, from a Snapshotter, means the disk is a ZFS zvol: the
// engine reads its ZFS snapshots itself.
var ErrZvolSnapshots = errors.New("a zvol: use its ZFS snapshots")

// Names lists the platforms vmobj knows, for --platform.
var Names = []string{"libvirt", "qnap", "truenas"}

// Detect picks the platform this host runs, by what's installed.
func Detect() string {
	switch {
	case exists(QNAPLibvirt.Virsh):
		return "qnap"
	case exists(TrueNASMidclt) && exists(TrueNASLibvirt.Socket):
		return "truenas"
	}
	return "libvirt"
}

// Check validates a --platform value; "auto" and "" mean Detect.
func Check(name string) (string, error) {
	if name == "" || name == "auto" {
		return Detect(), nil
	}
	i := sort.SearchStrings(Names, name)
	if i < len(Names) && Names[i] == name {
		return name, nil
	}
	return "", fmt.Errorf("unknown platform %q: use auto, libvirt, qnap or truenas", name)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
