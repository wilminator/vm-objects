// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package metrics records where a run spends its time: how long each
// stage (reading the source, hashing, sealing, storage writes and reads,
// writing a restore) took in total, how often, how many bytes, and how
// long one operation usually takes.
//
// A nil *Recorder is valid and records nothing, so code can call it
// unconditionally. Every method is safe from many goroutines at once.
package metrics

import (
	"fmt"
	"io"
	"math/bits"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wilminator/vm-objects/internal/units"
)

// Stage names used across vmobj, in the order a report lists them.
const (
	SourceRead  = "source read"      // reading the disk being backed up
	Hash        = "hash"             // content IDs of blocks and extents
	Seal        = "compress+encrypt" // sealing one object
	StoreWrite  = "storage write"    // one object written (file or upload)
	StoreRead   = "storage read"     // one object or range read
	StoreList   = "storage list"     // listing keys
	Open        = "decrypt+check"    // opening and checking one object
	OutputWrite = "restore write"    // writing restored data to the target
	TargetRead  = "target read"      // copy and in-place restores: reading what the target holds
)

var order = []string{SourceRead, Hash, Seal, StoreWrite, StoreRead, StoreList, Open, TargetRead, OutputWrite}

// buckets holds latencies by powers of two of microseconds: bucket i
// counts operations that took under 2^i µs (bucket 0: under 1 µs).
const buckets = 40

type stage struct {
	count, bytes, nanos atomic.Int64
	hist                [buckets]atomic.Int64
}

// Recorder collects stage timings. The zero value is not ready; use New.
type Recorder struct {
	mu     sync.Mutex
	stages map[string]*stage
	start  time.Time
}

// New starts a recorder; the report's wall time counts from here.
func New() *Recorder { return &Recorder{stages: map[string]*stage{}, start: time.Now()} }

func (r *Recorder) stage(name string) *stage {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stages[name]
	if s == nil {
		s = &stage{}
		r.stages[name] = s
	}
	return s
}

// Observe records one operation of a stage.
func (r *Recorder) Observe(name string, d time.Duration, bytes int64) {
	if r == nil {
		return
	}
	s := r.stage(name)
	s.count.Add(1)
	s.bytes.Add(bytes)
	s.nanos.Add(int64(d))
	us := uint64(d / time.Microsecond)
	i := bits.Len64(us) // 0 for under 1 µs, then 1, 2, 3... per doubling
	if i >= buckets {
		i = buckets - 1
	}
	s.hist[i].Add(1)
}

// Start times one operation: call the returned func with its byte count
// when it ends.
//
//	done := rec.Start(metrics.StoreWrite)
//	err := backend.Put(ctx, key, data)
//	done(int64(len(data)))
func (r *Recorder) Start(name string) func(bytes int64) {
	if r == nil {
		return func(int64) {}
	}
	t := time.Now()
	return func(bytes int64) { r.Observe(name, time.Since(t), bytes) }
}

// StageStats is one stage's totals.
type StageStats struct {
	Name  string
	Count int64
	Bytes int64
	Total time.Duration // summed over every operation, across goroutines
	P50   time.Duration // upper bound of the bucket holding the median
	P95   time.Duration
}

// Mean is the average time per operation.
func (s StageStats) Mean() time.Duration {
	if s.Count == 0 {
		return 0
	}
	return s.Total / time.Duration(s.Count)
}

// Stats returns every stage that saw an operation, in report order.
func (r *Recorder) Stats() []StageStats {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	names := make([]string, 0, len(r.stages))
	for n := range r.stages {
		names = append(names, n)
	}
	r.mu.Unlock()
	rank := func(n string) int {
		for i, o := range order {
			if o == n {
				return i
			}
		}
		return len(order)
	}
	sort.Slice(names, func(i, j int) bool {
		if rank(names[i]) != rank(names[j]) {
			return rank(names[i]) < rank(names[j])
		}
		return names[i] < names[j]
	})
	var out []StageStats
	for _, n := range names {
		s := r.stage(n)
		st := StageStats{Name: n, Count: s.count.Load(), Bytes: s.bytes.Load(), Total: time.Duration(s.nanos.Load())}
		if st.Count == 0 {
			continue
		}
		var hist [buckets]int64
		for i := range hist {
			hist[i] = s.hist[i].Load()
		}
		st.P50, st.P95 = quantile(hist, st.Count, 0.50), quantile(hist, st.Count, 0.95)
		out = append(out, st)
	}
	return out
}

// quantile returns the upper bound of the bucket holding the q-th
// operation: accurate to within a factor of two, which is enough to tell
// a 10 ms disk flush from a 100 µs one.
func quantile(hist [buckets]int64, count int64, q float64) time.Duration {
	want := int64(q*float64(count) + 0.5)
	if want < 1 {
		want = 1
	}
	var seen int64
	for i, n := range hist {
		seen += n
		if seen >= want {
			if i == 0 {
				return time.Microsecond
			}
			return time.Duration(1<<uint(i)) * time.Microsecond
		}
	}
	return 0
}

// Report writes a table of the stages, then the wall time. Because
// stages can overlap (hashing runs on several cores at once), their
// totals can add up to more than the wall time; a stage whose total is
// close to the wall time is the one the run waited on.
func (r *Recorder) Report(w io.Writer) {
	if r == nil {
		return
	}
	wall := time.Since(r.start)
	stats := r.Stats()
	fmt.Fprintf(w, "Time by stage (wall time %s; stages can overlap):\n", wall.Round(time.Second))
	fmt.Fprintf(w, "  %-17s %10s %6s %10s %12s %10s %10s %10s\n", "stage", "total", "share", "ops", "data", "mean", "p50", "p95")
	for _, s := range stats {
		share := 0.0
		if wall > 0 {
			share = 100 * float64(s.Total) / float64(wall)
		}
		data := "-"
		if s.Bytes > 0 {
			data = units.Format(s.Bytes)
		}
		fmt.Fprintf(w, "  %-17s %10s %5.0f%% %10d %12s %10s %10s %10s\n", s.Name,
			round(s.Total), share, s.Count, data, round(s.Mean()), round(s.P50), round(s.P95))
	}
	if len(stats) == 0 {
		fmt.Fprintln(w, "  (nothing recorded)")
	}
}

// round prints durations at a useful precision for their size.
func round(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(10 * time.Microsecond).String()
	default:
		return strings.TrimSpace(d.Round(time.Microsecond).String())
	}
}
