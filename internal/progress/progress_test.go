// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package progress

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	gib = 1 << 30
	mib = 1 << 20
)

// 1.7 GiB of 768 GiB after 4s: 435.2 MiB/s.
var sample = Stats{Label: "disk vda", Done: 1825361100, Total: 768 * gib, Elapsed: 4 * time.Second}

func render(t *testing.T, tmpl string, st Stats) string {
	t.Helper()
	p, err := ParseTemplate(tmpl)
	if err != nil {
		t.Fatalf("%q: %v", tmpl, err)
	}
	return p.Render(st)
}

func TestTemplates(t *testing.T) {
	for _, tc := range []struct{ tmpl, want string }{
		{"%P %p of %S (%s)", "0.2% 1.70 GiB of 768.00 GiB (435.20 MiB/s)"},
		{"%{MiB}p of %{MiB}S at %.0{MiB}s", "1740.80 MiB of 786432.00 MiB at 435 MiB/s"},
		{"%{GB}p", "1.83 GB"}, // decimal units are decimal
		{"%{GiB}p", "1.70 GiB"},
		{"%{B}p %{B}S %{B}s", "1825361100 824633720832 456340275"},
		{"[%8.2P]", "[   0.22%]"},
		{"%.0p", "2 GiB"},
		{"%n: %e, %r left", "disk vda: 4s, 30m03s left"},
		{"100%%", "100%"},
		{"%w%w", "//"},
	} {
		if got := render(t, tc.tmpl, sample); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.tmpl, got, tc.want)
		}
	}
	// The spinner turns with each render; remaining is "?" with no speed.
	if got := render(t, "%w", Stats{Spin: 2}); got != `\` {
		t.Errorf("spinner frame 2: %q", got)
	}
	if got := render(t, "%r", Stats{Total: 10}); got != "?" {
		t.Errorf("remaining with no speed: %q", got)
	}
	if got := render(t, "%e", Stats{Elapsed: 3723 * time.Second}); got != "1h02m03s" {
		t.Errorf("hours: %q", got)
	}
}

func TestTemplateErrors(t *testing.T) {
	for _, tmpl := range []string{
		"%x",          // unknown code
		"%{mb}p",      // units are case-sensitive
		"%{Gib}p",     //
		"%{GiB}P",     // percent has no unit
		"%{GiB}n",     // nor does the label
		"%.2e",        // nor precision on a duration
		"%.P",         // '.' needs a number
		"%{GiB",       // unclosed
		"ends with %", // dangling
		"%5%",         // %% takes nothing
	} {
		if _, err := ParseTemplate(tmpl); err == nil {
			t.Errorf("%q was accepted", tmpl)
		}
	}
	_, err := ParseTemplate("%{mb}p")
	if err == nil || !strings.Contains(err.Error(), "MiB") || !strings.Contains(err.Error(), "MB") {
		t.Errorf("unknown-unit error should list the units: %v", err)
	}
}

// clockTask makes a task whose clock the test moves, with the loop not
// running, so render can be called directly.
func clockTask(mode Mode, tmpl string, total int64) (*Task, *bytes.Buffer, *time.Time) {
	var buf bytes.Buffer
	now := time.Unix(1000, 0)
	p, _ := ParseTemplate(tmpl)
	cfg := Config{Mode: mode, Template: p, Out: &buf, DotSize: 10, now: func() time.Time { return now }}.resolved()
	t := &Task{cfg: cfg, label: "x", start: now, stop: make(chan struct{}), exited: make(chan struct{})}
	t.total.Store(total)
	close(t.exited)
	return t, &buf, &now
}

func TestLineMode(t *testing.T) {
	task, buf, now := clockTask(Line, "%P", 100)
	task.Set(10)
	*now = now.Add(time.Second)
	task.render(false)
	task.Set(100)
	task.render(true)
	if got := buf.String(); got != "\r10.0%\x1b[K\r100.0%\x1b[K\n" {
		t.Fatalf("line mode wrote %q", got)
	}
}

func TestLinesMode(t *testing.T) {
	task, buf, _ := clockTask(Lines, "%P", 100)
	task.Set(50)
	task.render(false)
	task.render(true) // the final render leaves the summary to the command
	if got := buf.String(); got != "50.0%\n" {
		t.Fatalf("lines mode wrote %q", got)
	}
}

func TestDotsMode(t *testing.T) {
	task, buf, _ := clockTask(Dots, "", 100) // a dot per 10 bytes
	task.Set(25)
	task.render(false)
	task.Set(61)
	task.render(false)
	task.render(true)
	if got := buf.String(); got != "......\n" {
		t.Fatalf("dots mode wrote %q", got)
	}
}

func TestJSONMode(t *testing.T) {
	task, buf, now := clockTask(JSON, "", 200)
	task.Set(50)
	*now = now.Add(2 * time.Second)
	task.render(false)
	task.Set(200)
	task.render(true)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSON lines, got %q", buf.String())
	}
	var first, last map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[1]), &last)
	if first["percent"] != 25.0 || first["bytes_per_sec"] != 25.0 || first["remaining_s"] != 6.0 || first["finished"] != false {
		t.Errorf("first object: %v", first)
	}
	if last["finished"] != true || last["done"] != 200.0 {
		t.Errorf("last object: %v", last)
	}
}

func TestAutoMode(t *testing.T) {
	yes, no := true, false
	if m := (Config{Terminal: &yes}).resolved().Mode; m != Line {
		t.Errorf("auto on a terminal: %v", m)
	}
	if m := (Config{Terminal: &no}).resolved().Mode; m != Lines {
		t.Errorf("auto off a terminal: %v", m)
	}
}

// The real loop prints on its interval and Finish stops it.
func TestStartAndFinish(t *testing.T) {
	var buf safeBuffer
	p, _ := ParseTemplate("%p")
	task := Config{Mode: Lines, Template: p, Interval: 10 * time.Millisecond, Out: &buf}.Start("x", 100)
	task.Set(5)
	time.Sleep(50 * time.Millisecond)
	task.Finish()
	task.Finish() // a second call is harmless
	n := strings.Count(buf.String(), "5 B\n")
	if n == 0 {
		t.Fatalf("no progress lines: %q", buf.String())
	}
	after := buf.String()
	time.Sleep(30 * time.Millisecond)
	if buf.String() != after {
		t.Fatal("output continued after Finish")
	}
	if none := (Config{Mode: None, Out: &buf}).Start("x", 1); none != nil {
		none.Finish()
	}
}

func TestParseMode(t *testing.T) {
	for name, want := range modeNames {
		if m, err := ParseMode(name); err != nil || m != want {
			t.Errorf("%s: %v %v", name, m, err)
		}
	}
	if _, err := ParseMode("bar"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
