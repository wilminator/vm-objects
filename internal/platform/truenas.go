// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

// TrueNAS SCALE (25.10 "Goldeye" and later) runs its own libvirt, reached
// through its own socket, and keeps each VM's real configuration in its
// middleware (midclt), from which it writes libvirt's definition at start.
const (
	TrueNASMidclt        = "/usr/bin/midclt"
	TrueNASLibvirtSocket = "/run/truenas_libvirt/libvirt-sock"
	// TrueNASLibvirtURI connects virsh to that libvirt.
	TrueNASLibvirtURI = "qemu+unix:///system?socket=" + TrueNASLibvirtSocket
)
