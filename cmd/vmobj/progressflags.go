// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wilminator/vm-objects/internal/engine"
	"github.com/wilminator/vm-objects/internal/metrics"
	"github.com/wilminator/vm-objects/internal/progress"
	"github.com/wilminator/vm-objects/internal/source"
	"github.com/wilminator/vm-objects/internal/units"
)

// progressFlags are the progress options every long-running command takes.
type progressFlags struct {
	mode, format, dotSize *string
	interval              *time.Duration
}

func addProgressFlags(fs *flag.FlagSet) *progressFlags {
	return &progressFlags{
		mode:     fs.String("progress", "auto", "how to show progress: auto, line, lines, dots, json or none"),
		format:   fs.String("progress-format", "", "progress line template, e.g. '%P %p of %S (%s)' (see vmobj help progress)"),
		dotSize:  fs.String("progress-dot-size", "1GiB", "bytes per dot with --progress dots"),
		interval: fs.Duration("progress-interval", 0, "time between updates (default 250ms on one line, 5s for lines)"),
	}
}

// config checks the options. Mistakes are reported before any work starts.
func (f *progressFlags) config() (progress.Config, error) {
	var c progress.Config
	m, err := progress.ParseMode(*f.mode)
	if err != nil {
		return c, err
	}
	c.Mode = m
	if *f.format != "" {
		if c.Template, err = progress.ParseTemplate(*f.format); err != nil {
			return c, err
		}
	}
	if c.DotSize, err = units.ParseSize(*f.dotSize); err != nil || c.DotSize <= 0 {
		return c, fmt.Errorf("--progress-dot-size %q: use a size such as 512MiB or 1GiB", *f.dotSize)
	}
	c.Interval = *f.interval
	return c, nil
}

// counting wraps a source and counts what a scan reads into a task.
type counting struct {
	source.Source
	t *progress.Task
}

func (c *counting) ReadAt(b []byte, off int64) (int, error) {
	n, err := c.Source.ReadAt(b, off)
	c.t.Add(int64(n))
	return n, err
}

// withProgress shows progress while src is read. Call the returned func
// when the read is over.
func withProgress(pc progress.Config, label string, src source.Source) (source.Source, func()) {
	t := pc.Start(label, src.Size())
	return &counting{Source: src, t: t}, t.Finish
}

// restoreProgress shows what a restore is doing: a line when it starts
// checking the backup, writing the disk and applying later changes
// (naming the disk, if given), and progress while it writes. Call the
// returned func when the restore ends.
// restoreProgress shows a restore's phases. scratch, when set, names where
// a copy-mode restore first writes the whole disk.
func restoreProgress(pc progress.Config, dest, scratch string) (func(target string, p engine.Phase, done, total int64), func()) {
	var (
		mu      sync.Mutex
		t       *progress.Task
		current string
		phase   engine.Phase
	)
	finish := func() {
		if t != nil {
			t.Finish()
			t = nil
		}
	}
	say := func(format string, a ...any) {
		if pc.Mode != progress.None && pc.Mode != progress.JSON {
			fmt.Fprintf(os.Stderr, format, a...)
		}
	}
	report := func(target string, p engine.Phase, done, total int64) {
		mu.Lock()
		defer mu.Unlock()
		label := dest
		if target != "" {
			label = "disk " + target
		}
		if scratch != "" && (p == engine.PhaseWriting || p == engine.PhaseChanges) {
			label = scratch
		}
		switch {
		case p == engine.PhaseChecking:
			finish()
			phase, current = p, target
			say("Checking that the backup for %s is complete...\n", label)
		case p != phase || target != current || t == nil:
			finish()
			phase, current = p, target
			name := label
			switch p {
			case engine.PhaseChanges:
				name = label + " (later changes)"
				say("Applying %s of later changes to %s...\n", units.Format(total), label)
			case engine.PhaseUpdating:
				name = label + " (compare)"
				say("Comparing %s of %s and writing only what differs...\n", units.Format(total), label)
			case engine.PhaseCheckingTarget:
				name = label + " (check)"
				say("Reading %s back to check it...\n", label)
			case engine.PhaseApplying:
				name = label + " (apply)"
				say("Applying %s of checked changes to %s...\n", units.Format(total), label)
			default:
				say("Writing %s to %s...\n", units.Format(total), label)
			}
			t = pc.Start(name, total)
			t.Set(done)
		default:
			t.Set(done)
		}
	}
	return report, func() { mu.Lock(); finish(); mu.Unlock() }
}

const progressHelp = `Progress options (measure, backup, backup-vm, restore, restore-vm, copy):

  --progress MODE           auto (default): one line rewritten in place on a
                            terminal, a new line every 5s otherwise (logs, cron)
                            line | lines | dots | json | none
  --progress-format TEMPLATE  what each update shows (line and lines modes)
  --progress-interval 5s    time between updates
  --progress-dot-size 1GiB  bytes per "." with --progress dots

TEMPLATE is text with codes, printf style: %[width][.precision][{unit}]code

  %P  percent done        %p  bytes done       %S  total bytes
  %s  average speed       %e  time elapsed     %r  time remaining
  %n  what is being done  %w  spinner / - \ |  %%  a literal %

width pads on the left; precision sets decimals (P p S s); {unit} fixes the
unit of p, S and s:

  binary, powers of 1024:   KiB MiB GiB TiB     (the default, chosen to fit)
  decimal, powers of 1000:  kB (or KB) MB GB TB
  {B}: a plain number of bytes, no label (for scripts)

The unit you write is the unit printed: 1.70 GiB is 1.83 GB. Units are
case-sensitive; an unknown code or unit is an error before any work starts.

Examples:
  '%P %p of %S (%s)'                   10.2% 1.70 GiB of 16.67 GiB (432.70 MiB/s)
  '%{MiB}p of %{MiB}S at %.0{MiB}s'    1740.80 MiB of 17070.08 MiB at 433 MiB/s
  '%w %6.1P  %r left'                  \   10.2%  35s left

json prints one object per update: label, done, total, percent,
bytes_per_sec, elapsed_s, remaining_s (once known) and finished.
`

// runMetrics records stage timings for this run when --metrics is given;
// nil otherwise (and then records nothing). There is one per process: a
// command opens its repositories after its flags are parsed.
var runMetrics *metrics.Recorder

func addMetricsFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("metrics", false, "print where the time went (by stage: read, hash, compress+encrypt, storage...) at the end")
}

func startMetrics(on bool) {
	if on {
		runMetrics = metrics.New()
	}
}

// copyProgress shows each backup being copied: a line as it starts its
// base blocks and its later changes, and progress while it copies.
func copyProgress(pc progress.Config) (func(name string, p engine.Phase, done, total int64), func()) {
	var (
		mu      sync.Mutex
		t       *progress.Task
		current string
		phase   engine.Phase
	)
	finish := func() {
		if t != nil {
			t.Finish()
			t = nil
		}
	}
	report := func(name string, p engine.Phase, done, total int64) {
		mu.Lock()
		defer mu.Unlock()
		if t == nil || name != current || p != phase {
			finish()
			current, phase = name, p
			label := name
			if pc.Mode != progress.None && pc.Mode != progress.JSON {
				if p == engine.PhaseChanges {
					label = name + " (later changes)"
					fmt.Fprintf(os.Stderr, "Copying %s: %s of later changes...\n", name, units.Format(total))
				} else {
					fmt.Fprintf(os.Stderr, "Copying %s: %s disk...\n", name, units.Format(total))
				}
			}
			t = pc.Start(label, total)
		}
		t.Set(done)
	}
	return report, func() { mu.Lock(); finish(); mu.Unlock() }
}
