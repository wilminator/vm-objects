// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"testing"
	"time"
)

func TestParseRestoreHeader(t *testing.T) {
	cases := []struct {
		in      string
		ongoing bool
		until   time.Time
	}{
		{"", false, time.Time{}},
		{`ongoing-request="true"`, true, time.Time{}},
		{`ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`, false,
			time.Date(2012, 12, 21, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		on, until := parseRestore(c.in)
		if on != c.ongoing || !until.Equal(c.until) {
			t.Errorf("parseRestore(%q) = %v %v", c.in, on, until)
		}
	}
}
