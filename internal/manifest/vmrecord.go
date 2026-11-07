// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package manifest

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// VMRecord ties one whole-VM backup together: every disk's manifest, the
// libvirt definition, and the small state files a VM needs to boot as it
// was (UEFI variables, TPM state). It is written after all of its disks'
// manifests, so a record only exists for a complete VM backup.
type VMRecord struct {
	Format      int       `json:"format"`
	VM          string    `json:"vm"`
	Name        string    `json:"name"`
	Kind        Kind      `json:"kind"`
	Created     time.Time `json:"created"`
	Profile     string    `json:"profile,omitempty"`
	RetainUntil time.Time `json:"retainUntil,omitzero"`

	// Platform is the module that described the VM: libvirt, qnap or
	// truenas. Empty in records made before modules existed: libvirt.
	Platform   string `json:"platform,omitempty"`
	DomainName string `json:"domainName"`
	UUID       string `json:"uuid"`
	Definition []byte `json:"definition"` // the platform's definition, as read
	// DefinitionType names Definition's format; empty means libvirt XML.
	DefinitionType string `json:"definitionType,omitempty"`
	// Consistency says how the disks were captured: "shut off",
	// "snapshot" (crash-consistent) or "quiesced snapshot".
	Consistency string   `json:"consistency"`
	Disks       []VMDisk `json:"disks"`
	Files       []VMFile `json:"files,omitempty"`
	Skipped     []string `json:"skipped,omitempty"`
}

// VMDisk is one disk of a VM backup.
type VMDisk struct {
	Target   string `json:"target"`   // vda, sda, ...
	Stream   string `json:"stream"`   // the disk's own backup series: "<vm>.<target>"
	Manifest string `json:"manifest"` // that series' manifest for this run
	Type     string `json:"type"`     // file or block, as defined
	Format   string `json:"format"`   // qcow2 or raw, as defined
	Path     string `json:"path"`     // where the disk was
}

// VMFile is a small file the VM needs, stored whole inside the record.
type VMFile struct {
	// Role is "nvram", "tpm", or "config": the platform's own settings
	// for the VM (TrueNAS's), restored next to the definition.
	Role string `json:"role"`
	// Name is the file's name: the NVRAM file's base name, a path
	// relative to the TPM state directory, or the settings file's name.
	Name string `json:"name"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}

// MaxVMFiles is the most file data a record may carry. NVRAM stores are
// usually 128 KiB to 4 MiB and TPM state a few KiB.
const MaxVMFiles = 16 << 20

// DiskStream names the backup series for one disk of a VM.
func DiskStream(vm, target string) string { return vm + "." + target }

// Validate checks a record is internally consistent before use.
func (r *VMRecord) Validate() error {
	if r.Format != Format {
		return fmt.Errorf("VM record format %d is not supported", r.Format)
	}
	if err := CheckVMName(r.VM); err != nil {
		return err
	}
	if len(r.Disks) == 0 {
		return fmt.Errorf("VM record %s has no disks", r.Name)
	}
	total := 0
	for _, d := range r.Disks {
		if d.Stream != DiskStream(r.VM, d.Target) || CheckVMName(d.Stream) != nil || d.Manifest == "" {
			return fmt.Errorf("VM record %s has a malformed disk entry %q", r.Name, d.Target)
		}
	}
	for _, f := range r.Files {
		// File names are used when restoring, so they must stay inside the
		// restore folder.
		clean := path.Clean(f.Name)
		if f.Name == "" || clean != f.Name || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("VM record %s has an unsafe file name %q", r.Name, f.Name)
		}
		if f.Role != "nvram" && f.Role != "tpm" && f.Role != "config" {
			return fmt.Errorf("VM record %s has a file with unknown role %q", r.Name, f.Role)
		}
		total += len(f.Data)
	}
	if total > MaxVMFiles {
		return fmt.Errorf("VM record %s carries %d bytes of files, over the %d limit", r.Name, total, MaxVMFiles)
	}
	return nil
}
