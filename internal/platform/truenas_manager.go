// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// TrueNASManaged drives TrueNAS VMs through the middleware (midclt), the
// way QNAPManaged drives Virtualization Station: TrueNAS keeps each VM's
// real configuration in its own records and writes libvirt's definition
// from them at start, so nothing is changed with virsh. Reading stays with
// TrueNAS (Describe). This is a closed set of calls: a method outside it
// is refused before midclt runs, ids are numbers taken from TrueNAS's own
// replies, and every argument is built with encoding/json and passed as an
// exec argument, never through a shell.
type TrueNASManaged struct {
	T *TrueNAS
	// PollEvery is how often a stop is rechecked.
	PollEvery time.Duration
}

// JobMidclt is a Midclt that can also wait for a middleware job to end
// (vm.stop is one).
type JobMidclt interface {
	CallJob(ctx context.Context, method string, args ...string) ([]byte, error)
}

// CallJob runs midclt in job mode: it returns when the job has finished.
func (m ExecMidclt) CallJob(ctx context.Context, method string, args ...string) ([]byte, error) {
	bin := m.Path
	if bin == "" {
		bin = TrueNASMidclt
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, append([]string{"call", "-j", method}, args...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("midclt call %s: %w: %s", method, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// The middleware methods vmobj may call to change anything. vm.query is
// the read the TrueNAS module already makes.
const (
	mVMStart        = "vm.start"
	mVMStop         = "vm.stop"
	mVMCreate       = "vm.create"
	mVMDelete       = "vm.delete"
	mVMDeviceCreate = "vm.device.create"
)

// trueNASMethods says which allowed methods are jobs.
var trueNASMethods = map[string]bool{ // true = a job
	mVMStart: false, mVMStop: true, mVMCreate: false, mVMDelete: false, mVMDeviceCreate: false,
}

// NewTrueNASManaged wraps the TrueNAS module.
func NewTrueNASManaged(t *TrueNAS) *TrueNASManaged {
	return &TrueNASManaged{T: t, PollEvery: 2 * time.Second}
}

func (m *TrueNASManaged) call(ctx context.Context, method string, args ...any) ([]byte, error) {
	job, ok := trueNASMethods[method]
	if !ok {
		return nil, fmt.Errorf("truenas: %s is not a call vmobj makes", method)
	}
	strs := make([]string, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		strs[i] = string(b)
	}
	if job {
		if jm, ok := m.T.Midclt.(JobMidclt); ok {
			return jm.CallJob(ctx, method, strs...)
		}
	}
	return m.T.Midclt.Call(ctx, method, strs...)
}

// vmStatus is the part of a vm.query record that says whether it runs.
type vmStatus struct {
	ID     int    `json:"id"`
	UUID   string `json:"uuid"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func (m *TrueNASManaged) find(ctx context.Context, name string) (*vmStatus, error) {
	raw, _, err := m.T.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	var s vmStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("reading vm.query's answer: %w", err)
	}
	return &s, nil
}

// State reports the VM's state as TrueNAS records it.
func (m *TrueNASManaged) State(ctx context.Context, vm *VM) (string, error) {
	s, err := m.find(ctx, vm.Name)
	if err != nil {
		return "", err
	}
	return normalState(s.Status.State)
}

func normalState(s string) (string, error) {
	switch strings.ToUpper(s) {
	case "RUNNING":
		return StateRunning, nil
	case "STOPPED":
		return StateStopped, nil
	case "SUSPENDED", "PAUSED":
		return StatePaused, nil
	}
	return "", fmt.Errorf("truenas: unknown VM state %q", s)
}

// Start starts a stopped VM.
func (m *TrueNASManaged) Start(ctx context.Context, vm *VM) error {
	s, err := m.find(ctx, vm.Name)
	if err != nil {
		return err
	}
	if st, err := normalState(s.Status.State); err == nil && st == StateRunning {
		return nil
	}
	_, err = m.call(ctx, mVMStart, s.ID, map[string]any{"overcommit": false})
	return err
}

// Stop asks the guest to shut down and waits up to timeout; with force it
// then powers the VM off.
func (m *TrueNASManaged) Stop(ctx context.Context, vm *VM, timeout time.Duration, force bool) error {
	s, err := m.find(ctx, vm.Name)
	if err != nil {
		return err
	}
	if st, err := normalState(s.Status.State); err == nil && st == StateStopped {
		return nil
	}
	if _, err := m.call(ctx, mVMStop, s.ID, map[string]any{"force": false, "force_after_timeout": force}); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err := m.State(ctx, vm)
		if err != nil {
			return err
		}
		if st == StateStopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("truenas: %s is still running %s after the stop request", vm.Name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.PollEvery):
		}
	}
}

// trueNASName is what TrueNAS accepts as a VM's name.
var trueNASName = regexp.MustCompile(`^[A-Za-z0-9_]{1,63}$`)

// createKeys are the vm.create settings carried over from the original's
// record. Left out on purpose: id and uuid (the copy is a new VM), devices
// (made one by one), autostart (a restored VM never starts by itself) and
// status.
var createKeys = []string{
	"description", "vcpus", "cores", "threads", "memory", "min_memory",
	"bootloader", "bootloader_ovmf", "cpu_mode", "cpu_model", "cpuset", "nodeset", "pin_vcpus",
	"hide_from_msr", "hyperv_enlightenments", "command_line_args", "machine_type", "arch_type",
	"time", "shutdown_timeout", "trusted_platform_module", "ensure_display_device",
	"enable_cpu_topology_extension",
}

// Create defines a VM on existing disks (zvols). spec.Definition, when it is
// the settings file kept with a TrueNAS backup (DefinitionType "truenas"),
// supplies the original's settings; the fields of spec say what differs.
// It never starts the VM, and it removes what it made if a step fails.
func (m *TrueNASManaged) Create(ctx context.Context, spec VMSpec) (*VM, error) {
	if !trueNASName.MatchString(spec.Name) {
		return nil, fmt.Errorf("truenas: %q can't be a TrueNAS VM name (letters, digits and '_', up to 63); pick another with --new-name", spec.Name)
	}
	if len(spec.Disks) == 0 {
		return nil, errors.New("truenas: a VM needs at least one disk")
	}
	for _, d := range spec.Disks {
		if !strings.HasPrefix(d.Path, "/dev/zvol/") || strings.Contains(d.Path, "..") {
			return nil, fmt.Errorf("truenas: disk %s is %q; a TrueNAS VM's disks are /dev/zvol/POOL/NAME", d.Target, d.Path)
		}
	}
	if _, _, err := m.T.lookup(ctx, spec.Name); err == nil {
		return nil, fmt.Errorf("truenas: a VM named %q already exists; pick another with --new-name", spec.Name)
	}

	var orig struct {
		Devices []struct {
			Attributes map[string]any `json:"attributes"`
		} `json:"devices"`
	}
	base := map[string]any{}
	if spec.DefinitionType == "truenas" && len(spec.Definition) > 0 {
		if err := json.Unmarshal(spec.Definition, &base); err != nil {
			return nil, fmt.Errorf("truenas: reading the backup's VM settings: %w", err)
		}
		if err := json.Unmarshal(spec.Definition, &orig); err != nil {
			return nil, fmt.Errorf("truenas: reading the backup's devices: %w", err)
		}
	}
	payload := map[string]any{}
	for _, k := range createKeys {
		if v, ok := base[k]; ok {
			payload[k] = v
		}
	}
	payload["name"] = spec.Name
	payload["autostart"] = false
	if spec.Memory > 0 {
		payload["memory"] = spec.Memory >> 20 // TrueNAS counts MiB
	}
	if _, ok := payload["vcpus"]; !ok && spec.VCPUs > 0 {
		payload["vcpus"], payload["cores"], payload["threads"] = 1, spec.VCPUs, 1
	}
	if _, ok := payload["bootloader"]; !ok {
		payload["bootloader"] = map[bool]string{true: "UEFI", false: "BIOS"}[spec.UEFI]
	}
	if _, ok := payload["trusted_platform_module"]; !ok {
		payload["trusted_platform_module"] = spec.TPM
	}

	out, err := m.call(ctx, mVMCreate, payload)
	if err != nil {
		return nil, fmt.Errorf("truenas: creating the VM: %w", err)
	}
	var made vmStatus
	if err := json.Unmarshal(out, &made); err != nil || made.ID == 0 {
		return nil, errors.New("truenas: vm.create's answer has no id")
	}
	fail := func(err error) (*VM, error) {
		// Remove only the VM just made, and never its zvols.
		if _, derr := m.call(context.WithoutCancel(ctx), mVMDelete, made.ID, map[string]any{"zvols": false, "force": true}); derr != nil {
			err = fmt.Errorf("%w (and removing the half-made VM %d failed: %v; delete it in the TrueNAS UI)", err, made.ID, derr)
		}
		return nil, err
	}

	var skipped []string
	devices := []map[string]any{}
	disk, nic := 0, 0
	for _, d := range orig.Devices {
		switch dtype, _ := d.Attributes["dtype"].(string); dtype {
		case "DISK":
			if disk >= len(spec.Disks) {
				return fail(fmt.Errorf("truenas: the original has more disks than the %d restored", len(spec.Disks)))
			}
			a := map[string]any{"dtype": "DISK", "path": spec.Disks[disk].Path}
			for _, k := range []string{"type", "iotype", "serial", "logical_sectorsize", "physical_sectorsize"} {
				if v, ok := d.Attributes[k]; ok {
					a[k] = v
				}
			}
			disk++
			devices = append(devices, a)
		case "NIC":
			a := map[string]any{"dtype": "NIC"} // no MAC: the copy gets its own
			for _, k := range []string{"type", "nic_attach", "trust_guest_rx_filters"} {
				if v, ok := d.Attributes[k]; ok {
					a[k] = v
				}
			}
			if nic < len(spec.Networks) {
				a["nic_attach"] = spec.Networks[nic]
			}
			nic++
			devices = append(devices, a)
		case "DISPLAY":
			a := map[string]any{"dtype": "DISPLAY"} // no port or password: TrueNAS picks the port
			for _, k := range []string{"type", "bind", "resolution", "web", "wait"} {
				if v, ok := d.Attributes[k]; ok {
					a[k] = v
				}
			}
			devices = append(devices, a)
		default:
			skipped = append(skipped, fmt.Sprintf("%v device: not carried over", d.Attributes["dtype"]))
		}
	}
	if len(orig.Devices) == 0 { // no saved settings: just the disks and NICs asked for
		for _, d := range spec.Disks {
			devices = append(devices, map[string]any{"dtype": "DISK", "path": d.Path, "type": "VIRTIO"})
		}
		for _, n := range spec.Networks {
			devices = append(devices, map[string]any{"dtype": "NIC", "type": "VIRTIO", "nic_attach": n})
		}
	}
	for i, a := range devices {
		if _, err := m.call(ctx, mVMDeviceCreate, map[string]any{"vm": made.ID, "attributes": a, "order": 1000 + i}); err != nil {
			return fail(fmt.Errorf("truenas: adding the %v device: %w", a["dtype"], err))
		}
	}
	return &VM{Name: spec.Name, UUID: made.UUID, Skipped: skipped}, nil
}
