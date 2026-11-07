// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package vmdef

import (
	"strings"
	"testing"
)

// A definition in the style libvirt and QNAP Virtualization Station
// write, with things a careless rewrite would damage: a comment, both
// quote styles, a QEMU namespace block and a CD-ROM.
const sample = `<!-- managed by QVS: do not edit -->
<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>WinGames</name>
  <uuid>7240004C-9F00-4ACA-8577-8987C8073490</uuid>
  <memory unit='KiB'>8388608</memory>
  <os>
    <type arch='x86_64' machine='pc-i440fx-4.1'>hvm</type>
    <loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader>
    <nvram>/var/lib/libvirt/qemu/nvram/WinGames_VARS.fd</nvram>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='writeback'/>
      <source file='/share/VMs/WinGames/WinGames/top.qcow2'/>
      <target dev='hda' bus='ide'/>
    </disk>
    <disk type="block" device="disk">
      <driver name="qemu" type="raw"/>
      <source dev="/dev/zvol/tank/vms/data"/>
      <target dev="vdb" bus="virtio"/>
    </disk>
    <disk type='file' device='cdrom'>
      <source file='/share/ISO/virtio-win.iso'/>
      <target dev='hdc' bus='ide'/>
      <readonly/>
    </disk>
    <tpm model='tpm-crb'>
      <backend type='emulator' version='2.0'/>
    </tpm>
  </devices>
  <qemu:commandline>
    <qemu:arg value='-cpu'/>
  </qemu:commandline>
</domain>
`

func TestParse(t *testing.T) {
	d, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "WinGames" || d.UUID != "7240004c-9f00-4aca-8577-8987c8073490" {
		t.Fatalf("name %q uuid %q", d.Name, d.UUID)
	}
	if d.NVRAM != "/var/lib/libvirt/qemu/nvram/WinGames_VARS.fd" || !d.TPM || d.TPMVersion != "2.0" {
		t.Fatalf("nvram %q tpm %v %q", d.NVRAM, d.TPM, d.TPMVersion)
	}
	if len(d.Disks) != 2 {
		t.Fatalf("disks %+v", d.Disks)
	}
	if d.Disks[0] != (Disk{Target: "hda", Type: "file", Format: "qcow2", Path: "/share/VMs/WinGames/WinGames/top.qcow2"}) {
		t.Fatalf("hda %+v", d.Disks[0])
	}
	if d.Disks[1] != (Disk{Target: "vdb", Type: "block", Format: "raw", Path: "/dev/zvol/tank/vms/data"}) {
		t.Fatalf("vdb %+v", d.Disks[1])
	}
	if len(d.Skipped) != 1 || !strings.Contains(d.Skipped[0], "cdrom hdc") {
		t.Fatalf("skipped %v", d.Skipped)
	}
}

func TestRewriteChangesOnlyWhatItShould(t *testing.T) {
	out, err := Rewrite([]byte(sample), Changes{
		Disks: map[string]DiskChange{
			"hda": {Path: "/restore/WinGames-hda.raw", Format: "raw"},
			"vdb": {Path: "/restore/WinGames-vdb.raw", AsFile: true},
		},
		NVRAM: "/restore/WinGames_VARS.fd",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := sample
	for _, r := range [][2]string{
		{`type='qcow2' cache`, `type='raw' cache`},
		{`file='/share/VMs/WinGames/WinGames/top.qcow2'`, `file='/restore/WinGames-hda.raw'`},
		{`<disk type="block"`, `<disk type="file"`},
		{`<source dev="/dev/zvol/tank/vms/data"/>`, `<source file="/restore/WinGames-vdb.raw"/>`},
		{`<nvram>/var/lib/libvirt/qemu/nvram/WinGames_VARS.fd</nvram>`, `<nvram>/restore/WinGames_VARS.fd</nvram>`},
	} {
		if !strings.Contains(want, r[0]) {
			t.Fatalf("test setup: %q not in sample", r[0])
		}
		want = strings.Replace(want, r[0], r[1], 1)
	}
	if string(out) != want {
		t.Fatalf("rewrite changed more (or less) than intended:\n%s", out)
	}
	d, _ := Parse(out)
	if d.Disks[1] != (Disk{Target: "vdb", Type: "file", Format: "raw", Path: "/restore/WinGames-vdb.raw"}) {
		t.Fatalf("vdb after rewrite %+v", d.Disks[1])
	}
}

func TestRewriteEscapesAndRefusesUnknownDisks(t *testing.T) {
	out, err := Rewrite([]byte(sample), Changes{Disks: map[string]DiskChange{"hda": {Path: `/r/it's "odd" & <x>.raw`}}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(out)
	if err != nil || d.Disks[0].Path != `/r/it's "odd" & <x>.raw` {
		t.Fatalf("path after escaping: %q, %v", d.Disks[0].Path, err)
	}
	if _, err := Rewrite([]byte(sample), Changes{Disks: map[string]DiskChange{"vdz": {Path: "/x"}}}); err == nil {
		t.Fatal("rewrote a disk that doesn't exist")
	}
	// The CD-ROM is not a disk to rewrite.
	if _, err := Rewrite([]byte(sample), Changes{Disks: map[string]DiskChange{"hdc": {Path: "/x"}}}); err == nil {
		t.Fatal("rewrote the CD-ROM")
	}
}

func TestParseRefusesUnsupported(t *testing.T) {
	for _, bad := range []string{
		strings.Replace(sample, `<disk type='file' device='disk'>`, `<disk type='network' device='disk'>`, 1),
		strings.Replace(sample, `<target dev="vdb"`, `<target dev="hda"`, 1),
		strings.Replace(sample, `<uuid>7240004C-9F00-4ACA-8577-8987C8073490</uuid>`, ``, 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad[:200])
		}
	}
}

func TestRewriteBlockToBlock(t *testing.T) {
	out, err := Rewrite([]byte(sample), Changes{Disks: map[string]DiskChange{"vdb": {Path: "/dev/zvol/tank/restore/data"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sample, `/dev/zvol/tank/vms/data`, `/dev/zvol/tank/restore/data`, 1)
	if string(out) != want {
		t.Fatalf("block to block rewrite:\n%s", out)
	}
}

func TestRewriteNewNameDropsUUID(t *testing.T) {
	out, err := Rewrite([]byte(sample), Changes{Name: "WinGames-restored", DropUUID: true})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sample, "<name>WinGames</name>", "<name>WinGames-restored</name>", 1)
	want = strings.Replace(want, "<uuid>7240004C-9F00-4ACA-8577-8987C8073490</uuid>", "", 1)
	if string(out) != want {
		t.Fatalf("rename:\n%s", out)
	}
}
