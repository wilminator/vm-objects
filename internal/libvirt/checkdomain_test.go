// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package libvirt

import "testing"

func TestCheckDomain(t *testing.T) {
	for _, good := range []string{"WinGames", "42497a00-6fa5-4747-98f2-a1f4090613ee", "1_WinGames", "win 11"} {
		if err := CheckDomain(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
	for _, bad := range []string{"", "-c", "--connect=qemu+ssh://x/system", "a\nb", "a\x1b[2J"} {
		if CheckDomain(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
