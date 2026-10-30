// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package policy

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/manifest"
)

// Day is 24 hours. Retention is counted in whole days, as S3 does.
const Day = 24 * time.Hour

// Rule says how one kind of backup is stored.
type Rule struct {
	Class  Class
	Retain time.Duration // how long a backup of this kind must stay restorable
	Every  time.Duration // how often this kind is expected to run (for planning)
}

// Profile assigns a rule to each kind of backup.
//
// Data (blocks and packs) goes to each rule's class. Manifests, pack
// tables of contents and the repository config always go to Standard:
// they are small, and listing, verifying or planning a restore must be
// able to read them at once, even when the data itself is archived.
type Profile struct {
	Name        string
	Description string
	Rules       map[manifest.Kind]Rule
	// RefreshAge is how old a stored base block may get before a full
	// backup uploads it again instead of reusing it, so no full depends on
	// very old objects forever. Zero means never refresh by age.
	RefreshAge time.Duration
	// Margin is extra lock time beyond retention, to cover clock skew and
	// a backup that runs late.
	Margin time.Duration
}

// MetadataClass is where manifests and indexes are kept.
const MetadataClass = Standard

// Rule returns the rule for a kind of backup.
func (p Profile) Rule(k manifest.Kind) Rule { return p.Rules[k] }

// Validate checks a profile is usable and warns about rules that would
// cost money for nothing: retention shorter than a class's minimum storage
// period is billed for the minimum anyway.
func (p Profile) Validate() (warnings []string, err error) {
	for _, k := range []manifest.Kind{manifest.Full, manifest.Incremental, manifest.Differential} {
		r, ok := p.Rules[k]
		if !ok {
			return nil, fmt.Errorf("profile %s: no rule for %s backups", p.Name, k)
		}
		ci, ok := Info(r.Class)
		if !ok {
			return nil, fmt.Errorf("profile %s: unknown class %q", p.Name, r.Class)
		}
		if r.Retain < Day {
			return nil, fmt.Errorf("profile %s: %s retention must be at least one day", p.Name, k)
		}
		if min := time.Duration(ci.MinDays) * Day; r.Retain < min {
			warnings = append(warnings, fmt.Sprintf("%s backups are kept %d days in %s, which bills a minimum of %d days",
				k, int(r.Retain/Day), ci.Name, ci.MinDays))
		}
	}
	if p.Margin < 0 || p.RefreshAge < 0 {
		return nil, fmt.Errorf("profile %s: negative durations", p.Name)
	}
	return warnings, nil
}

// LookAhead is how far beyond its own retention a reused object's lock
// must reach: one full-backup interval, so the next full (which will
// re-check every block) comes before any lock runs short.
func (p Profile) LookAhead() time.Duration { return p.Rules[manifest.Full].Every }

// builtins are the profiles vmobj ships with.
var builtins = map[string]Profile{
	"disaster-recovery": {
		Name: "disaster-recovery",
		Description: "Cheapest long-term protection. Monthly fulls and weekly differentials in Deep Archive, " +
			"daily incrementals in Standard-IA. Restoring from the archive takes 12 to 48 hours.",
		Rules: map[manifest.Kind]Rule{
			manifest.Full:         {Class: DeepArchive, Retain: 180 * Day, Every: 30 * Day},
			manifest.Differential: {Class: DeepArchive, Retain: 180 * Day, Every: 7 * Day},
			manifest.Incremental:  {Class: StandardIA, Retain: 30 * Day, Every: Day},
		},
		RefreshAge: 180 * Day, // six full intervals
		Margin:     Day,
	},
	"fast-restore": {
		Name: "fast-restore",
		Description: "Restores start at once. Fulls and differentials in Glacier Instant Retrieval, " +
			"incrementals in Standard-IA.",
		Rules: map[manifest.Kind]Rule{
			manifest.Full:         {Class: GlacierIR, Retain: 90 * Day, Every: 30 * Day},
			manifest.Differential: {Class: GlacierIR, Retain: 90 * Day, Every: 7 * Day},
			manifest.Incremental:  {Class: StandardIA, Retain: 30 * Day, Every: Day},
		},
		RefreshAge: 180 * Day,
		Margin:     Day,
	},
	"standard": {
		Name:        "standard",
		Description: "Everything in S3 Standard: no minimum storage periods, highest monthly price. Good for testing.",
		Rules: map[manifest.Kind]Rule{
			manifest.Full:         {Class: Standard, Retain: 90 * Day, Every: 30 * Day},
			manifest.Differential: {Class: Standard, Retain: 30 * Day, Every: 7 * Day},
			manifest.Incremental:  {Class: Standard, Retain: 14 * Day, Every: Day},
		},
		RefreshAge: 180 * Day,
		Margin:     Day,
	},
}

// DefaultProfile is used when none is named.
const DefaultProfile = "disaster-recovery"

// Lookup returns a built-in profile by name.
func Lookup(name string) (Profile, error) {
	p, ok := builtins[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown profile %q (have %s)", name, strings.Join(Names(), ", "))
	}
	// Go note: maps are references, so copy the rules before returning,
	// or a caller's override would change the built-in for everyone.
	rules := make(map[manifest.Kind]Rule, len(p.Rules))
	for k, r := range p.Rules {
		rules[k] = r
	}
	p.Rules = rules
	return p, nil
}

// Names lists the built-in profiles.
func Names() []string {
	var n []string
	for k := range builtins {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}
