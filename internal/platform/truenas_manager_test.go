// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// stateMidclt is a small stateful TrueNAS middleware: one VM, whose state
// follows vm.start and vm.stop, and which records what vm.create and
// vm.device.create were asked.
type stateMidclt struct {
	state     string
	calls     []string
	jobs      []string
	created   map[string]any
	devices   []map[string]any
	deleted   []string
	failDev   string // a device type whose creation fails
	stopStay  bool   // vm.stop leaves the VM running (a guest that ignores it)
	exists    bool   // a VM named "Copy" already exists
	uuidTaken string // a VM with this UUID exists
}

func (f *stateMidclt) Call(ctx context.Context, method string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, method)
	switch method {
	case "vm.query":
		var filter [][]any
		json.Unmarshal([]byte(args[0]), &filter)
		f0 := filter[0]
		if f0[0] == "uuid" && f.uuidTaken != "" && f0[2] == f.uuidTaken {
			return []byte(`[{"id":4,"name":"Other","uuid":"` + f.uuidTaken + `"}]`), nil
		}
		if f0[0] == "name" && f0[2] == "Games" || f0[0] == "id" && f0[2] == float64(3) {
			return []byte(fmt.Sprintf(`[{"id":3,"name":"Games","uuid":"u-3","status":{"state":%q}}]`, f.state)), nil
		}
		if f.exists && f0[0] == "name" && f0[2] == "Copy" {
			return []byte(`[{"id":9,"name":"Copy","status":{"state":"STOPPED"}}]`), nil
		}
		return []byte("[]"), nil
	case "vm.start":
		f.state = "RUNNING"
		return []byte("true"), nil
	case "vm.create":
		json.Unmarshal([]byte(args[0]), &f.created)
		return []byte(`{"id":12,"name":"Copy","uuid":"u-12"}`), nil
	case "vm.device.create":
		var d map[string]any
		json.Unmarshal([]byte(args[0]), &d)
		if a, _ := d["attributes"].(map[string]any); a["dtype"] == f.failDev {
			return nil, errors.New("middleware: [EINVAL] bad device")
		}
		f.devices = append(f.devices, d)
		return []byte(`{"id":50}`), nil
	case "vm.delete":
		f.deleted = append(f.deleted, args[0])
		return []byte("true"), nil
	}
	return nil, fmt.Errorf("unexpected midclt call %s %v", method, args)
}

func (f *stateMidclt) CallJob(ctx context.Context, method string, args ...string) ([]byte, error) {
	f.jobs = append(f.jobs, method)
	if method == "vm.stop" && !f.stopStay {
		f.state = "STOPPED"
	}
	return []byte("null"), nil
}

func newManaged(f *stateMidclt) *TrueNASManaged {
	m := NewTrueNASManaged(NewTrueNAS(f, &fakeVirsh{}))
	m.PollEvery = time.Millisecond
	return m
}

func TestTrueNASManagedStartStop(t *testing.T) {
	f := &stateMidclt{state: "STOPPED"}
	m := newManaged(f)
	ctx := context.Background()
	vm := &VM{Name: "Games"}
	if st, err := m.State(ctx, vm); err != nil || st != StateStopped {
		t.Fatalf("%q %v", st, err)
	}
	if err := m.Start(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.State(ctx, vm); st != StateRunning {
		t.Fatalf("state %q after start", st)
	}
	if err := m.Start(ctx, vm); err != nil { // already running: nothing to do
		t.Fatal(err)
	}
	if err := m.Stop(ctx, vm, time.Second, false); err != nil {
		t.Fatal(err)
	}
	if len(f.jobs) != 1 || f.jobs[0] != "vm.stop" {
		t.Fatalf("vm.stop should run as a job: %v", f.jobs)
	}
	// A guest that ignores the request: an error, not a hang.
	f.state, f.stopStay = "RUNNING", true
	if err := m.Stop(ctx, vm, 20*time.Millisecond, false); err == nil {
		t.Fatal("a VM that kept running was reported stopped")
	}
}

func TestTrueNASManagedCreate(t *testing.T) {
	f := &stateMidclt{state: "STOPPED"}
	m := newManaged(f)
	cfg, err := trueNASConfig(json.RawMessage(tnVMQuery[1 : len(tnVMQuery)-1])) // the one record
	if err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{Name: "Copy", Memory: 4096 << 20, UEFI: true, TPM: true,
		Disks: []DiskSpec{{Target: "vda", Path: "/dev/zvol/tank/vms/copy"}}, Networks: []string{"br0"},
		Definition: cfg, DefinitionType: "truenas"}
	vm, err := m.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name != "Copy" || vm.UUID != "u-12" {
		t.Fatalf("%+v", vm)
	}
	c := f.created
	if c["name"] != "Copy" || c["autostart"] != false || c["memory"] != float64(4096) ||
		c["command_line_args"] == nil || c["machine_type"] != "pc-i440fx-10.0" || c["bootloader"] != "UEFI" {
		t.Fatalf("vm.create got %v", c)
	}
	if _, ok := c["id"]; ok || c["uuid"] != nil {
		t.Fatalf("the copy kept the original's identity: %v", c)
	}
	var disk, display map[string]any
	for _, d := range f.devices {
		a := d["attributes"].(map[string]any)
		switch a["dtype"] {
		case "DISK":
			disk = a
		case "DISPLAY":
			display = a
		}
		if d["vm"] != float64(12) {
			t.Fatalf("device for another VM: %v", d)
		}
	}
	if disk["path"] != "/dev/zvol/tank/vms/copy" || disk["type"] != "VIRTIO" {
		t.Fatalf("disk %v", disk)
	}
	if display == nil || display["password"] != nil || display["port"] != nil {
		t.Fatalf("display %v: no password or port may be sent", display)
	}
	for _, call := range f.calls {
		if call == "vm.start" {
			t.Fatal("Create started the VM")
		}
	}
}

func TestTrueNASManagedCreateRefusals(t *testing.T) {
	disk := []DiskSpec{{Target: "vda", Path: "/dev/zvol/tank/vms/copy"}}
	for name, spec := range map[string]VMSpec{
		"dash in name": {Name: "Win-Games", Disks: disk},
		"flag as name": {Name: "--force", Disks: disk},
		"no disks":     {Name: "Copy"},
		"not a zvol":   {Name: "Copy", Disks: []DiskSpec{{Target: "vda", Path: "/share/VMs/x.qcow2"}}},
		"dotdot":       {Name: "Copy", Disks: []DiskSpec{{Target: "vda", Path: "/dev/zvol/../etc/passwd"}}},
	} {
		f := &stateMidclt{state: "STOPPED"}
		if _, err := newManaged(f).Create(context.Background(), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
		for _, c := range f.calls {
			if c == "vm.create" {
				t.Errorf("%s: reached vm.create", name)
			}
		}
	}
	f := &stateMidclt{state: "STOPPED", exists: true}
	if _, err := newManaged(f).Create(context.Background(), VMSpec{Name: "Copy", Disks: disk}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("an existing name: %v", err)
	}
}

// A device that fails removes the half-made VM, and only the VM, never its zvols.
func TestTrueNASManagedCreateCleansUp(t *testing.T) {
	f := &stateMidclt{state: "STOPPED", failDev: "DISPLAY"}
	cfg, _ := trueNASConfig(json.RawMessage(tnVMQuery[1 : len(tnVMQuery)-1]))
	_, err := newManaged(f).Create(context.Background(), VMSpec{Name: "Copy",
		Disks:      []DiskSpec{{Target: "vda", Path: "/dev/zvol/tank/vms/copy"}},
		Definition: cfg, DefinitionType: "truenas"})
	if err == nil || !strings.Contains(err.Error(), "DISPLAY") {
		t.Fatalf("got %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "12" {
		t.Fatalf("deleted %v", f.deleted)
	}
}

// Only the closed set of changing calls can be made.
func TestTrueNASManagedRefusesOtherMethods(t *testing.T) {
	m := newManaged(&stateMidclt{})
	for _, bad := range []string{"vm.update", "pool.dataset.delete", "filesystem.setperm", "core.bulk"} {
		if _, err := m.call(context.Background(), bad, 1); err == nil {
			t.Errorf("%s was allowed", bad)
		}
	}
}

// A restore makes a new UUID by default; --reuse-uuid keeps the original's
// (the guest's SMBIOS UUID), unless another VM already holds it.
func TestTrueNASManagedReuseUUID(t *testing.T) {
	const orig = "0a1b2c3d-1111-4222-8333-444455556666"
	var rec map[string]any
	cfg, _ := trueNASConfig(json.RawMessage(tnVMQuery[1 : len(tnVMQuery)-1]))
	json.Unmarshal(cfg, &rec)
	rec["uuid"] = orig
	cfg, _ = json.Marshal(rec)
	spec := VMSpec{Name: "Copy", Memory: 4096 << 20, UEFI: true,
		Disks: []DiskSpec{{Target: "vda", Path: "/dev/zvol/tank/vms/copy"}}, Definition: cfg, DefinitionType: "truenas"}

	f := &stateMidclt{state: "STOPPED"}
	if _, err := newManaged(f).Create(context.Background(), spec); err != nil || f.created["uuid"] != nil {
		t.Fatalf("default: %v, uuid %v", err, f.created["uuid"])
	}
	spec.ReuseUUID = true
	f = &stateMidclt{state: "STOPPED"}
	if _, err := newManaged(f).Create(context.Background(), spec); err != nil || f.created["uuid"] != orig {
		t.Fatalf("reuse: %v, uuid %v", err, f.created["uuid"])
	}
	f = &stateMidclt{state: "STOPPED", uuidTaken: orig}
	if _, err := newManaged(f).Create(context.Background(), spec); err == nil || f.created != nil {
		t.Fatalf("a taken UUID was reused: %v", err)
	}
	spec.Definition = nil
	f = &stateMidclt{state: "STOPPED"}
	if _, err := newManaged(f).Create(context.Background(), spec); err == nil {
		t.Fatal("reuse without a saved UUID was allowed")
	}
}
