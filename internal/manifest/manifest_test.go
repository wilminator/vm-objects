// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package manifest

import (
	"slices"
	"testing"
)

func TestCompareNamesOrdersSameSecondSuffixes(t *testing.T) {
	// Plain text order, as a storage listing returns it (with ".manifest"
	// removed), is wrong for the suffixed names.
	got := []string{
		"20261107T161921Z-incremental-10",
		"20261107T161921Z-incremental-2",
		"20261107T161921Z-incremental-3",
		"20261107T161921Z-incremental",
		"20261107T161920Z-full",
		"notes",
		"20261107T161922Z-incremental",
	}
	slices.SortStableFunc(got, CompareNames)
	want := []string{
		"20261107T161920Z-full",
		"20261107T161921Z-incremental",
		"20261107T161921Z-incremental-2",
		"20261107T161921Z-incremental-3",
		"20261107T161921Z-incremental-10",
		"20261107T161922Z-incremental",
		"notes",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order:\n got %q\nwant %q", got, want)
	}
}
