package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
)

// A restore compares the backup's libtpms with the target's. TPM state
// moves forward only (nas-ops #7), so from newer to older it isn't used.
func TestRestoreNotesTPMByHostVersion(t *testing.T) {
	tn := &manifest.HostInfo{Platform: "truenas", Libtpms: "0.9.2"}
	qn := &manifest.HostInfo{Platform: "qnap", Libtpms: "0.8.8"}
	mk := func(src *manifest.HostInfo) *manifest.VMRecord {
		return &manifest.VMRecord{Name: "n", DomainName: "WinGames", Kind: "full", Created: time.Now(),
			Consistency: "shut off", Platform: "truenas", Host: src,
			Files: []manifest.VMFile{{Role: "tpm", Name: "tpm2/x"}}}
	}
	notes := func(src, dst *manifest.HostInfo) string {
		st := &RestoreVMStats{Definition: "/r/WinGames.xml", Disks: map[string]string{"vda": "/r/a.img"}}
		return restoreNotesFor("qnap", mk(src), st, "WinGames", true, true, dst)
	}

	skip := notes(tn, qn)
	if !strings.Contains(skip, "TPM STATE IS NOT USED") || !strings.Contains(skip, "0.9.2") || !strings.Contains(skip, "0.8.8") {
		t.Fatalf("newer to older:\n%s", skip)
	}
	if strings.Contains(skip, "put the TPM state where QNAP keeps it") {
		t.Fatalf("asked to copy TPM state that can't load:\n%s", skip)
	}
	if load := notes(qn, tn); !strings.Contains(load, "so it loads") || !strings.Contains(load, "put the TPM state where QNAP keeps it") {
		t.Fatalf("older to newer:\n%s", load)
	}
	// An old backup (no host record) or an unreadable target: the old
	// wording, copy steps and the forward-only warning, nothing claimed.
	for _, unk := range []string{notes(nil, qn), notes(tn, nil)} {
		if strings.Contains(unk, "NOT USED") || strings.Contains(unk, "so it loads") ||
			!strings.Contains(unk, "put the TPM state where QNAP keeps it") || !strings.Contains(unk, "only moves forward") {
			t.Fatalf("unknown versions:\n%s", unk)
		}
	}
}

// A backup records the versions of the host that ran the VM, so a restore
// elsewhere can compare them.
func TestBackupVMRecordsTheHost(t *testing.T) {
	v := newVMHarness(t)
	vda := filepath.Join(v.dir, "vda.img")
	randomDisk(t, vda, 64<<10, 1)
	v.virsh.xml = domainXML(fileDisk("vda", vda), v.nvram)
	v.virsh.version = "Using library: libvirt 10.0.0\nUsing API: QEMU 10.0.0\nRunning hypervisor: QEMU 8.2.2\n"
	st, err := BackupVM(v.ctx, v.opts(manifest.Full))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := v.repo.GetVMRecord(v.ctx, "win11", st.Record)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Host == nil || rec.Host.Platform != "libvirt" || rec.Host.Libvirt != "10.0.0" || rec.Host.QEMU != "8.2.2" || rec.Host.Libtpms != "" {
		t.Fatalf("host %+v: versions it can't read must stay empty", rec.Host)
	}
}
