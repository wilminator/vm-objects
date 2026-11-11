package manifest

import "testing"

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b         string
		newer, known bool
	}{
		{"0.9.2", "0.8.8", true, true},
		{"0.8.8", "0.9.2", false, true},
		{"0.8.8", "0.8.8", false, true},
		{"10.0", "7.2.1", true, true},
		{"7.2", "7.2.0", false, true},
		{"", "0.8.8", false, false},
		{"0.9.2", "", false, false},
		{"0.9.2-rc1", "0.8.8", false, false},
	} {
		if n, k := Newer(c.a, c.b); n != c.newer || k != c.known {
			t.Errorf("Newer(%q,%q) = %v,%v; want %v,%v", c.a, c.b, n, k, c.newer, c.known)
		}
	}
}

// TPM state moves forward only (nas-ops #7: TrueNAS's libtpms 0.9.2 wrote
// state QNAP's 0.8.8 can't load).
func TestTPMState(t *testing.T) {
	tn := &HostInfo{Platform: "truenas", Libtpms: "0.9.2"}
	qn := &HostInfo{Platform: "qnap", Libtpms: "0.8.8"}
	for _, c := range []struct {
		src, dst *HostInfo
		want     string
	}{
		{tn, qn, "skip"},
		{qn, tn, "load"},
		{tn, tn, "load"},
		{nil, qn, "unknown"},
		{tn, nil, "unknown"},
		{tn, &HostInfo{Platform: "qnap"}, "unknown"},
	} {
		if got := TPMState(c.src, c.dst); got != c.want {
			t.Errorf("%+v -> %+v: %s, want %s", c.src, c.dst, got, c.want)
		}
	}
}
