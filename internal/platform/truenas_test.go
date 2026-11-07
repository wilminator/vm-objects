// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shapes below follow TrueNAS 25.10's vm.query and libvirt output;
// every identifying value is made up.
const tnUUID = "11111111-2222-3333-4444-555555555555"

const tnVMQuery = `[{
  "command_line_args": "-smbios \"type=1,manufacturer=qemu,serial=EXAMPLE-SERIAL\"",
  "cpu_mode": "HOST-PASSTHROUGH", "name": "Games", "vcpus": 1, "cores": 4, "memory": 8192,
  "trusted_platform_module": true, "bootloader": "UEFI", "bootloader_ovmf": "OVMF_CODE_4M.fd",
  "arch_type": "x86_64", "machine_type": "pc-i440fx-10.0", "uuid": "` + tnUUID + `", "id": 3,
  "devices": [
    {"id": 7, "attributes": {"dtype": "DISK", "path": "/dev/zvol/tank/vms/games", "type": "VIRTIO", "iotype": "IO_URING"}, "vm": 3},
    {"id": 8, "attributes": {"dtype": "DISPLAY", "type": "SPICE", "port": 5900, "password": "hunter2"}, "vm": 3}
  ],
  "display_available": true,
  "status": {"state": "STOPPED", "pid": null, "domain_state": "SHUTOFF"}
}]`

func tnXML(dir string) string {
	return `<domain type='kvm'>
  <name>3_Games</name>
  <uuid>` + tnUUID + `</uuid>
  <os firmware='efi'>
    <loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
    <nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd'>` + dir + `/nvram/3_Games_VARS.fd</nvram>
  </os>
  <devices>
    <disk type='block' device='disk'>
      <driver name='qemu' type='raw' cache='none' io='io_uring'/>
      <source dev='/dev/zvol/tank/vms/games'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <tpm model='tpm-crb'>
      <backend type='emulator' version='2.0' persistent_state='yes'>
        <source type='dir' path='` + dir + `/tpm/3_Games_tpm_state'/>
      </backend>
    </tpm>
    <graphics type='spice' port='5900' autoport='no' listen='0.0.0.0' passwd='hunter2'>
      <listen type='address' address='0.0.0.0'/>
    </graphics>
  </devices>
</domain>`
}

// fakeMidclt answers vm.query for one VM, by name or ID.
type fakeMidclt struct{ answer string }

func (f fakeMidclt) Call(_ context.Context, method string, args ...string) ([]byte, error) {
	if method != "vm.query" || len(args) != 1 {
		return nil, fmt.Errorf("unexpected midclt call %s %v", method, args)
	}
	var filter [][]any
	if err := json.Unmarshal([]byte(args[0]), &filter); err != nil {
		return nil, err
	}
	f0 := filter[0]
	match := (f0[0] == "name" && f0[2] == "Games") || (f0[0] == "id" && f0[2] == float64(3))
	if !match {
		return []byte("[]"), nil
	}
	return []byte(f.answer), nil
}

// trueNASHost lays out a TrueNAS-like /var/db/system/vm in a temp dir.
func trueNASHost(t *testing.T) (*TrueNAS, *fakeVirsh, string) {
	t.Helper()
	dir := t.TempDir()
	for p, data := range map[string]string{
		"nvram/3_Games_VARS.fd":                       "uefi vars",
		"tpm/3_Games_tpm_state/tpm2-00.permall":       "tpm state",
		"tpm/3_Games_tpm_state/tpm2-00.permall.blank": "blank",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v := &fakeVirsh{state: "shut off", xml: tnXML(dir)}
	return NewTrueNAS(fakeMidclt{answer: tnVMQuery}, v), v, dir
}

func TestTrueNASDescribe(t *testing.T) {
	tn, v, _ := trueNASHost(t)
	vm, err := tn.Describe(context.Background(), "Games")
	if err != nil {
		t.Fatal(err)
	}
	if tn.Name() != "truenas" || vm.Name != "Games" || vm.UUID != tnUUID || vm.Running {
		t.Fatalf("described %s %s running=%v", vm.Name, vm.UUID, vm.Running)
	}
	// virsh is asked about TrueNAS's libvirt name for it.
	for _, c := range v.calls {
		if !strings.HasSuffix(c, "3_Games") {
			t.Fatalf("virsh was asked about the wrong domain: %q", c)
		}
	}
	if len(vm.Disks) != 1 || vm.Disks[0].Path != "/dev/zvol/tank/vms/games" || vm.Disks[0].Type != "block" {
		t.Fatalf("disks = %+v", vm.Disks)
	}
	got := map[string]string{}
	var config []byte
	for _, f := range vm.Files {
		got[f.Role+":"+f.Name] = string(f.Data)
		if f.Role == "config" {
			config = f.Data
		}
	}
	// The NVRAM and TPM state come from where the definition says, with
	// TrueNAS's flat TPM layout kept as is.
	for k, want := range map[string]string{
		"nvram:3_Games_VARS.fd":       "uefi vars",
		"tpm:tpm2-00.permall":         "tpm state",
		"tpm:tpm2-00.permall.blank":   "blank",
		"config:" + TrueNASConfigFile: "",
	} {
		if _, ok := got[k]; !ok || (want != "" && got[k] != want) {
			t.Fatalf("file %s missing or wrong; have %v", k, keys(got))
		}
	}
	// The settings file keeps what recreating the VM needs, and no password
	// or moment-in-time state.
	for _, must := range []string{"command_line_args", "EXAMPLE-SERIAL", "machine_type", "IO_URING"} {
		if !strings.Contains(string(config), must) {
			t.Fatalf("settings file lacks %s:\n%s", must, config)
		}
	}
	for _, mustNot := range []string{"hunter2", "password", "STOPPED", "display_available"} {
		if strings.Contains(string(config), mustNot) {
			t.Fatalf("settings file still has %s:\n%s", mustNot, config)
		}
	}
	if strings.Contains(string(vm.Definition), "hunter2") || strings.Contains(string(vm.Definition), "passwd") {
		t.Fatal("the stored definition kept the display password")
	}
	if !strings.Contains(string(vm.Definition), "<graphics type='spice' port='5900'") {
		t.Fatal("removing the password damaged the graphics element")
	}
}

func TestTrueNASLookupByIDAndLibvirtName(t *testing.T) {
	tn, _, _ := trueNASHost(t)
	for _, name := range []string{"3", "3_Games"} {
		vm, err := tn.Describe(context.Background(), name)
		if err != nil || vm.Name != "Games" {
			t.Fatalf("Describe(%q) = %v, %v", name, vm, err)
		}
	}
	for _, name := range []string{"Nope", "4_Games", "3_Other"} {
		if _, err := tn.Describe(context.Background(), name); err == nil || !strings.Contains(err.Error(), "no VM") {
			t.Fatalf("Describe(%q): %v", name, err)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
