// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package progress

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/units"
)

// A Template describes one progress line. It is plain text with codes in
// it, in the style of printf:
//
//	%[width][.precision][{unit}]code
//
// Codes:
//
//	%P  percent done            10.2%
//	%p  bytes done              1.70 GiB
//	%S  total bytes             768.00 GiB
//	%s  average speed           432.70 MiB/s
//	%e  time elapsed            4m12s
//	%r  time remaining          26m40s ("?" until there is a speed)
//	%n  what is being worked on disk vda
//	%w  a spinner               / - \ |
//	%%  a literal %
//
// width pads the value on the left to at least that many characters.
// precision sets decimal places (P, p, S, s). {unit} fixes the unit of
// p, S and s; without it the unit is chosen to fit, always a binary one.
const DefaultTemplate = "%6.1P  %p of %S  (%s)"

// Units that {unit} accepts. Binary units are powers of 1024, decimal
// units powers of 1000; the label printed is the one asked for. {B} is a
// plain whole number of bytes with no label, for scripts.
var fixedUnits = map[string]float64{
	"B":   1,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	"kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
}

// UnitNames lists the accepted units, for error messages and help.
const UnitNames = "B, KiB, MiB, GiB, TiB (powers of 1024) or kB (also KB), MB, GB, TB (powers of 1000)"

type Template struct {
	parts []part
	raw   string
}

type part struct {
	text  string // literal text, when code is 0
	code  byte
	width int
	prec  int // -1: the code's default
	unit  string
}

// ParseTemplate checks a template and prepares it. Unknown codes, units on
// codes that have no unit, and unknown units are errors.
func ParseTemplate(s string) (*Template, error) {
	t := &Template{raw: s}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, part{text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			lit.WriteByte(s[i])
			continue
		}
		start := i
		i++
		p := part{prec: -1}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			p.width = p.width*10 + int(s[i]-'0')
			i++
		}
		if i < len(s) && s[i] == '.' {
			i++
			j := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if j == i {
				return nil, fmt.Errorf("progress format %q: a '.' at position %d needs a number after it", s, start+1)
			}
			p.prec, _ = strconv.Atoi(s[j:i])
		}
		if i < len(s) && s[i] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("progress format %q: '{' at position %d is never closed", s, i+1)
			}
			p.unit = s[i+1 : i+end]
			if _, ok := fixedUnits[p.unit]; !ok {
				return nil, fmt.Errorf("progress format %q: unknown unit {%s}; use %s (case matters)", s, p.unit, UnitNames)
			}
			i += end + 1
		}
		if i >= len(s) {
			return nil, fmt.Errorf("progress format %q ends in the middle of a code", s)
		}
		p.code = s[i]
		switch p.code {
		case '%':
			if p.width != 0 || p.prec >= 0 || p.unit != "" {
				return nil, fmt.Errorf("progress format %q: %%%% takes no width, precision or unit", s)
			}
			lit.WriteByte('%')
			continue
		case 'p', 'S', 's':
		case 'P':
			if p.unit != "" {
				return nil, fmt.Errorf("progress format %q: %%P is a percentage and takes no unit", s)
			}
		case 'e', 'r', 'n', 'w':
			if p.unit != "" || p.prec >= 0 {
				return nil, fmt.Errorf("progress format %q: %%%c takes no precision or unit", s, p.code)
			}
		default:
			return nil, fmt.Errorf("progress format %q: unknown code %%%c (use P p S s e r n w or %%%%)", s, p.code)
		}
		flush()
		t.parts = append(t.parts, p)
	}
	flush()
	return t, nil
}

// String returns the template as it was written.
func (t *Template) String() string { return t.raw }

// Stats are the numbers a template is filled from.
type Stats struct {
	Label   string
	Done    int64
	Total   int64
	Elapsed time.Duration
	Spin    int // advances on every render, for %w
}

// Rate is the average speed in bytes per second (0 until time has passed).
func (st Stats) Rate() float64 {
	if st.Elapsed <= 0 {
		return 0
	}
	return float64(st.Done) / st.Elapsed.Seconds()
}

// Percent is done as a share of total (0 when the total is unknown).
func (st Stats) Percent() float64 {
	if st.Total <= 0 {
		return 0
	}
	return 100 * float64(st.Done) / float64(st.Total)
}

// Remaining estimates the time left; ok is false until there is a speed.
func (st Stats) Remaining() (time.Duration, bool) {
	r := st.Rate()
	if r <= 0 || st.Total <= 0 {
		return 0, false
	}
	left := float64(st.Total-st.Done) / r
	if left < 0 {
		left = 0
	}
	return time.Duration(left * float64(time.Second)), true
}

const spinner = `/-\|`

// Render fills the template.
func (t *Template) Render(st Stats) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.code == 0 {
			b.WriteString(p.text)
			continue
		}
		var v string
		switch p.code {
		case 'P':
			v = strconv.FormatFloat(st.Percent(), 'f', precOr(p.prec, 1), 64) + "%"
		case 'p':
			v = formatSize(float64(st.Done), p)
		case 'S':
			v = formatSize(float64(st.Total), p)
		case 's':
			v = formatSize(st.Rate(), p)
			if p.unit != "B" {
				v += "/s"
			}
		case 'e':
			v = formatDuration(st.Elapsed)
		case 'r':
			if d, ok := st.Remaining(); ok {
				v = formatDuration(d)
			} else {
				v = "?"
			}
		case 'n':
			v = st.Label
		case 'w':
			v = string(spinner[st.Spin%len(spinner)])
		}
		if pad := p.width - len(v); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(v)
	}
	return b.String()
}

func precOr(p, def int) int {
	if p < 0 {
		return def
	}
	return p
}

func formatSize(n float64, p part) string {
	switch p.unit {
	case "":
		if p.prec < 0 {
			return units.Format(int64(n))
		}
		// Auto unit with a chosen precision: the same binary unit
		// units.Format would pick.
		for _, u := range []string{"TiB", "GiB", "MiB", "KiB"} {
			if n >= fixedUnits[u] {
				return strconv.FormatFloat(n/fixedUnits[u], 'f', p.prec, 64) + " " + u
			}
		}
		return strconv.FormatFloat(n, 'f', 0, 64) + " B"
	case "B":
		return strconv.FormatInt(int64(math.Round(n)), 10)
	default:
		return strconv.FormatFloat(n/fixedUnits[p.unit], 'f', precOr(p.prec, 2), 64) + " " + p.unit
	}
}

// formatDuration prints whole seconds: 45s, 4m12s, 1h02m03s.
func formatDuration(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	h, m, sec := s/3600, s/60%60, s%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
