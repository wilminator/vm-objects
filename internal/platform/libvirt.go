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
	// QNAPQemuImg needs LD_LIBRARY_PATH=QNAPLibDir to find its libraries.
	QNAPQemuImg = "/QVS/usr/bin/qemu-img"
	QNAPLibDir  = "/QVS/usr/lib"
)

// LibvirtConfig says where one libvirt installation keeps its pieces. The
// generic libvirt module takes it as given (with --virsh, --connect,
// --libvirt-socket and --swtpm-dir); each appliance module supplies its
// own (QNAPLibvirt, TrueNASLibvirt), and those flags override them.
type LibvirtConfig struct {
	Virsh  string // the virsh program
	Socket string // libvirt's socket; "" for virsh's default connection
	URI    string // a full libvirt URI; overrides Socket
	// SwtpmDir holds emulated TPM state, one folder per VM UUID, for VMs
	// whose definition doesn't name a folder of its own.
	SwtpmDir string
	// TPMLayout is how TPM state sits in its folder: "tpm2" (libvirt's:
	// the files in a tpm2/ subfolder) or "flat" (TrueNAS's).
	TPMLayout string
	// NVRAMDir is where the platform keeps NVRAM stores; restore notes
	// name it.
	NVRAMDir string
}

// The libvirt settings of each platform vmobj knows.
var (
	DefaultLibvirt = LibvirtConfig{Virsh: "virsh", SwtpmDir: DefaultSwtpmDir, TPMLayout: "tpm2", NVRAMDir: "/var/lib/libvirt/qemu/nvram"}
	QNAPLibvirt    = LibvirtConfig{Virsh: QNAPVirsh, SwtpmDir: QNAPSwtpmDir, TPMLayout: "tpm2"} // NVRAM: not yet seen on a QNAP; each definition names its store
	TrueNASLibvirt = LibvirtConfig{Virsh: "virsh", Socket: TrueNASLibvirtSocket, SwtpmDir: "/var/db/system/vm/tpm", TPMLayout: "flat", NVRAMDir: "/var/db/system/vm/nvram"}
)

// Appliances maps each platform name to its libvirt settings.
var Appliances = map[string]LibvirtConfig{"libvirt": DefaultLibvirt, "qnap": QNAPLibvirt, "truenas": TrueNASLibvirt}

// ConnectURI is the libvirt URI virsh is given: URI, else one built from
// Socket, else "" (virsh's default).
func (c LibvirtConfig) ConnectURI() string {
	switch {
	case c.URI != "":
		return c.URI
	case c.Socket != "":
		return "qemu+unix:///system?socket=" + c.Socket
	}
	return ""
}

// With returns c with every setting that over gives in place of c's.
func (c LibvirtConfig) With(over LibvirtConfig) LibvirtConfig {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&c.Virsh, over.Virsh)
	set(&c.Socket, over.Socket)
	set(&c.URI, over.URI)
	set(&c.SwtpmDir, over.SwtpmDir)
	set(&c.TPMLayout, over.TPMLayout)
	set(&c.NVRAMDir, over.NVRAMDir)
	return c
}

// Runner is virsh as these settings call it.
func (c LibvirtConfig) Runner() libvirt.Virsh {
	return libvirt.Virsh{Path: c.Virsh, Connect: c.ConnectURI()}
}

// New returns the module for a platform name (see Names), with its own
// libvirt settings overridden by the ones over gives.
func New(name string, over LibvirtConfig) (Platform, error) {
	name, err := Check(name)
	if err != nil {
		return nil, err
	}
	cfg := Appliances[name].With(over)
	if name == "truenas" {
		t := NewTrueNAS(nil, cfg.Runner())
		t.Config = cfg
		return t, nil
	}
	return &Libvirt{Virsh: cfg.Runner(), SwtpmDir: cfg.SwtpmDir, Config: cfg, name: name}, nil
}

// Libvirt describes VMs through virsh. It is the generic libvirt module,
// and, with QNAP's settings, the QNAP one.
type Libvirt struct {
	Virsh    libvirt.Runner
	SwtpmDir string // default DefaultSwtpmDir
	Config   LibvirtConfig
	name     string
}

// NewLibvirt returns the generic libvirt module.
func NewLibvirt(virsh libvirt.Runner, swtpmDir string) *Libvirt {
	if swtpmDir == "" {
		swtpmDir = DefaultSwtpmDir
	}
	cfg := DefaultLibvirt.With(LibvirtConfig{SwtpmDir: swtpmDir})
	return &Libvirt{Virsh: virsh, SwtpmDir: swtpmDir, Config: cfg, name: "libvirt"}
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
	cfg := QNAPLibvirt.With(LibvirtConfig{SwtpmDir: swtpmDir})
	return &Libvirt{Virsh: virsh, SwtpmDir: swtpmDir, Config: cfg, name: "qnap"}
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
		// A folder named in the definition wins over libvirt's default.
		tpmDir = dom.TPMPath
		if tpmDir == "" {
			tpmDir = filepath.Join(l.SwtpmDir, dom.UUID)
		}
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
