// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package units parses and prints byte sizes such as "64KiB" and "4MiB".
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var suffixes = []struct {
	name string
	mult int64
}{
	{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
	{"B", 1},
}

// ParseSize turns "64KiB", "4MiB", "1048576" or "1.5GiB" into bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf.name) {
			num := strings.TrimSpace(strings.TrimSuffix(s, suf.name))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
				return 0, fmt.Errorf("size %q: not a number", s)
			}
			v := f * float64(suf.mult)
			if v >= math.MaxInt64 {
				return 0, fmt.Errorf("size %q is too large", s)
			}
			return int64(v), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("size %q: use a number of bytes or a suffix such as KiB, MiB, GiB", s)
	}
	return n, nil
}

// Format prints a byte count in binary units, for example "2.68 GiB".
func Format(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TiB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
