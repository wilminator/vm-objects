// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wilminator/vm-objects/internal/libvirt"
	"github.com/wilminator/vm-objects/internal/manifest"
)

const testUUID = "aaaaaaaa-1111-2222-3333-444444444444"

// fakeVirsh answers domstate and dumpxml for one domain.
type fakeVirsh struct {
	state string
	xml   string
	calls []string
}

func (f *fakeVirsh) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "domstate":
		return f.state + "\n", nil
	case "dumpxml":
		return f.xml, nil
	case "domfsfreeze", "domfsthaw":
		return "", nil
	}
	return "", errors.New("unexpected virsh " + args[0])
}

func domainXML(nvram string) string {
	return `<domain type='kvm'><name>win11</name><uuid>` + testUUID + `</uuid>
<os><type>hvm</type><nvram>` + nvram + `</nvram></os>
<devices>
  <disk type='block' device='disk'><driver name='qemu' type='raw'/><source dev='/dev/zvol/tank/win11'/><target dev='vda'/></disk>
  <disk type='file' device='cdrom'><target dev='hdc'/><readonly/></disk>
  <tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm>
</devices></domain>`
}

func TestLibvirtDescribe(t *testing.T) {
	dir := t.TempDir()
	nvram := filepath.Join(dir, "VARS.fd")
	if err := os.WriteFile(nvram, []byte("nvram"), 0o600); err != nil {
		t.Fatal(err)
	}
	swtpm := filepath.Join(dir, "swtpm")
	if err := os.MkdirAll(filepath.Join(swtpm, testUUID, "tpm2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(swtpm, testUUID, "tpm2", "tpm2-00.permall"), []byte("tpm"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := &fakeVirsh{state: "running", xml: domainXML(nvram)}
	p := NewLibvirt(v, swtpm)
	vm, err := p.Describe(context.Background(), "win11")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "libvirt" || vm.Name != "win11" || vm.UUID != testUUID || !vm.Running || vm.DefinitionType != "libvirt-xml" {
		t.Fatalf("described %+v (platform %s)", vm, p.Name())
	}
	if len(vm.Disks) != 1 || vm.Disks[0].Target != "vda" || vm.Disks[0].Path != "/dev/zvol/tank/win11" {
		t.Fatalf("disks = %+v", vm.Disks)
	}
	roles := map[string]string{}
	for _, f := range vm.Files {
		roles[f.Role] = f.Name + "=" + string(f.Data)
	}
	if roles["nvram"] != "VARS.fd=nvram" || roles["tpm"] != "tpm2/tpm2-00.permall=tpm" {
		t.Fatalf("files = %v", roles)
	}
	if err := p.Freeze(context.Background(), "win11"); err != nil {
		t.Fatal(err)
	}
}

func TestQNAPDefaults(t *testing.T) {
	p := NewQNAP(nil, "")
	if p.Name() != "qnap" || p.SwtpmDir != QNAPSwtpmDir {
		t.Fatalf("qnap module: name %s, swtpm %s", p.Name(), p.SwtpmDir)
	}
	if v, ok := p.Virsh.(libvirt.Virsh); !ok || v.Path != QNAPVirsh {
		t.Fatalf("qnap virsh = %#v", p.Virsh)
	}
	if NewLibvirt(nil, "").SwtpmDir != DefaultSwtpmDir {
		t.Fatal("libvirt module doesn't default its TPM folder")
	}
}

func TestMissingTPMStateNamesTheFolder(t *testing.T) {
	v := &fakeVirsh{state: "shut off", xml: strings.Replace(domainXML(""), "<nvram></nvram>", "", 1)}
	_, err := NewLibvirt(v, t.TempDir()).Describe(context.Background(), "win11")
	if err == nil || !strings.Contains(err.Error(), testUUID) || !strings.Contains(err.Error(), "--swtpm-dir") {
		t.Fatalf("missing TPM state: %v", err)
	}
}

func TestVMFilesSizeLimit(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.fd")
	if err := os.WriteFile(big, make([]byte, manifest.MaxVMFiles+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVMFiles(big, ""); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("oversized NVRAM: %v", err)
	}
}

func TestCheck(t *testing.T) {
	for _, n := range Names {
		if got, err := Check(n); err != nil || got != n {
			t.Fatalf("Check(%q) = %q, %v", n, got, err)
		}
	}
	if _, err := Check("vmware"); err == nil {
		t.Fatal("Check accepted an unknown platform")
	}
	if got, err := Check("auto"); err != nil || got == "" {
		t.Fatalf("Check(auto) = %q, %v", got, err)
	}
}

func TestLibvirtSettings(t *testing.T) {
	if got := TrueNASLibvirt.ConnectURI(); got != TrueNASLibvirtURI {
		t.Fatalf("TrueNAS URI %q", got)
	}
	if got := (LibvirtConfig{Socket: "/s", URI: "qemu:///x"}).ConnectURI(); got != "qemu:///x" {
		t.Fatalf("URI should win over socket: %q", got)
	}
	if got := DefaultLibvirt.ConnectURI(); got != "" {
		t.Fatalf("generic libvirt should use virsh's default, got %q", got)
	}
	c := QNAPLibvirt.With(LibvirtConfig{SwtpmDir: "/elsewhere"})
	if c.Virsh != QNAPVirsh || c.SwtpmDir != "/elsewhere" {
		t.Fatalf("override: %+v", c)
	}

	// The generic module with an appliance's socket given by hand.
	p, err := New("libvirt", LibvirtConfig{Socket: "/run/other/libvirt-sock", Virsh: "/opt/bin/virsh"})
	if err != nil {
		t.Fatal(err)
	}
	l := p.(*Libvirt)
	if v := l.Virsh.(libvirt.Virsh); v.Path != "/opt/bin/virsh" || v.Connect != "qemu+unix:///system?socket=/run/other/libvirt-sock" || l.SwtpmDir != DefaultSwtpmDir {
		t.Fatalf("generic with overrides: %+v %+v", v, l)
	}
	p, err = New("truenas", LibvirtConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tn := p.(*TrueNAS)
	if v := tn.Virsh.(libvirt.Virsh); v.Connect != TrueNASLibvirtURI || tn.Config.TPMLayout != "flat" {
		t.Fatalf("truenas: %+v %+v", v, tn.Config)
	}
	if _, err := New("vmware", LibvirtConfig{}); err == nil {
		t.Fatal("unknown platform accepted")
	}
}
