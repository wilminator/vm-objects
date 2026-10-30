// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package policy

import (
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
)

func TestParseClass(t *testing.T) {
	for in, want := range map[string]Class{
		"DEEP_ARCHIVE": DeepArchive, "deep-archive": DeepArchive, "deep": DeepArchive,
		"standard-ia": StandardIA, "IA": StandardIA, "glacier-ir": GlacierIR, "instant": GlacierIR,
		"flexible": Glacier, "GLACIER": Glacier, "one-zone-ia": OneZoneIA, "Standard": Standard,
		"intelligent-tiering": IntelligentTiering,
	} {
		if got, err := ParseClass(in); err != nil || got != want {
			t.Errorf("ParseClass(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseClass("REDUCED_REDUNDANCY"); err == nil {
		t.Error("accepted a class vmobj does not plan for")
	}
}

func TestSlowest(t *testing.T) {
	d, c := Slowest([]Class{StandardIA, Glacier, DeepArchive}, Bulk)
	if d != 48*time.Hour || c != DeepArchive {
		t.Fatalf("Slowest bulk = %v %s", d, c)
	}
	// Deep Archive has no Expedited tier; it falls back to Standard (12h),
	// which is still slower than Flexible's Expedited (minutes).
	d, c = Slowest([]Class{Glacier, DeepArchive}, Expedited)
	if d != 12*time.Hour || c != DeepArchive {
		t.Fatalf("Slowest expedited = %v %s", d, c)
	}
	if d, _ := Slowest([]Class{Standard, StandardIA, GlacierIR}, Bulk); d != 0 {
		t.Fatalf("instant classes wait %v", d)
	}
}

func TestValidateWarnsAboutMinimumDays(t *testing.T) {
	p, _ := Lookup("disaster-recovery")
	r := p.Rules[manifest.Full]
	r.Retain = 30 * Day // Deep Archive bills 180 days regardless
	p.Rules[manifest.Full] = r
	warn, err := p.Validate()
	if err != nil || len(warn) != 1 {
		t.Fatalf("Validate = %v, %v; want one warning", warn, err)
	}
	// Lookup hands out copies: the change above must not leak.
	again, _ := Lookup("disaster-recovery")
	if again.Rules[manifest.Full].Retain != 180*Day {
		t.Fatal("changing a looked-up profile changed the built-in")
	}
	delete(p.Rules, manifest.Incremental)
	if _, err := p.Validate(); err == nil {
		t.Fatal("a profile without an incremental rule validated")
	}
}
