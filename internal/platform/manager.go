// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"context"
	"strings"
	"time"
)

// Hypervisor management through the platform's own API. TrueNAS and QNAP
// Virtualization Station each keep their own record of every VM and
// regenerate libvirt's definition from it, so a change made with virsh
// behind their back gives a VM the UI doesn't know, or one it overwrites.
// The rule: read from libvirt (as Describe does), change through the
// manager. Only a plain libvirt host is changed with virsh.

// SnapshotInfo is a platform-level snapshot: one the platform's UI lists.
type SnapshotInfo struct {
	ID          string // the platform's id for it
	Name        string
	Description string
	Created     time.Time
	Locked      bool
}

// BackupSnapshotPrefix and BackupSnapshotMarker identify the platform
// snapshots vmobj takes for backing up a running VM. A snapshot is vmobj's
// only if its name starts with the prefix and its description has the
// marker, so one a person names "vmobj-something" is never touched.
const (
	BackupSnapshotPrefix = "vmobj-"
	BackupSnapshotMarker = "[vmobj-backup]"
)

// IsBackupSnapshot reports whether a snapshot is one vmobj took for a backup.
func IsBackupSnapshot(name, description string) bool {
	return strings.HasPrefix(name, BackupSnapshotPrefix) && strings.Contains(description, BackupSnapshotMarker)
}

// SnapshotManager takes and removes snapshots through the platform's own
// API. On QNAP a snapshot is an external qcow2 overlay: taking one turns
// the disk's current top file read-only, which is what lets vmobj back up
// a running VM's image-file disks, and deleting it merges the overlay back.
type SnapshotManager interface {
	ListSnapshots(ctx context.Context, vm *VM) ([]SnapshotInfo, error)
	// CreateSnapshot takes a snapshot and returns it, ID filled in, once
	// the platform reports it done.
	CreateSnapshot(ctx context.Context, vm *VM, s SnapshotInfo) (SnapshotInfo, error)
	RevertSnapshot(ctx context.Context, vm *VM, id string) error
	DeleteSnapshot(ctx context.Context, vm *VM, id string) error
}

// VM states a Manager reports.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StatePaused  = "paused"
)

// VMSpec describes a VM to create on existing disks, for a full restore.
type VMSpec struct {
	Name     string
	OSType   string // the platform's OS type (QNAP: one of /qvs/os_types, e.g. win11)
	Machine  string // q35, pc
	Memory   int64  // bytes
	VCPUs    int
	UEFI     bool
	TPM      bool
	Disks    []DiskSpec
	Networks []string // bridge or network names, one per NIC
	// ReuseUUID keeps the original's UUID (the guest sees it as its SMBIOS
	// UUID, and licensing can be tied to it). Off, the platform makes a new
	// one. Only for a real restore: two VMs with one UUID clash.
	ReuseUUID bool
	// Definition is the original's definition, for settings a platform
	// can carry over beyond the fields above.
	Definition     []byte
	DefinitionType string
}

// DiskSpec is one disk of a VMSpec: an existing image or zvol, used in
// place.
type DiskSpec struct {
	Target string // vda, sda...
	Path   string
	Format string // qcow2, raw
	Bus    string // virtio, sata...
}

// Manager drives a VM's lifecycle through the platform's own API.
type Manager interface {
	State(ctx context.Context, vm *VM) (string, error)
	Start(ctx context.Context, vm *VM) error
	// Stop asks the guest to shut down, waiting up to timeout, then
	// forces it off if force is set.
	Stop(ctx context.Context, vm *VM, timeout time.Duration, force bool) error
	// Create defines a VM on existing disks. It never starts it.
	Create(ctx context.Context, spec VMSpec) (*VM, error)
}
