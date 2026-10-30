// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package units

import "testing"

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"64KiB": 64 << 10, "4MiB": 4 << 20, "1.5GiB": 3 << 29,
		"1048576": 1 << 20, "10MB": 10e6, " 2 KiB ": 2048,
	}
	for in, want := range good {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "-4MiB", "1e400GiB", "NaNMiB", "InfKiB", "99999999999TiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted a bad size", bad)
		}
	}
}

func TestFormat(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 2048: "2.00 KiB", 3 << 30: "3.00 GiB"} {
		if got := Format(n); got != want {
			t.Errorf("Format(%d) = %q, want %q", n, got, want)
		}
	}
}
