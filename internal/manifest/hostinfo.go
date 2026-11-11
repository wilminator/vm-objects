// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package manifest

import (
	"strconv"
	"strings"
)

// HostInfo says what ran a VM: the platform and the versions of the parts
// whose saved state or settings only move forward (nas-ops #7: TPM state
// written by a newer libtpms won't load on an older one). A field vmobj
// couldn't read is empty, never guessed. A record made before this existed
// has no HostInfo at all.
type HostInfo struct {
	Platform        string `json:"platform"`                  // libvirt, qnap or truenas
	PlatformVersion string `json:"platformVersion,omitempty"` // TrueNAS 25.10.7, Virtualization Station 4.2.0.281
	Libvirt         string `json:"libvirt,omitempty"`
	QEMU            string `json:"qemu,omitempty"`
	Swtpm           string `json:"swtpm,omitempty"`
	Libtpms         string `json:"libtpms,omitempty"`
}

// Newer reports whether version a is newer than b, comparing their dotted
// numbers (0.9.2 > 0.8.8; 10.0 > 7.2.1). known is false when either is
// empty or isn't made of dotted numbers: the caller then says it couldn't
// tell, and doesn't guess.
func Newer(a, b string) (newer, known bool) {
	pa, ok1 := versionParts(a)
	pb, ok2 := versionParts(b)
	if !ok1 || !ok2 {
		return false, false
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y, true
		}
	}
	return false, true
}

func versionParts(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return nil, false
	}
	var out []int
	for _, s := range strings.Split(v, ".") {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// TPMState says whether saved TPM state from src loads on dst: TPM state
// moves forward only, to the same or a newer libtpms. It answers "load",
// "skip" (the target's libtpms is older: it can't read it) or "unknown".
func TPMState(src, dst *HostInfo) string {
	if src == nil || dst == nil {
		return "unknown"
	}
	newer, known := Newer(src.Libtpms, dst.Libtpms)
	switch {
	case !known:
		return "unknown"
	case newer:
		return "skip"
	}
	return "load"
}
