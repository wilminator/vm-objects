// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Observe(StoreWrite, time.Second, 10)
	r.Start(Hash)(5)
	r.Report(&bytes.Buffer{})
	if r.Stats() != nil {
		t.Fatal("a nil recorder returned stats")
	}
}

func TestStatsAndQuantiles(t *testing.T) {
	r := New()
	// 90 fast writes (100 µs) and 10 slow ones (20 ms): the median is fast,
	// p95 is slow. That split is what tells a flush-bound run apart.
	for i := 0; i < 90; i++ {
		r.Observe(StoreWrite, 100*time.Microsecond, 4<<20)
	}
	for i := 0; i < 10; i++ {
		r.Observe(StoreWrite, 20*time.Millisecond, 4<<20)
	}
	r.Observe(Hash, time.Millisecond, 0)
	st := r.Stats()
	if len(st) != 2 || st[0].Name != Hash || st[1].Name != StoreWrite {
		t.Fatalf("stages %+v, want hash then storage write (report order)", st)
	}
	w := st[1]
	if w.Count != 100 || w.Bytes != 400<<20 {
		t.Fatalf("count %d bytes %d", w.Count, w.Bytes)
	}
	if w.Total != 90*100*time.Microsecond+10*20*time.Millisecond {
		t.Fatalf("total %v", w.Total)
	}
	// Buckets are powers of two, so each quantile is within 2x.
	if w.P50 < 100*time.Microsecond || w.P50 > 200*time.Microsecond {
		t.Errorf("p50 %v, want about 100µs", w.P50)
	}
	if w.P95 < 20*time.Millisecond || w.P95 > 40*time.Millisecond {
		t.Errorf("p95 %v, want about 20ms", w.P95)
	}
}

func TestConcurrentObserve(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				r.Start(SourceRead)(1)
			}
		}()
	}
	wg.Wait()
	if st := r.Stats(); len(st) != 1 || st[0].Count != 8000 || st[0].Bytes != 8000 {
		t.Fatalf("%+v", st)
	}
}

func TestReport(t *testing.T) {
	r := New()
	r.Observe(StoreWrite, 30*time.Millisecond, 4<<20)
	var b bytes.Buffer
	r.Report(&b)
	out := b.String()
	for _, want := range []string{"Time by stage", "storage write", "4.00 MiB", "30ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
