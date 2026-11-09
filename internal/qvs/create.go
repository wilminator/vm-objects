// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package qvs

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// Creating a VM on existing disk images, for a full restore: POST
// /qvs/v2/vms (confirmed to exist, POST only), with the fields of VS
// 4.2's CreateVMSerializer (read from its code; confirmed by the first
// create). The VM is never started.

const routeCreateVM = "/qvs/v2/vms"

// CreateSpec is a VM to create.
type CreateSpec struct {
	Name    string
	OSType  string // one of GET /qvs/os_types, e.g. win11
	UEFI    bool
	Machine string // q35, pc
	Cores   int
	Memory  int64 // bytes
	Disks   []CreateDisk
	Bridges []string // one NIC per bridge
}

// CreateDisk is an existing image to attach.
type CreateDisk struct {
	Path string // a local path under /share/<share>/...
	Bus  string // virtio, sata, ide, scsi
}

type createVM struct {
	Name     string          `json:"name"`
	OSType   string          `json:"os_type"`
	BIOS     string          `json:"bios"`
	Machine  string          `json:"machine,omitempty"`
	Cores    int             `json:"cores"`
	Memory   int64           `json:"memory"`
	Disks    []createDisk    `json:"disks"`
	Adapters []createAdapter `json:"adapters"`
	Graphics []createGraphic `json:"graphics"`
	CDROMs   []struct{}      `json:"cdroms"`
}

type createDisk struct {
	CreatingImage bool   `json:"creating_image"` // false: use the image at path
	Path          string `json:"path"`
	Bus           string `json:"bus"`
	Cache         string `json:"cache"`
}

type createAdapter struct {
	Type   string `json:"type"`
	Bridge string `json:"bridge"`
	Model  string `json:"model"`
}

// createGraphic never carries a password: vmobj sends none.
type createGraphic struct {
	Type           string `json:"type"`
	LocalhostOnly  bool   `json:"localhost_only"`
	EnablePassword bool   `json:"enable_password"`
}

var vmNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.()-]{0,63}$`)
var shareDataRE = regexp.MustCompile(`^/share/CACHEDEV[0-9]+_DATA/`)
var busRE = regexp.MustCompile(`^(virtio|sata|ide|scsi)$`)
var wordRE = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)

// SharedPath turns a local path under a share (/share/VMs/x/y, or its
// /share/CACHEDEV1_DATA/VMs/x/y form) into Virtualization Station's
// shared://VMs/x/y. It refuses anything else: other places, "..", odd
// characters.
func SharedPath(p string) (string, error) {
	if !path.IsAbs(p) || path.Clean(p) != p || strings.Contains(p, "..") {
		return "", fmt.Errorf("qvs: %q isn't a clean absolute path", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' || r == '"' || r == '\'' || r == '`' || r == '$' {
			return "", fmt.Errorf("qvs: %q holds a character vmobj won't send", p)
		}
	}
	rest := shareDataRE.ReplaceAllString(p, "/share/")
	rest, ok := strings.CutPrefix(rest, "/share/")
	if !ok || strings.Count(rest, "/") < 1 || strings.HasPrefix(rest, ".") {
		return "", fmt.Errorf("qvs: %q isn't a file inside a share (/share/<share>/...)", p)
	}
	return "shared://" + rest, nil
}

// CreateVM defines a VM on existing images and returns it once VS lists
// it. It refuses a name already in use. The VM is not started.
func (c *Client) CreateVM(ctx context.Context, s CreateSpec) (*VM, error) {
	if !vmNameRE.MatchString(s.Name) {
		return nil, fmt.Errorf("qvs: VM name %q: letters, digits, space, _ . ( ) -, up to 64, not starting with a symbol", s.Name)
	}
	if !wordRE.MatchString(s.OSType) || (s.Machine != "" && !wordRE.MatchString(s.Machine)) {
		return nil, fmt.Errorf("qvs: os type %q / machine %q", s.OSType, s.Machine)
	}
	if s.Cores < 1 || s.Cores > 256 || s.Memory < 256<<20 || len(s.Disks) == 0 {
		return nil, fmt.Errorf("qvs: a VM needs cores, at least 256 MiB of memory and a disk")
	}
	in := createVM{Name: s.Name, OSType: s.OSType, BIOS: "seabios", Machine: s.Machine, Cores: s.Cores, Memory: s.Memory,
		Adapters: []createAdapter{}, CDROMs: []struct{}{},
		Graphics: []createGraphic{{Type: "vnc", LocalhostOnly: true, EnablePassword: false}}}
	if s.UEFI {
		in.BIOS = "ovmf"
	}
	for _, d := range s.Disks {
		sp, err := SharedPath(d.Path)
		if err != nil {
			return nil, err
		}
		bus := d.Bus
		if bus == "" {
			bus = "virtio"
		}
		if !busRE.MatchString(bus) {
			return nil, fmt.Errorf("qvs: disk bus %q", bus)
		}
		in.Disks = append(in.Disks, createDisk{CreatingImage: false, Path: sp, Bus: bus, Cache: "writeback"})
	}
	for _, b := range s.Bridges {
		if !wordRE.MatchString(strings.ToLower(b)) {
			return nil, fmt.Errorf("qvs: bridge %q", b)
		}
		in.Adapters = append(in.Adapters, createAdapter{Type: "bridge", Bridge: b, Model: "virtio"})
	}
	vms, err := c.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		if v.Name == s.Name {
			return nil, fmt.Errorf("qvs: a VM named %q exists already (id %d); pick another name", s.Name, v.ID)
		}
	}
	if err := c.call(ctx, http.MethodPost, routeCreateVM, in, nil); err != nil {
		return nil, err
	}
	var got *VM
	err = c.until(ctx, func() (bool, error) {
		vms, err := c.ListVMs(ctx)
		for i := range vms {
			if vms[i].Name == s.Name {
				got = &vms[i]
				return true, nil
			}
		}
		return false, err
	})
	if err != nil {
		return nil, fmt.Errorf("creating %q: %w", s.Name, err)
	}
	return got, nil
}
