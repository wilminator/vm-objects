// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package policy describes where and for how long backups are kept: the S3
// storage classes, the profiles that assign a class and a retention period
// to each kind of backup, and the Object Lock timing rules.
package policy

import (
	"fmt"
	"strings"
	"time"
)

// Class is an S3 storage class, spelt the way the S3 API spells it.
type Class string

const (
	Standard           Class = "STANDARD"
	IntelligentTiering Class = "INTELLIGENT_TIERING"
	StandardIA         Class = "STANDARD_IA"
	OneZoneIA          Class = "ONEZONE_IA"
	GlacierIR          Class = "GLACIER_IR"   // Glacier Instant Retrieval
	Glacier            Class = "GLACIER"      // Glacier Flexible Retrieval
	DeepArchive        Class = "DEEP_ARCHIVE" // Glacier Deep Archive
)

// ClassInfo is what vmobj needs to know about a class to plan around it.
// Figures are AWS's published rules; prices are left out on purpose, since
// they change and vary by region.
type ClassInfo struct {
	Class Class
	Name  string // as shown in the AWS console
	// MinDays is the minimum storage duration: deleting or overwriting an
	// object sooner is billed as if it had been kept this long.
	MinDays int
	// MinBillable is the smallest size billed per object (smaller objects
	// are billed as this size).
	MinBillable int64
	// Archived classes can't be read until a restore request finishes.
	Archived bool
	// RestoreWait is the typical time for a restore, by retrieval tier.
	RestoreWait map[Tier]time.Duration
}

// Tier is a retrieval speed for archived objects.
type Tier string

const (
	Expedited Tier = "Expedited"
	StandardT Tier = "Standard"
	Bulk      Tier = "Bulk"
)

// ParseTier accepts expedited, standard or bulk, in any case.
func ParseTier(s string) (Tier, error) {
	for _, t := range []Tier{Expedited, StandardT, Bulk} {
		if strings.EqualFold(s, string(t)) {
			return t, nil
		}
	}
	return "", fmt.Errorf("unknown retrieval tier %q (want expedited, standard or bulk)", s)
}

var classes = map[Class]ClassInfo{
	Standard:           {Class: Standard, Name: "S3 Standard"},
	IntelligentTiering: {Class: IntelligentTiering, Name: "S3 Intelligent-Tiering"},
	StandardIA:         {Class: StandardIA, Name: "S3 Standard-IA", MinDays: 30, MinBillable: 128 << 10},
	OneZoneIA:          {Class: OneZoneIA, Name: "S3 One Zone-IA", MinDays: 30, MinBillable: 128 << 10},
	GlacierIR:          {Class: GlacierIR, Name: "S3 Glacier Instant Retrieval", MinDays: 90, MinBillable: 128 << 10},
	Glacier: {Class: Glacier, Name: "S3 Glacier Flexible Retrieval", MinDays: 90, Archived: true,
		RestoreWait: map[Tier]time.Duration{Expedited: 5 * time.Minute, StandardT: 5 * time.Hour, Bulk: 12 * time.Hour}},
	DeepArchive: {Class: DeepArchive, Name: "S3 Glacier Deep Archive", MinDays: 180, Archived: true,
		// Deep Archive has no Expedited tier.
		RestoreWait: map[Tier]time.Duration{StandardT: 12 * time.Hour, Bulk: 48 * time.Hour}},
}

// Info returns the facts about a class. An empty class means Standard,
// which is what S3 reports for objects stored without one.
func Info(c Class) (ClassInfo, bool) {
	if c == "" {
		c = Standard
	}
	ci, ok := classes[c]
	return ci, ok
}

// ParseClass accepts the API spelling or a few friendly names
// ("deep-archive", "standard-ia", "glacier-ir", "flexible").
func ParseClass(s string) (Class, error) {
	norm := strings.ToUpper(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(s)))
	alias := map[string]Class{
		"DEEP": DeepArchive, "DEEPARCHIVE": DeepArchive, "GLACIER_DEEP_ARCHIVE": DeepArchive,
		"FLEXIBLE": Glacier, "GLACIER_FLEXIBLE": Glacier, "GLACIER_FLEXIBLE_RETRIEVAL": Glacier,
		"INSTANT": GlacierIR, "GLACIER_INSTANT": GlacierIR, "GLACIER_INSTANT_RETRIEVAL": GlacierIR,
		"IA": StandardIA, "INTELLIGENT": IntelligentTiering, "ONE_ZONE_IA": OneZoneIA,
	}
	if c, ok := alias[norm]; ok {
		return c, nil
	}
	if _, ok := classes[Class(norm)]; ok {
		return Class(norm), nil
	}
	return "", fmt.Errorf("unknown storage class %q", s)
}

// Slowest returns the longest restore wait among the classes at the given
// tier, and the class responsible. A restore can't finish before every
// object it needs is readable, so this is the wait for the whole restore.
// Classes without the tier (Deep Archive has no Expedited) fall back to
// their fastest tier.
func Slowest(cs []Class, t Tier) (time.Duration, Class) {
	var worst time.Duration
	var which Class
	for _, c := range cs {
		ci, _ := Info(c)
		if !ci.Archived {
			continue
		}
		d, ok := ci.RestoreWait[t]
		if !ok {
			d = ci.RestoreWait[StandardT]
		}
		if d > worst {
			worst, which = d, c
		}
	}
	return worst, which
}
