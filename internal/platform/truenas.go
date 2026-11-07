// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/wilminator/vm-objects/internal/libvirt"
	"github.com/wilminator/vm-objects/internal/manifest"
)

// TrueNAS SCALE (25.10 "Goldeye" and later) runs its own libvirt, reached
// through its own socket, and keeps each VM's real configuration in its
// middleware (midclt), from which it writes libvirt's definition at start.
// Its libvirt names a VM "<id>_<name>" (1_WinGames for WinGames), and the
// definition says where the NVRAM and TPM state live (under
// /var/db/system/vm).
const (
	TrueNASMidclt        = "/usr/bin/midclt"
	TrueNASLibvirtSocket = "/run/truenas_libvirt/libvirt-sock"
	// TrueNASLibvirtURI connects virsh to that libvirt.
	TrueNASLibvirtURI = "qemu+unix:///system?socket=" + TrueNASLibvirtSocket
	// TrueNASConfigFile is the name of the VM's TrueNAS settings inside a
	// backup: its vm.query record, with passwords removed.
	TrueNASConfigFile = "truenas-vm.json"
)

// Midclt runs TrueNAS's middleware client. Tests use a fake.
type Midclt interface {
	Call(ctx context.Context, method string, args ...string) ([]byte, error)
}

// ExecMidclt runs the real midclt program.
type ExecMidclt struct{ Path string }

func (m ExecMidclt) Call(ctx context.Context, method string, args ...string) ([]byte, error) {
	bin := m.Path
	if bin == "" {
		bin = TrueNASMidclt
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, append([]string{"call", method}, args...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("midclt call %s: %w: %s", method, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// TrueNAS describes TrueNAS VMs: the disks, NVRAM and TPM state through
// TrueNAS's libvirt, plus TrueNAS's own settings for the VM (CPU, memory,
// firmware, SMBIOS arguments, devices), kept with the backup so the VM can
// be recreated on TrueNAS as it was.
type TrueNAS struct {
	Midclt Midclt
	Virsh  libvirt.Runner // connected to TrueNAS's libvirt socket
}

// NewTrueNAS returns the TrueNAS module. A nil midclt runs
// /usr/bin/midclt; a nil virsh connects through TrueNAS's socket.
func NewTrueNAS(m Midclt, virsh libvirt.Runner) *TrueNAS {
	if m == nil {
		m = ExecMidclt{}
	}
	if virsh == nil {
		virsh = libvirt.Virsh{Connect: TrueNASLibvirtURI}
	}
	return &TrueNAS{Midclt: m, Virsh: virsh}
}

func (t *TrueNAS) Name() string { return "truenas" }

// trueNASVM is the part of a vm.query record vmobj reads itself.
type trueNASVM struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// domain is the VM's name in TrueNAS's libvirt.
func (v *trueNASVM) domain() string { return fmt.Sprintf("%d_%s", v.ID, v.Name) }

var libvirtName = regexp.MustCompile(`^(\d+)_(.+)$`)

// lookup finds a VM by its TrueNAS name, its numeric ID, or its libvirt
// name (1_WinGames), and returns the raw vm.query record and its fields.
func (t *TrueNAS) lookup(ctx context.Context, vm string) (json.RawMessage, *trueNASVM, error) {
	query := func(field string, value any) (json.RawMessage, *trueNASVM, error) {
		filter, _ := json.Marshal([][]any{{field, "=", value}})
		out, err := t.Midclt.Call(ctx, "vm.query", string(filter))
		if err != nil {
			return nil, nil, err
		}
		var raws []json.RawMessage
		if err := json.Unmarshal(out, &raws); err != nil {
			return nil, nil, fmt.Errorf("reading vm.query's answer: %w", err)
		}
		if len(raws) != 1 {
			return nil, nil, nil
		}
		var v trueNASVM
		if err := json.Unmarshal(raws[0], &v); err != nil {
			return nil, nil, fmt.Errorf("reading vm.query's answer: %w", err)
		}
		return raws[0], &v, nil
	}
	if raw, v, err := query("name", vm); err != nil || v != nil {
		return raw, v, err
	}
	if id, err := strconv.Atoi(vm); err == nil {
		if raw, v, err := query("id", id); err != nil || v != nil {
			return raw, v, err
		}
	}
	if m := libvirtName.FindStringSubmatch(vm); m != nil {
		id, _ := strconv.Atoi(m[1])
		raw, v, err := query("id", id)
		if err != nil {
			return nil, nil, err
		}
		if v != nil && v.Name == m[2] {
			return raw, v, nil
		}
	}
	return nil, nil, fmt.Errorf("TrueNAS has no VM %q (give its name as the TrueNAS UI shows it, or its ID)", vm)
}

// Describe reads the VM through TrueNAS's libvirt and adds TrueNAS's
// settings for it as a file of the backup. The VM is named as TrueNAS
// names it (WinGames, not 1_WinGames).
func (t *TrueNAS) Describe(ctx context.Context, vm string) (*VM, error) {
	raw, v, err := t.lookup(ctx, vm)
	if err != nil {
		return nil, err
	}
	lv := &Libvirt{Virsh: t.Virsh, SwtpmDir: DefaultSwtpmDir, name: "truenas"}
	desc, err := lv.Describe(ctx, v.domain())
	if err != nil {
		return nil, err
	}
	// TrueNAS sets the display password from its own settings when the VM
	// starts, so the copy of libvirt's definition kept here goes without it.
	desc.Definition = xmlPasswords.ReplaceAll(desc.Definition, nil)
	config, err := trueNASConfig(raw)
	if err != nil {
		return nil, err
	}
	desc.Files = append(desc.Files, manifest.VMFile{Role: "config", Name: TrueNASConfigFile, Mode: 0o600, Data: config})
	desc.Name = v.Name
	return desc, nil
}

func (t *TrueNAS) Freeze(ctx context.Context, vm string) error {
	_, v, err := t.lookup(ctx, vm)
	if err != nil {
		return err
	}
	return libvirt.Freeze(ctx, t.Virsh, v.domain())
}

func (t *TrueNAS) Thaw(ctx context.Context, vm string) error {
	_, v, err := t.lookup(ctx, vm)
	if err != nil {
		return err
	}
	return libvirt.Thaw(ctx, t.Virsh, v.domain())
}

// trueNASConfig turns a vm.query record into the settings file kept with
// the backup: every key whose name mentions a password is removed (display
// devices carry one), and so are the fields that describe the moment
// rather than the VM (its running state).
func trueNASConfig(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok {
		delete(m, "status")
		delete(m, "display_available")
	}
	return json.MarshalIndent(withoutPasswords(v), "", "  ")
}

var (
	passwordKey  = regexp.MustCompile(`(?i)passw`)
	xmlPasswords = regexp.MustCompile(`\s+passwd=(?:'[^']*'|"[^"]*")`)
)

func withoutPasswords(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if passwordKey.MatchString(k) {
				delete(x, k)
				continue
			}
			x[k] = withoutPasswords(val)
		}
	case []any:
		for i := range x {
			x[i] = withoutPasswords(x[i])
		}
	}
	return v
}
