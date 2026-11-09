// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package vmdef

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// Hardware is what a restore needs to create a VM like the original on
// another platform: the rest of the definition is the platform's own.
type Hardware struct {
	Memory  int64 // bytes
	VCPUs   int
	UEFI    bool              // OVMF firmware, not BIOS
	Machine string            // "q35", "pc" (the libvirt machine type's family)
	Buses   map[string]string // disk target -> bus: virtio, sata, ide, scsi
	Bridges []string          // bridged NICs' bridge names, in order
}

// ParseHardware reads a domain definition's hardware.
func ParseHardware(data []byte) (*Hardware, error) {
	var raw struct {
		Memory struct {
			Unit  string `xml:"unit,attr"`
			Value string `xml:",chardata"`
		} `xml:"memory"`
		VCPU string `xml:"vcpu"`
		OS   struct {
			Firmware string `xml:"firmware,attr"`
			Type     struct {
				Machine string `xml:"machine,attr"`
			} `xml:"type"`
			Loader struct {
				Type  string `xml:"type,attr"`
				Value string `xml:",chardata"`
			} `xml:"loader"`
		} `xml:"os"`
		Devices struct {
			Disks []struct {
				Device string `xml:"device,attr"`
				Target struct {
					Dev string `xml:"dev,attr"`
					Bus string `xml:"bus,attr"`
				} `xml:"target"`
			} `xml:"disk"`
			Interfaces []struct {
				Type   string `xml:"type,attr"`
				Source struct {
					Bridge string `xml:"bridge,attr"`
				} `xml:"source"`
			} `xml:"interface"`
		} `xml:"devices"`
	}
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("domain definition: %w", err)
	}
	h := &Hardware{Buses: map[string]string{}}
	mem, err := strconv.ParseInt(strings.TrimSpace(raw.Memory.Value), 10, 64)
	if err != nil || mem <= 0 {
		return nil, fmt.Errorf("domain definition: memory %q", raw.Memory.Value)
	}
	switch strings.ToLower(raw.Memory.Unit) {
	case "", "kib", "k":
		h.Memory = mem << 10
	case "mib", "m":
		h.Memory = mem << 20
	case "gib", "g":
		h.Memory = mem << 30
	case "b", "bytes":
		h.Memory = mem
	default:
		return nil, fmt.Errorf("domain definition: memory unit %q", raw.Memory.Unit)
	}
	if h.VCPUs, err = strconv.Atoi(strings.TrimSpace(raw.VCPU)); err != nil || h.VCPUs <= 0 {
		return nil, fmt.Errorf("domain definition: vcpu %q", raw.VCPU)
	}
	loader := strings.ToLower(raw.OS.Loader.Value)
	h.UEFI = raw.OS.Firmware == "efi" || raw.OS.Loader.Type == "pflash" ||
		strings.Contains(loader, "ovmf") || strings.Contains(loader, "efi")
	switch m := strings.ToLower(raw.OS.Type.Machine); {
	case strings.Contains(m, "q35"):
		h.Machine = "q35"
	case m != "":
		h.Machine = "pc"
	}
	for _, d := range raw.Devices.Disks {
		if d.Device == "disk" && d.Target.Dev != "" {
			h.Buses[d.Target.Dev] = d.Target.Bus
		}
	}
	for _, i := range raw.Devices.Interfaces {
		if i.Type == "bridge" && i.Source.Bridge != "" {
			h.Bridges = append(h.Bridges, i.Source.Bridge)
		}
	}
	return h, nil
}
