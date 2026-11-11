package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/platform"
)

// tnHost is the TrueNAS module with a known libtpms version.
type tnHost struct {
	*platform.TrueNAS
	libtpms string
}

func (h tnHost) HostInfo(context.Context) (*manifest.HostInfo, error) {
	return &manifest.HostInfo{Platform: "truenas", Libtpms: h.libtpms}, nil
}

// restoreZFS makes zvols appear as plain files under dir, as udev makes
// device nodes, and remembers what was asked.
type restoreZFS struct {
	dir     string
	created []string
	exists  map[string]bool
}

func (z *restoreZFS) Run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "list":
		name := args[len(args)-1]
		if z.exists[name] {
			return name + "\n", nil
		}
		return "", errors.New("dataset does not exist")
	case "create":
		z.created = append(z.created, strings.Join(args[1:], " "))
		name := args[len(args)-1]
		size, _ := strconv.ParseInt(args[len(args)-2], 10, 64)
		p := filepath.Join(z.dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			return "", err
		}
		z.exists[name] = true
		return "", nil
	}
	return "", fmt.Errorf("unexpected zfs %v", args)
}

// A tiny TrueNAS: remembers the VM it was asked to make.
type tnRestoreManager struct {
	fakeManager
	taken    string
	failOnce bool
	spec     platform.VMSpec
}

func (m *tnRestoreManager) NameFree(_ context.Context, name string) error {
	if name == m.taken {
		return errors.New("a VM with that name exists")
	}
	return nil
}
func (m *tnRestoreManager) Create(_ context.Context, spec platform.VMSpec) (*platform.VM, error) {
	if m.failOnce {
		m.failOnce = false
		return nil, errors.New("middleware: not reachable")
	}
	m.spec = spec
	return &platform.VM{Name: spec.Name, UUID: "99999999-aaaa-bbbb-cccc-dddddddddddd", Domain: "7_" + spec.Name}, nil
}

func trueNASBackup(t *testing.T, libtpms string) (*harness, *BackupVMStats, []byte) {
	h := newHarness(t)
	disk := make([]byte, 1<<20)
	rand.New(rand.NewSource(5)).Read(disk)
	h.write(disk)
	sys := filepath.Join(h.dir, "system", "vm")
	nvram := filepath.Join(sys, "nvram", "2_Games_VARS.fd")
	tpmDir := filepath.Join(sys, "tpm", "2_Games_tpm_state")
	for p, data := range map[string]string{nvram: "vars", filepath.Join(tpmDir, "tpm2-00.permall"): "tpm"} {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(data), 0o600)
	}
	xml := `<domain type='kvm'><name>2_Games</name><uuid>` + testUUID + `</uuid>
<os firmware='efi'><nvram>` + nvram + `</nvram></os>
<devices>
  <disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='` + h.img + `'/><target dev='vda'/></disk>
  <tpm model='tpm-crb'><backend type='emulator' version='2.0'><source type='dir' path='` + tpmDir + `'/></backend></tpm>
</devices></domain>`
	tn := tnHost{platform.NewTrueNAS(tnMidclt{}, &fakeVirsh{state: "shut off", xml: xml}), libtpms}
	st, err := BackupVM(h.ctx, BackupVMOptions{Repo: h.repo, Domain: "Games", Kind: manifest.Full,
		StateDir: filepath.Join(h.dir, "state"), Platform: tn})
	if err != nil {
		t.Fatal(err)
	}
	return h, st, disk
}

func TestRestoreVMCreatesOnTrueNAS(t *testing.T) {
	h, bst, disk := trueNASBackup(t, "0.9.2")
	z := &restoreZFS{dir: filepath.Join(h.dir, "zvol"), exists: map[string]bool{}}
	m := &tnRestoreManager{}
	opts := func(dir string, host *manifest.HostInfo) RestoreVMOptions {
		return RestoreVMOptions{Repo: h.repo, VM: "Games", Record: bst.Record, Dir: dir, Target: "truenas",
			Create: true, Manager: m, Pool: "tank/vms", NewName: "Copy", ZFS: z, ZvolDir: z.dir,
			NVRAMDir: filepath.Join(dir, "..", "nvram-"+filepath.Base(dir)), SwtpmDir: filepath.Join(dir, "..", "tpm-"+filepath.Base(dir)),
			TargetHost: host}
	}

	dir := filepath.Join(h.dir, "r1")
	st, err := RestoreVM(h.ctx, opts(dir, &manifest.HostInfo{Platform: "truenas", Libtpms: "0.9.2"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(z.created) != 1 || !strings.HasPrefix(z.created[0], "-p -s -V "+strconv.Itoa(1<<20)) || !strings.HasSuffix(z.created[0], "tank/vms/Copy-vda") {
		t.Fatalf("zfs create %v", z.created)
	}
	if got, _ := os.ReadFile(st.Disks["vda"]); !bytes.Equal(got, disk) {
		t.Fatal("the zvol differs from the disk")
	}
	if len(m.spec.Disks) != 1 || m.spec.Disks[0].Path != filepath.Join(z.dir, "tank/vms/Copy-vda") || m.spec.DefinitionType != "truenas" ||
		m.spec.Name != "Copy" || m.spec.ReuseUUID {
		t.Fatalf("spec %+v", m.spec)
	}
	if st.Created == nil || st.Created.Domain != "7_Copy" {
		t.Fatalf("created %+v", st.Created)
	}
	// UEFI variables and TPM state are placed under the VM's libvirt name.
	nv, _ := os.ReadFile(filepath.Join(dir, "..", "nvram-r1", "7_Copy_VARS.fd"))
	tp, _ := os.ReadFile(filepath.Join(dir, "..", "tpm-r1", "7_Copy_tpm_state", "tpm2-00.permall"))
	if string(nv) != "vars" || string(tp) != "tpm" {
		t.Fatalf("placed nvram %q, tpm %q", nv, tp)
	}
	notes, _ := os.ReadFile(filepath.Join(dir, "RESTORE.txt"))
	if !strings.HasPrefix(string(notes), "vmobj created this VM in TrueNAS") || !strings.Contains(string(notes), "loads on this host") {
		t.Fatalf("RESTORE.txt:\n%s", notes)
	}
	if _, err := os.Stat(filepath.Join(dir, RestoreVMMarker)); err == nil {
		t.Fatal("the unfinished marker is still there")
	}

	// Another restore onto the same zvol name is refused before writing.
	if _, err := RestoreVM(h.ctx, opts(filepath.Join(h.dir, "r2"), nil)); err == nil || !strings.Contains(err.Error(), "exists already") {
		t.Fatalf("existing zvol: %v", err)
	}
	// A name TrueNAS already has, a bad name and a bad pool are refused too.
	m.taken = "Taken"
	o := opts(filepath.Join(h.dir, "r3"), nil)
	o.NewName = "Taken"
	if _, err := RestoreVM(h.ctx, o); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("taken name: %v", err)
	}
	o = opts(filepath.Join(h.dir, "r4"), nil)
	o.NewName = "bad-name"
	if _, err := RestoreVM(h.ctx, o); err == nil {
		t.Fatal("a name TrueNAS rejects was accepted")
	}
	o = opts(filepath.Join(h.dir, "r5"), nil)
	o.Pool = "tank/../x"
	if _, err := RestoreVM(h.ctx, o); err == nil {
		t.Fatal("a pool with .. was accepted")
	}
	if len(z.created) != 1 {
		t.Fatalf("a refused restore still made zvols: %v", z.created)
	}
}

// TPM state from a newer libtpms isn't placed on a host with an older one.
func TestRestoreVMOnTrueNASSkipsTooNewTPMState(t *testing.T) {
	h, bst, _ := trueNASBackup(t, "0.9.2")
	z := &restoreZFS{dir: filepath.Join(h.dir, "zvol"), exists: map[string]bool{}}
	dir := filepath.Join(h.dir, "r1")
	tpm := filepath.Join(h.dir, "tpm")
	nvram := filepath.Join(h.dir, "nvram")
	_, err := RestoreVM(h.ctx, RestoreVMOptions{Repo: h.repo, VM: "Games", Record: bst.Record, Dir: dir, Target: "truenas",
		Create: true, Manager: &tnRestoreManager{}, Pool: "tank/vms", NewName: "Copy", ZFS: z, ZvolDir: z.dir,
		NVRAMDir: nvram, SwtpmDir: tpm, ReuseUUID: true,
		TargetHost: &manifest.HostInfo{Platform: "truenas", Libtpms: "0.8.8"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tpm, "7_Copy_tpm_state")); err == nil {
		t.Fatal("TPM state from libtpms 0.9.2 was placed for 0.8.8")
	}
	if _, err := os.Stat(filepath.Join(nvram, "7_Copy_VARS.fd")); err != nil {
		t.Fatalf("the UEFI variables were not placed: %v", err)
	}
	notes, _ := os.ReadFile(filepath.Join(dir, "RESTORE.txt"))
	if !strings.Contains(string(notes), "TPM STATE IS NOT USED") || !strings.Contains(string(notes), "0.9.2") {
		t.Fatalf("RESTORE.txt:\n%s", notes)
	}
}

// If the VM can't be created after the disks are written, the same
// command run again reuses the zvols (checking them) and creates the VM.
func TestRestoreVMOnTrueNASResumes(t *testing.T) {
	h, bst, disk := trueNASBackup(t, "0.9.2")
	z := &restoreZFS{dir: filepath.Join(h.dir, "zvol"), exists: map[string]bool{}}
	m := &tnRestoreManager{failOnce: true}
	o := RestoreVMOptions{Repo: h.repo, VM: "Games", Record: bst.Record, Dir: filepath.Join(h.dir, "r1"), Target: "truenas",
		Create: true, Manager: m, Pool: "tank/vms", NewName: "Copy", ZFS: z, ZvolDir: z.dir,
		NVRAMDir: filepath.Join(h.dir, "nv"), SwtpmDir: filepath.Join(h.dir, "tp")}
	if _, err := RestoreVM(h.ctx, o); err == nil || !strings.Contains(err.Error(), "run the same restore-vm again") {
		t.Fatalf("first run: %v", err)
	}
	st, err := RestoreVM(h.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(z.created) != 1 || !st.Resumed || st.Created == nil {
		t.Fatalf("zvols created %v, resumed %v, created %+v", z.created, st.Resumed, st.Created)
	}
	if got, _ := os.ReadFile(st.Disks["vda"]); !bytes.Equal(got, disk) {
		t.Fatal("the zvol differs from the disk")
	}
}
