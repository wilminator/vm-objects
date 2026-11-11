// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/wilminator/vm-objects/internal/qvs"
)

// QNAPManaged is the QNAP module with Virtualization Station's API: it
// reads VMs from libvirt like the plain QNAP module, and takes and removes
// snapshots, and starts and stops VMs, through Virtualization Station, so
// its UI stays in step. With it, backup-vm can back up a running VM whose
// disks are image files.
type QNAPManaged struct {
	*Libvirt
	API *qvs.Client
}

// NewQNAPManaged logs in to Virtualization Station with the credentials
// file; Close logs out.
func NewQNAPManaged(ctx context.Context, l *Libvirt, credentials string) (*QNAPManaged, error) {
	cfg, err := qvs.LoadConfig(credentials)
	if err != nil {
		return nil, err
	}
	c, err := qvs.New(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Login(ctx); err != nil {
		return nil, err
	}
	return &QNAPManaged{Libvirt: l, API: c}, nil
}

// Close ends the Virtualization Station session.
func (q *QNAPManaged) Close(ctx context.Context) { q.API.Logout(ctx) }

func (q *QNAPManaged) vmID(ctx context.Context, vm *VM) (qvs.ID, error) {
	v, err := q.API.FindVM(ctx, vm.UUID, vm.Name)
	if err != nil {
		return 0, err
	}
	return v.ID, nil
}

func (q *QNAPManaged) ListSnapshots(ctx context.Context, vm *VM) ([]SnapshotInfo, error) {
	id, err := q.vmID(ctx, vm)
	if err != nil {
		return nil, err
	}
	snaps, err := q.API.ListSnapshots(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotInfo, len(snaps))
	for i, s := range snaps {
		out[i] = SnapshotInfo{ID: s.ID.String(), Name: s.Name, Description: s.Description, Created: s.Created, Locked: s.Locked}
	}
	return out, nil
}

func (q *QNAPManaged) CreateSnapshot(ctx context.Context, vm *VM, s SnapshotInfo) (SnapshotInfo, error) {
	id, err := q.vmID(ctx, vm)
	if err != nil {
		return s, err
	}
	got, err := q.API.CreateSnapshot(ctx, id, s.Name, s.Description)
	if err != nil {
		return s, err
	}
	return SnapshotInfo{ID: got.ID.String(), Name: got.Name, Description: got.Description, Created: got.Created, Locked: got.Locked}, nil
}

func (q *QNAPManaged) RevertSnapshot(ctx context.Context, vm *VM, snap string) error {
	sid, err := qvs.ParseID(snap)
	if err != nil {
		return err
	}
	id, err := q.vmID(ctx, vm)
	if err != nil {
		return err
	}
	return q.API.RevertSnapshot(ctx, id, sid)
}

func (q *QNAPManaged) DeleteSnapshot(ctx context.Context, vm *VM, snap string) error {
	sid, err := qvs.ParseID(snap)
	if err != nil {
		return err
	}
	id, err := q.vmID(ctx, vm)
	if err != nil {
		return err
	}
	return q.API.DeleteSnapshot(ctx, id, sid)
}

func (q *QNAPManaged) State(ctx context.Context, vm *VM) (string, error) {
	id, err := q.vmID(ctx, vm)
	if err != nil {
		return "", err
	}
	return q.API.State(ctx, id)
}

func (q *QNAPManaged) Start(ctx context.Context, vm *VM) error {
	id, err := q.vmID(ctx, vm)
	if err != nil {
		return err
	}
	return q.API.Act(ctx, id, qvs.ActionStart)
}

// Stop asks the guest to shut down, waits up to timeout for it to stop,
// then forces it off when force is set.
func (q *QNAPManaged) Stop(ctx context.Context, vm *VM, timeout time.Duration, force bool) error {
	id, err := q.vmID(ctx, vm)
	if err != nil {
		return err
	}
	if err := q.API.Act(ctx, id, qvs.ActionShutdown); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := q.API.State(ctx, id); err == nil && st == StateStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(q.API.PollEvery):
		}
	}
	if !force {
		return fmt.Errorf("the VM didn't shut down within %s", timeout)
	}
	return q.API.Act(ctx, id, qvs.ActionForceShutdown)
}

// Create defines a VM through Virtualization Station on existing qcow2
// images under a share. It is never started.
func (q *QNAPManaged) Create(ctx context.Context, spec VMSpec) (*VM, error) {
	cs := qvs.CreateSpec{Name: spec.Name, OSType: spec.OSType, UEFI: spec.UEFI, Machine: "", // the original's machine type may be newer than VS's QEMU; VS picks its own
		Cores: spec.VCPUs, Memory: spec.Memory, Bridges: spec.Networks}
	for _, d := range spec.Disks {
		if d.Format != "" && d.Format != "qcow2" {
			return nil, fmt.Errorf("disk %s is %s; Virtualization Station takes qcow2 images", d.Target, d.Format)
		}
		cs.Disks = append(cs.Disks, qvs.CreateDisk{Path: d.Path, Bus: d.Bus})
	}
	v, err := q.API.CreateVM(ctx, cs)
	if err != nil {
		return nil, err
	}
	return &VM{Name: v.Name, UUID: v.UUID}, nil
}
