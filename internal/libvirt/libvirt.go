// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package libvirt asks libvirt about a VM through the virsh command. vmobj
// only reads definitions and state, and (when asked to quiesce) freezes and
// thaws the guest's filesystems through the QEMU guest agent. It never
// defines, starts, stops or changes a VM.
package libvirt

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs virsh. The real one runs the program; tests use a fake.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Virsh runs the virsh program.
type Virsh struct {
	Path    string // default "virsh"; on QNAP, /QVS/usr/bin/virsh
	Connect string // libvirt URI, e.g. qemu:///system; empty for virsh's default
}

// Run runs virsh with args and returns its standard output.
func (v Virsh) Run(ctx context.Context, args ...string) (string, error) {
	bin := v.Path
	if bin == "" {
		bin = "virsh"
	}
	if v.Connect != "" {
		args = append([]string{"--connect", v.Connect}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("virsh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Definition returns the domain's persistent definition, including
// security details (such as a VNC password) so a restore is complete; the
// backup is encrypted.
func Definition(ctx context.Context, r Runner, domain string) ([]byte, error) {
	out, err := r.Run(ctx, "dumpxml", "--inactive", "--security-info", "--", domain)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// State is a domain's run state.
type State string

const (
	ShutOff State = "shut off"
	Running State = "running"
	Paused  State = "paused"
)

// DomainState asks whether the domain is running.
func DomainState(ctx context.Context, r Runner, domain string) (State, error) {
	out, err := r.Run(ctx, "domstate", "--", domain)
	if err != nil {
		return "", err
	}
	return State(strings.TrimSpace(out)), nil
}

// Freeze asks the guest agent to flush and freeze the guest's filesystems.
// Always pair it with Thaw.
func Freeze(ctx context.Context, r Runner, domain string) error {
	_, err := r.Run(ctx, "domfsfreeze", "--", domain)
	return err
}

// Thaw resumes the guest's filesystems.
func Thaw(ctx context.Context, r Runner, domain string) error {
	_, err := r.Run(ctx, "domfsthaw", "--", domain)
	return err
}

// CheckDomain refuses a domain name or UUID that virsh could read as
// something else: one starting with "-" (an option) or holding control
// characters. vmobj passes names as separate arguments, never through a
// shell, so this is the only way one could be misread.
func CheckDomain(name string) error {
	if name == "" || name[0] == '-' {
		return fmt.Errorf("domain %q: a name must not be empty or start with '-'", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("domain %q holds a control character", name)
		}
	}
	return nil
}
