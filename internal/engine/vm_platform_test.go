// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
	"github.com/wilminator/vm-objects/internal/vmdef"
)

// tnMidclt answers vm.query with one VM, whatever the filter.
type tnMidclt struct{}

func (tnMidclt) Call(context.Context, string, ...string) ([]byte, error) {
	return []byte(`[{"id": 2, "name": "Games", "command_line_args": "-smbios \"type=1,serial=EXAMPLE\"",
	  "devices": [{"attributes": {"dtype": "DISPLAY", "password": "secret"}}], "status": {"state": "STOPPED"}}]`), nil
}

// A backup made by the TrueNAS module (NVRAM and TPM state in TrueNAS's
// folders, TPM files kept flat, TrueNAS's settings as a file) restores
// with every piece in place, for each kind of target.
func TestTrueNASBackupRestoresForEachTarget(t *testing.T) {
	h := newHarness(t)
	disk := bytes.Repeat([]byte("games!"), 50000)
	h.write(disk)
	sys := filepath.Join(h.dir, "system", "vm")
	nvram := filepath.Join(sys, "nvram", "2_Games_VARS.fd")
	tpmDir := filepath.Join(sys, "tpm", "2_Games_tpm_state")
	for p, data := range map[string]string{nvram: "vars", filepath.Join(tpmDir, "tpm2-00.permall"): "tpm"} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	xml := `<domain type='kvm'><name>2_Games</name><uuid>` + testUUID + `</uuid>
<os firmware='efi'><nvram>` + nvram + `</nvram></os>
<devices>
  <disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='` + h.img + `'/><target dev='vda'/></disk>
  <tpm model='tpm-crb'><backend type='emulator' version='2.0'><source type='dir' path='` + tpmDir + `'/></backend></tpm>
  <graphics type='spice' port='5900' passwd='secret'/>
</devices></domain>`
	tn := platform.NewTrueNAS(tnMidclt{}, &fakeVirsh{state: "shut off", xml: xml})
	st, err := BackupVM(h.ctx, BackupVMOptions{Repo: h.repo, Domain: "Games", Kind: manifest.Full,
		StateDir: filepath.Join(h.dir, "state"), Platform: tn})
	if err != nil {
		t.Fatal(err)
	}
	if st.NVRAMFiles != 1 || st.TPMFiles != 1 {
		t.Fatalf("backed up %d NVRAM and %d TPM files", st.NVRAMFiles, st.TPMFiles)
	}
	rec, err := h.repo.GetVMRecord(h.ctx, "Games", st.Record)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Platform != "truenas" || rec.DomainName != "Games" || bytes.Contains(rec.Definition, []byte("secret")) {
		t.Fatalf("record: platform %q, domain %q, password kept: %v", rec.Platform, rec.DomainName, bytes.Contains(rec.Definition, []byte("secret")))
	}

	for _, target := range []string{"libvirt", "qnap", "truenas"} {
		t.Run(target, func(t *testing.T) {
			dir := filepath.Join(h.dir, "restore-"+target)
			rs, err := RestoreVM(h.ctx, RestoreVMOptions{Repo: h.repo, VM: "Games", Record: st.Record, Dir: dir, Target: target})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(rs.Disks["vda"]); !bytes.Equal(got, disk) {
				t.Fatal("disk differs")
			}
			for p, want := range map[string]string{
				"2_Games_VARS.fd":          "vars",
				"tpm/tpm2-00.permall":      "tpm",
				platform.TrueNASConfigFile: "",
			} {
				got, err := os.ReadFile(filepath.Join(dir, p))
				if err != nil || (want != "" && string(got) != want) {
					t.Fatalf("%s: %q, %v", p, got, err)
				}
			}
			if cfg, _ := os.ReadFile(filepath.Join(dir, platform.TrueNASConfigFile)); bytes.Contains(cfg, []byte("secret")) || !bytes.Contains(cfg, []byte("EXAMPLE")) {
				t.Fatalf("settings file: %s", cfg)
			}
			// The definition's named TPM folder now names the restored one.
			def, _ := os.ReadFile(rs.Definition)
			dom, err := vmdef.Parse(def)
			if err != nil || dom.TPMPath != filepath.Join(dir, "tpm") {
				t.Fatalf("restored definition's TPM folder = %q (%v)", dom.TPMPath, err)
			}
			notes, _ := os.ReadFile(filepath.Join(dir, "RESTORE.txt"))
			n := string(notes)
			if !strings.Contains(n, "made on truenas") {
				t.Fatalf("notes don't say where the backup came from:\n%s", n)
			}
			switch target {
			case "qnap":
				// QNAP's layout wants the flat TrueNAS files in tpm2/.
				if !strings.Contains(n, platform.QNAPSwtpmDir+"/<UUID>/tpm2") || !strings.Contains(n, "activation") {
					t.Fatalf("qnap notes:\n%s", n)
				}
			case "truenas":
				if !strings.Contains(n, "_tpm_state") || !strings.Contains(n, "--mode in-place") || !strings.Contains(n, "command_line_args") {
					t.Fatalf("truenas notes:\n%s", n)
				}
			case "libvirt":
				if !strings.Contains(n, "nothing to copy") && !strings.Contains(n, "as restored") {
					t.Fatalf("libvirt notes:\n%s", n)
				}
			}
		})
	}
}
