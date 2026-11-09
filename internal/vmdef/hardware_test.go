// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package vmdef

import "testing"

func TestParseHardware(t *testing.T) {
	h, err := ParseHardware([]byte(`<domain type='kvm'><name>w</name><memory unit='KiB'>16777216</memory><vcpu placement='static'>4</vcpu>
<os><type arch='x86_64' machine='pc-q35-7.2'>hvm</type><loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader></os>
<devices><disk type='block' device='disk'><target dev='vda' bus='virtio'/></disk><disk type='file' device='cdrom'><target dev='sda' bus='sata'/></disk>
<interface type='bridge'><source bridge='br0'/></interface></devices></domain>`))
	if err != nil {
		t.Fatal(err)
	}
	if h.Memory != 16<<30 || h.VCPUs != 4 || !h.UEFI || h.Machine != "q35" || h.Buses["vda"] != "virtio" || len(h.Buses) != 1 || len(h.Bridges) != 1 || h.Bridges[0] != "br0" {
		t.Fatalf("%+v", h)
	}
	if _, err := ParseHardware([]byte(`<domain><memory unit='XB'>1</memory><vcpu>1</vcpu></domain>`)); err == nil {
		t.Fatal("an unknown memory unit was accepted")
	}
}
