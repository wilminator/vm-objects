// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package storetest is a conformance suite every store.Backend must pass.
// The same tests run against the local folder backend, the S3 backend on
// the in-memory fake, and (in CI, or by hand) the S3 backend on a real
// S3-compatible service. If all three pass, the engine can't tell them
// apart, which is the point of the Backend interface.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/wilminator/vm-objects/internal/store"
)

// Run runs every check. newBackend must return an empty backend each time.
//
// Go note: t.Run starts a named subtest, so a failure reports which
// behaviour broke ("TestS3Fake/RangeReads") and others still run.
func Run(t *testing.T, newBackend func(t *testing.T) store.Backend) {
	ctx := context.Background()

	t.Run("PutGetReplace", func(t *testing.T) {
		b := newBackend(t)
		must(t, b.Put(ctx, "blocks/ab/abc", []byte("first")))
		must(t, b.Put(ctx, "blocks/ab/abc", []byte("second version")))
		got, err := b.Get(ctx, "blocks/ab/abc")
		must(t, err)
		if string(got) != "second version" {
			t.Fatalf("Get = %q, want the replacement", got)
		}
	})

	t.Run("EmptyAndBinaryObjects", func(t *testing.T) {
		b := newBackend(t)
		bin := make([]byte, 70000) // bigger than one network buffer
		for i := range bin {
			bin[i] = byte(i * 7)
		}
		must(t, b.Put(ctx, "a/empty", nil))
		must(t, b.Put(ctx, "a/bin", bin))
		if got, err := b.Get(ctx, "a/empty"); err != nil || len(got) != 0 {
			t.Fatalf("empty object: %d bytes, %v", len(got), err)
		}
		if got, err := b.Get(ctx, "a/bin"); err != nil || !bytes.Equal(got, bin) {
			t.Fatalf("binary object differs (%d bytes), %v", len(got), err)
		}
	})

	t.Run("Missing", func(t *testing.T) {
		b := newBackend(t)
		if _, err := b.Get(ctx, "nope/x"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get missing: %v, want ErrNotFound", err)
		}
		if _, err := b.GetRange(ctx, "nope/x", 0, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetRange missing: %v, want ErrNotFound", err)
		}
		if ok, err := b.Exists(ctx, "nope/x"); ok || err != nil {
			t.Fatalf("Exists missing = %v, %v", ok, err)
		}
		must(t, b.Delete(ctx, "nope/x")) // deleting a missing key is fine
	})

	t.Run("RangeReads", func(t *testing.T) {
		b := newBackend(t)
		must(t, b.Put(ctx, "p/pack", []byte("0123456789")))
		for _, c := range []struct {
			off, n int64
			want   string
		}{{0, 10, "0123456789"}, {3, 4, "3456"}, {9, 1, "9"}, {5, 0, ""}} {
			got, err := b.GetRange(ctx, "p/pack", c.off, c.n)
			if err != nil || string(got) != c.want {
				t.Fatalf("GetRange(%d,%d) = %q, %v; want %q", c.off, c.n, got, err, c.want)
			}
		}
		// Past the end, or starting past the end, must fail rather than
		// return fewer bytes than asked for.
		for _, c := range [][2]int64{{6, 5}, {10, 1}, {50, 2}} {
			if got, err := b.GetRange(ctx, "p/pack", c[0], c[1]); err == nil {
				t.Fatalf("GetRange(%d,%d) = %q, want an error", c[0], c[1], got)
			}
		}
	})

	t.Run("ExistsAndDelete", func(t *testing.T) {
		b := newBackend(t)
		must(t, b.Put(ctx, "m/vm1/x.manifest", []byte("m")))
		if ok, err := b.Exists(ctx, "m/vm1/x.manifest"); !ok || err != nil {
			t.Fatalf("Exists = %v, %v", ok, err)
		}
		must(t, b.Delete(ctx, "m/vm1/x.manifest"))
		if ok, _ := b.Exists(ctx, "m/vm1/x.manifest"); ok {
			t.Fatal("object still exists after Delete")
		}
	})

	t.Run("ListPrefixSortedAndPaged", func(t *testing.T) {
		b := newBackend(t)
		var want []string
		// Enough keys to span several pages on the fake (3 per page).
		for i := 9; i >= 0; i-- {
			k := fmt.Sprintf("blocks/%02x/key%d", i, i)
			must(t, b.Put(ctx, k, []byte{byte(i)}))
			want = append(want, k)
		}
		must(t, b.Put(ctx, "manifests/vm/a.manifest", nil))
		must(t, b.Put(ctx, "blocksX", nil)) // shares the text prefix "blocks"
		slices.Sort(want)

		got, err := b.List(ctx, "blocks/")
		must(t, err)
		if !slices.Equal(got, want) {
			t.Fatalf("List(blocks/) =\n %v\nwant\n %v", got, want)
		}
		if got, _ := b.List(ctx, "nothing/"); len(got) != 0 {
			t.Fatalf("List(nothing/) = %v", got)
		}
		all, _ := b.List(ctx, "")
		if len(all) != len(want)+2 {
			t.Fatalf("List(\"\") returned %d keys, want %d", len(all), len(want)+2)
		}
	})

	t.Run("RejectsBadKeys", func(t *testing.T) {
		b := newBackend(t)
		for _, bad := range []string{"", "../x", "/abs", "a//b", "a/./b", "dir/", "a/.tmp-x"} {
			if err := b.Put(ctx, bad, nil); err == nil {
				t.Fatalf("Put accepted bad key %q", bad)
			}
			if _, err := b.Get(ctx, bad); err == nil || errors.Is(err, store.ErrNotFound) {
				t.Fatalf("Get(%q) = %v, want a key error", bad, err)
			}
		}
	})

	t.Run("ConcurrentPuts", func(t *testing.T) {
		b := newBackend(t)
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- b.Put(ctx, fmt.Sprintf("c/%02d", i), bytes.Repeat([]byte{byte(i)}, 1000))
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			must(t, err)
		}
		keys, err := b.List(ctx, "c/")
		must(t, err)
		if len(keys) != 16 {
			t.Fatalf("%d keys after 16 concurrent puts", len(keys))
		}
	})

	t.Run("StatAndExplicitStandard", func(t *testing.T) {
		b := newBackend(t)
		// Explicit Standard must work everywhere, including services that
		// know no other class.
		must(t, store.PutWith(ctx, b, "s/obj", []byte("12345"), store.PutOptions{Class: "STANDARD"}))
		info, err := store.Stat(ctx, b, "s/obj")
		must(t, err)
		if !info.Readable() || info.Archived {
			t.Fatalf("Standard object reported unreadable: %+v", info)
		}
		if _, ok := b.(store.Statter); ok && info.Size != 5 {
			t.Fatalf("Stat size = %d, want 5", info.Size)
		}
		if _, err := store.Stat(ctx, b, "s/missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Stat missing: %v, want ErrNotFound", err)
		}
	})

	t.Run("CancelledContext", func(t *testing.T) {
		b := newBackend(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		// The local backend finishes quick file operations regardless;
		// remote backends must give up. Either way nothing may panic, and
		// a failed Put must not leave a readable partial object.
		if err := b.Put(cctx, "x/cancelled", []byte("data")); err != nil {
			if got, gerr := b.Get(ctx, "x/cancelled"); gerr == nil && !bytes.Equal(got, []byte("data")) {
				t.Fatalf("partial object after a cancelled Put: %q", got)
			}
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
