// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package progress prints how far a long job has got: a status line
// rewritten in place on a terminal, a line every few seconds in a log,
// dots, JSON for scripts, or nothing.
package progress

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is how progress is shown.
type Mode int

const (
	Auto  Mode = iota // Line on a terminal, Lines otherwise
	Line              // one status line, rewritten in place
	Lines             // a new line every interval
	Dots              // a "." per DotSize bytes
	JSON              // one JSON object per line
	None              // nothing
)

var modeNames = map[string]Mode{"auto": Auto, "line": Line, "lines": Lines, "dots": Dots, "json": JSON, "none": None}

// ParseMode reads a --progress value.
func ParseMode(s string) (Mode, error) {
	if m, ok := modeNames[s]; ok {
		return m, nil
	}
	return 0, fmt.Errorf("unknown --progress %q (use auto, line, lines, dots, json or none)", s)
}

// Config says how to show progress. The zero value (Auto, the default
// template and intervals, standard error) is ready to use.
type Config struct {
	Mode     Mode
	Template *Template     // nil: DefaultTemplate
	Interval time.Duration // 0: the mode's default
	DotSize  int64         // Dots: bytes per dot; 0: 1 GiB
	Out      io.Writer     // nil: standard error
	// Terminal says whether Out is a terminal, for Auto. nil: check
	// whether standard error is one.
	Terminal *bool

	now func() time.Time // tests
}

// resolved fills in the defaults.
func (c Config) resolved() Config {
	if c.Out == nil {
		c.Out = os.Stderr
	}
	if c.Mode == Auto {
		tty := false
		if c.Terminal != nil {
			tty = *c.Terminal
		} else {
			tty = isTerminal(os.Stderr)
		}
		if tty {
			c.Mode = Line
		} else {
			c.Mode = Lines
		}
	}
	if c.Template == nil {
		c.Template, _ = ParseTemplate(DefaultTemplate)
		if c.Mode == Line {
			c.Template, _ = ParseTemplate("%w " + DefaultTemplate + "  %r left")
		}
	}
	if c.Interval <= 0 {
		switch c.Mode {
		case Line:
			c.Interval = 250 * time.Millisecond
		case Dots, JSON:
			c.Interval = time.Second
		default:
			c.Interval = 5 * time.Second
		}
	}
	if c.DotSize <= 0 {
		c.DotSize = 1 << 30
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// isTerminal reports whether f is a character device (a terminal, as far
// as this package cares), without needing a terminal library.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Task is one job being measured. Set or Add the bytes done from any
// goroutine; call Finish once at the end.
type Task struct {
	cfg   Config
	label string
	total atomic.Int64
	done  atomic.Int64
	start time.Time

	mu       sync.Mutex // guards output
	spin     int
	dots     int64 // dots printed so far
	finished bool
	stop     chan struct{}
	exited   chan struct{}
}

// Start begins showing progress for a job of total bytes. label names
// it, for %n and JSON.
func (c Config) Start(label string, total int64) *Task {
	cfg := c.resolved()
	t := &Task{cfg: cfg, label: label, start: cfg.now(), stop: make(chan struct{}), exited: make(chan struct{})}
	t.total.Store(total)
	if cfg.Mode == None {
		close(t.exited)
		return t
	}
	go t.loop()
	return t
}

// Set records the bytes done so far.
func (t *Task) Set(done int64) { t.done.Store(done) }

// Add adds to the bytes done.
func (t *Task) Add(n int64) { t.done.Add(n) }

// SetTotal changes the job's size, if it becomes known late.
func (t *Task) SetTotal(total int64) { t.total.Store(total) }

func (t *Task) loop() {
	defer close(t.exited)
	tick := time.NewTicker(t.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.render(false)
		}
	}
}

// Finish stops the updates and prints the final state: the line mode's
// line is completed with a newline, dots end their row, JSON prints a
// last object with "finished": true. Safe to call more than once.
func (t *Task) Finish() {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	t.mu.Unlock()
	if t.cfg.Mode == None {
		return
	}
	close(t.stop)
	<-t.exited
	t.render(true)
}

func (t *Task) stats() Stats {
	return Stats{Label: t.label, Done: t.done.Load(), Total: t.total.Load(), Elapsed: t.cfg.now().Sub(t.start), Spin: t.spin}
}

func (t *Task) render(final bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.stats()
	t.spin++
	w := t.cfg.Out
	switch t.cfg.Mode {
	case Line:
		// \r returns to the start of the line; ESC[K clears what an
		// earlier, longer line left behind.
		end := ""
		if final {
			end = "\n"
		}
		fmt.Fprintf(w, "\r%s\x1b[K%s", t.cfg.Template.Render(st), end)
	case Lines:
		if !final { // the command prints its own summary at the end
			fmt.Fprintln(w, t.cfg.Template.Render(st))
		}
	case Dots:
		want := st.Done / t.cfg.DotSize
		if want > t.dots {
			fmt.Fprint(w, strings.Repeat(".", int(want-t.dots)))
			t.dots = want
		}
		if final && t.dots > 0 {
			fmt.Fprintln(w)
		}
	case JSON:
		rem, ok := st.Remaining()
		obj := map[string]any{
			"label": st.Label, "done": st.Done, "total": st.Total,
			"percent":       round1(st.Percent()),
			"bytes_per_sec": int64(st.Rate()),
			"elapsed_s":     round1(st.Elapsed.Seconds()),
			"finished":      final,
		}
		if ok {
			obj["remaining_s"] = round1(rem.Seconds())
		}
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "%s\n", b)
	}
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
