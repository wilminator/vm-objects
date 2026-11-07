// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wilminator/vm-objects/internal/libvirt"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// DefaultSwtpmDir is where libvirt keeps emulated TPM state, one folder
// per VM UUID.
const DefaultSwtpmDir = "/var/lib/libvirt/swtpm"

// QNAP Virtualization Station keeps its own copy of libvirt under /QVS.
const (
	QNAPVirsh    = "/QVS/usr/bin/virsh"
	QNAPSwtpmDir = "/QVS/var/lib/libvirt/swtpm"
)

// Libvirt describes VMs through virsh. It is the generic libvirt module,
// and, with QNAP's paths, the QNAP one.
type Libvirt struct {
	Virsh    libvirt.Runner
	SwtpmDir string // default DefaultSwtpmDir
	name     string
}

// NewLibvirt returns the generic libvirt module.
func NewLibvirt(virsh libvirt.Runner, swtpmDir string) *Libvirt {
	if swtpmDir == "" {
		swtpmDir = DefaultSwtpmDir
	}
	return &Libvirt{Virsh: virsh, SwtpmDir: swtpmDir, name: "libvirt"}
}

// NewQNAP returns the module for QNAP Virtualization Station: libvirt,
// with QNAP's virsh and TPM folder unless others are given.
func NewQNAP(virsh libvirt.Runner, swtpmDir string) *Libvirt {
	if virsh == nil {
		virsh = libvirt.Virsh{Path: QNAPVirsh}
	}
	if swtpmDir == "" {
		swtpmDir = QNAPSwtpmDir
	}
	return &Libvirt{Virsh: virsh, SwtpmDir: swtpmDir, name: "qnap"}
}

func (l *Libvirt) Name() string {
	if l.name == "" {
		return "libvirt"
	}
	return l.name
}

// Describe reads the domain's state, definition, NVRAM and TPM state.
func (l *Libvirt) Describe(ctx context.Context, domain string) (*VM, error) {
	state, err := libvirt.DomainState(ctx, l.Virsh, domain)
	if err != nil {
		return nil, err
	}
	xmlDef, err := libvirt.Definition(ctx, l.Virsh, domain)
	if err != nil {
		return nil, err
	}
	dom, err := vmdef.Parse(xmlDef)
	if err != nil {
		return nil, err
	}
	vm := &VM{
		Name: dom.Name, UUID: dom.UUID, Running: state != libvirt.ShutOff,
		Definition: xmlDef, DefinitionType: "libvirt-xml",
		Disks: dom.Disks, Skipped: dom.Skipped,
	}
	tpmDir := ""
	if dom.TPM {
		tpmDir = filepath.Join(l.SwtpmDir, dom.UUID)
	}
	if vm.Files, err = ReadVMFiles(dom.NVRAM, tpmDir); err != nil {
		return nil, err
	}
	return vm, nil
}

func (l *Libvirt) Freeze(ctx context.Context, domain string) error {
	return libvirt.Freeze(ctx, l.Virsh, domain)
}

func (l *Libvirt) Thaw(ctx context.Context, domain string) error {
	return libvirt.Thaw(ctx, l.Virsh, domain)
}

// ReadVMFiles reads a VM's NVRAM store (a file; "" for none) and its TPM
// state (every file under a folder; "" for none), within the size limit
// for a VM record.
func ReadVMFiles(nvram, tpmDir string) ([]manifest.VMFile, error) {
	var files []manifest.VMFile
	total := 0
	add := func(f manifest.VMFile) error {
		total += len(f.Data)
		if total > manifest.MaxVMFiles {
			return fmt.Errorf("the VM's NVRAM and TPM files exceed %d bytes", manifest.MaxVMFiles)
		}
		files = append(files, f)
		return nil
	}
	if nvram != "" {
		data, err := os.ReadFile(nvram)
		if err != nil {
			return nil, fmt.Errorf("reading the NVRAM store: %w", err)
		}
		if err := add(manifest.VMFile{Role: "nvram", Name: filepath.Base(nvram), Mode: 0o600, Data: data}); err != nil {
			return nil, err
		}
	}
	if tpmDir != "" {
		err := filepath.WalkDir(tpmDir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !e.Type().IsRegular() {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(tpmDir, p)
			return add(manifest.VMFile{Role: "tpm", Name: filepath.ToSlash(rel), Mode: uint32(info.Mode().Perm()), Data: data})
		})
		if err != nil {
			return nil, fmt.Errorf("reading the TPM state in %s (set --swtpm-dir if it lives elsewhere): %w", tpmDir, err)
		}
	}
	return files, nil
}
