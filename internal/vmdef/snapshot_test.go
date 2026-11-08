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

// The shape of a Virtualization Station snapshot record (libvirt
// snapshot XML), with placeholder names.
const vsSnapshot = `<domainsnapshot>
  <name>1794103287</name>
  <description>before the update</description>
  <state>shutoff</state>
  <creationTime>1794103287</creationTime>
  <memory snapshot='no'/>
  <disks>
    <disk name='vda' snapshot='external' type='file'>
      <driver type='qcow2'/>
      <source file='/share/VMs/test/vm-vda.1794103287'/>
    </disk>
    <disk name='hda' snapshot='no'/>
  </disks>
  <domain type='kvm'><name>vm</name>
    <devices><graphics type='vnc' passwd='hunter2'/></devices>
  </domain>
</domainsnapshot>`

func TestParseSnapshot(t *testing.T) {
	s, err := ParseSnapshot([]byte(vsSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "1794103287" || s.Description != "before the update" || s.State != "shutoff" {
		t.Fatalf("%+v", s)
	}
	if s.Created.Unix() != 1794103287 {
		t.Fatalf("created %v", s.Created)
	}
	if len(s.Overlays) != 1 || s.Overlays["vda"] != "/share/VMs/test/vm-vda.1794103287" {
		t.Fatalf("overlays %v", s.Overlays)
	}
	if strings.Contains(strings.ToLower(s.Description+s.Name), "hunter2") {
		t.Fatal("read from the definition copy")
	}
	for _, bad := range []string{"<domainsnapshot><creationTime>1</creationTime></domainsnapshot>",
		"<domainsnapshot><name>x</name></domainsnapshot>", "<other/>",
		"<domainsnapshot><name>x</name><creationTime>1</creationTime><disks><disk name='vda' snapshot='external'/></disks></domainsnapshot>"} {
		if _, err := ParseSnapshot([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
