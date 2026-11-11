// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/source"
)

// restore-vm --platform truenas --create --pool POOL/PARENT: each disk is
// restored onto a new zvol, the VM is made through TrueNAS's middleware
// from the backup's own settings (truenas-vm.json), and the UEFI variables
// and TPM state are put where TrueNAS looks for them. The VM is never
// started.

var datasetRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,127}(/[A-Za-z0-9_][A-Za-z0-9_.:-]{0,127}){0,6}$`)

func (o RestoreVMOptions) zfs() source.ZFSRunner {
	if o.ZFS == nil {
		return source.ExecZFS{}
	}
	return o.ZFS
}

func (o RestoreVMOptions) zvolDir() string {
	if o.ZvolDir == "" {
		return "/dev/zvol"
	}
	return o.ZvolDir
}

// zvolSizeStep rounds a new zvol's size up: ZFS wants a multiple of the
// volume block size, and 1 MiB is a multiple of every size it allows.
const zvolSizeStep = 1 << 20

// checkTrueNASRestore refuses what can be known before anything is
// written, and returns the zvol each disk will get (a dataset name).
func checkTrueNASRestore(ctx context.Context, o RestoreVMOptions, rec *manifest.VMRecord, name string, resuming bool) (map[string]string, error) {
	hasConfig := false
	for _, f := range rec.Files {
		hasConfig = hasConfig || f.Role == "config" && f.Name == platform.TrueNASConfigFile
	}
	if !hasConfig {
		return nil, errors.New("this backup has no TrueNAS settings (truenas-vm.json), so it can't be restored onto TrueNAS yet; only backups made on TrueNAS can")
	}
	if !datasetRE.MatchString(o.Pool) || strings.Contains(o.Pool, "..") {
		return nil, fmt.Errorf("restore-vm --pool %q: give the dataset the zvols go under, such as fast/vms", o.Pool)
	}
	if err := platform.CheckTrueNASName(name); err != nil {
		return nil, err
	}
	if nf, ok := o.Manager.(interface {
		NameFree(context.Context, string) error
	}); ok {
		if err := nf.NameFree(ctx, name); err != nil && !resuming {
			return nil, err
		}
	}
	zvols := map[string]string{}
	for _, d := range rec.Disks {
		ds := o.Pool + "/" + name + "-" + d.Target
		if !datasetRE.MatchString(ds) {
			return nil, fmt.Errorf("the disk %s can't be a zvol name (%q)", d.Target, ds)
		}
		_, err := o.zfs().Run(ctx, "list", "-H", "-o", "name", ds)
		switch {
		case err == nil && !resuming:
			return nil, fmt.Errorf("the zvol %s exists already; pick another --new-name or --pool, or remove it", ds)
		case err != nil && resuming:
			// An earlier run stopped before this disk; it is made now.
		}
		zvols[d.Target] = ds
	}
	return zvols, nil
}

// restoreTrueNASDisk makes the disk's zvol (or reuses the one an earlier,
// interrupted run made), waits for its device, and restores onto it.
func restoreTrueNASDisk(ctx context.Context, o RestoreVMOptions, d manifest.VMDisk, dataset string, resuming bool, st *RestoreVMStats) (string, error) {
	dev := path.Join(o.zvolDir(), dataset)
	_, lerr := o.zfs().Run(ctx, "list", "-H", "-o", "name", dataset)
	existed := lerr == nil
	if !existed {
		m, err := o.Repo.GetManifest(ctx, d.Stream, d.Manifest)
		if err != nil {
			return "", err
		}
		size := (m.DiskSize + zvolSizeStep - 1) / zvolSizeStep * zvolSizeStep
		// Sparse (-s): the zvol takes space as it is written, as the
		// original did not reserve its size either.
		if _, err := o.zfs().Run(ctx, "create", "-p", "-s", "-V", fmt.Sprintf("%d", size), dataset); err != nil {
			return "", fmt.Errorf("creating the zvol %s: %w", dataset, err)
		}
	}
	if err := waitForDevice(ctx, dev, 30*time.Second); err != nil {
		return "", err
	}
	ro := RestoreOptions{Repo: o.Repo, VM: d.Stream, Manifest: d.Manifest, Output: dev}
	if fi, err := os.Stat(dev); err != nil || fi.Mode()&fs.ModeDevice == 0 || existed {
		// An earlier run's zvol (or, in tests, a plain file): bring it to
		// the backup in place and read it back.
		ro.Mode, ro.Check = ModeInPlace, true
		if existed {
			st.Resumed = true
		}
	}
	if o.Progress != nil {
		target := d.Target
		ro.Progress = func(p Phase, done, total int64) { o.Progress(target, p, done, total) }
	}
	if _, err := Restore(ctx, ro); err != nil {
		return "", err
	}
	return dev, nil
}

func waitForDevice(ctx context.Context, p string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the zvol device %s didn't appear within %s", p, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// createTrueNASVM defines the VM on the restored zvols from the backup's
// TrueNAS settings, then places the UEFI variables and TPM state.
func createTrueNASVM(ctx context.Context, o RestoreVMOptions, rec *manifest.VMRecord, name string, st *RestoreVMStats) error {
	var cfg []byte
	for _, f := range rec.Files {
		if f.Role == "config" && f.Name == platform.TrueNASConfigFile {
			cfg = f.Data
		}
	}
	spec := platform.VMSpec{Name: name, Networks: o.Bridges, ReuseUUID: o.ReuseUUID, Definition: cfg, DefinitionType: "truenas"}
	for _, d := range rec.Disks {
		spec.Disks = append(spec.Disks, platform.DiskSpec{Target: d.Target, Path: st.Disks[d.Target], Format: "raw"})
	}
	vm, err := o.Manager.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating the VM: %w (the zvols are restored; run the same restore-vm again to retry)", err)
	}
	st.Created = vm
	return nil
}

// placeTrueNASState puts the UEFI variables and TPM state where TrueNAS's
// libvirt looks for them, once the VM exists: it names them after the
// VM's libvirt name (<id>_<name>). A failure is reported in the notes, not
// fatal: the files are also in the restore folder.
func placeTrueNASState(o RestoreVMOptions, rec *manifest.VMRecord, vm *platform.VM, dir string) (placed, problems []string) {
	if vm == nil || vm.Domain == "" {
		return nil, []string{"the VM's libvirt name is unknown, so the UEFI variables and TPM state were not placed"}
	}
	nvramDir, swtpmDir := o.NVRAMDir, o.SwtpmDir
	if nvramDir == "" {
		nvramDir = platform.TrueNASLibvirt.NVRAMDir
	}
	if swtpmDir == "" {
		swtpmDir = platform.TrueNASLibvirt.SwtpmDir
	}
	tpmUse := manifest.TPMState(rec.Host, o.TargetHost) != "skip"
	for _, f := range rec.Files {
		var from, to string
		switch f.Role {
		case "nvram":
			from, to = filepath.Join(dir, f.Name), filepath.Join(nvramDir, vm.Domain+"_VARS.fd")
		case "tpm":
			if !tpmUse {
				continue
			}
			from = filepath.Join(dir, "tpm", filepath.FromSlash(f.Name))
			to = filepath.Join(swtpmDir, vm.Domain+"_tpm_state", filepath.Base(f.Name))
		default:
			continue
		}
		if err := copyFile(from, to); err != nil {
			problems = append(problems, fmt.Sprintf("could not place %s: %v", to, err))
			continue
		}
		placed = append(placed, to)
	}
	return placed, problems
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o600)
}

// trueNASCreatedNotes writes RESTORE.txt for a VM vmobj created on TrueNAS.
func trueNASCreatedNotes(o RestoreVMOptions, rec *manifest.VMRecord, st *RestoreVMStats, dir string, tpm bool) string {
	var b strings.Builder
	vm := st.Created
	fmt.Fprintf(&b, "vmobj created this VM in TrueNAS as %q (UUID %s), on the restored zvols, and\ndid not start it.\n\n", vm.Name, vm.UUID)
	from := rec.Platform
	if from == "" {
		from = "libvirt"
	}
	fmt.Fprintf(&b, "Restored %s from backup %s (%s, taken %s, %s, made on %s).\n\nZvols:\n", rec.DomainName, rec.Name, rec.Kind,
		rec.Created.Local().Format("2006-01-02 15:04"), rec.Consistency, from)
	var disks []string
	for t := range st.Disks {
		disks = append(disks, t)
	}
	sort.Strings(disks)
	for _, t := range disks {
		fmt.Fprintf(&b, "  %s  %s\n", t, st.Disks[t])
	}
	if len(vm.Skipped) > 0 {
		b.WriteString("\nDevices that were not carried over:\n")
		for _, s := range vm.Skipped {
			fmt.Fprintf(&b, "  %s\n", s)
		}
	}
	placed, problems := placeTrueNASState(o, rec, vm, dir)
	if len(placed) > 0 {
		b.WriteString("\nPlaced for TrueNAS:\n")
		for _, p := range placed {
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}
	for _, p := range problems {
		fmt.Fprintf(&b, "\nNOT DONE: %s. The files are in %s; copy them over by hand while the VM is stopped.\n", p, dir)
	}
	if tpm {
		switch manifest.TPMState(rec.Host, o.TargetHost) {
		case "skip":
			fmt.Fprintf(&b, "\nTPM STATE IS NOT USED: it was written by libtpms %s, and this host's %s is older and can't\n", rec.Host.Libtpms, o.TargetHost.Libtpms)
			b.WriteString("read it. The VM gets a fresh TPM at its first start: Windows asks for the BitLocker\nrecovery key, and Windows Hello PINs must be set up again.\n")
		case "load":
			b.WriteString("\nThe TPM state loads on this host (its libtpms is the same or newer).\n")
		default:
			b.WriteString("\nThe TPM state was placed without a version check (the libtpms versions of this\nbackup and this host are not both known). If the VM won't start and its log shows\n\"TPM result for CMD_INIT: 0x101\", the host's libtpms is older than the one that wrote\nthe state: move the VM's TPM folder aside and start again; it gets a fresh TPM\n(BitLocker recovery key needed).\n")
		}
	}
	b.WriteString("\nThe NIC (if any) is attached as chosen with --bridge, or as the original had it,\nand has a new MAC address. Check it before starting the VM: two VMs with one IP\nor MAC on a network clash.\n")
	return b.String()
}
