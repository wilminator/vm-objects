package platform

import "testing"

func TestParseVirshVersion(t *testing.T) {
	out := "Compiled against library: libvirt 10.0.0\nUsing library: libvirt 10.0.0\nUsing API: QEMU 10.0.0\nRunning hypervisor: QEMU 8.2.2\n"
	if l, q := ParseVirshVersion(out); l != "10.0.0" || q != "8.2.2" {
		t.Fatalf("%q %q", l, q)
	}
	if l, q := ParseVirshVersion("error: failed to connect\n"); l != "" || q != "" {
		t.Fatalf("%q %q", l, q)
	}
}
