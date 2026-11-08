// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
)

// hanging is an S3 service whose connection goes quiet, the way one does
// when its network path disappears: requests (or a download part-way
// through its body) wait until cancelled, and nothing errors by itself.
type hanging struct {
	S3API
	mu        sync.Mutex
	hangGets  int // GetObject calls that never answer
	hangBody  int // GetObject calls whose body stops after a few bytes
	hangPuts  int
	resetGets int // GetObject calls that fail with "connection reset"
	gets      int
}

func (h *hanging) take(n *int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if *n > 0 {
		*n--
		return true
	}
	return false
}

func (h *hanging) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	h.mu.Lock()
	h.gets++
	h.mu.Unlock()
	if h.take(&h.resetGets) {
		return nil, syscall.ECONNRESET
	}
	if h.take(&h.hangGets) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out, err := h.S3API.GetObject(ctx, in, opts...)
	if err != nil || !h.take(&h.hangBody) {
		return out, err
	}
	data, _ := io.ReadAll(out.Body)
	out.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data[:3]), stuck{ctx}))
	return out, nil
}

type stuck struct{ ctx context.Context }

func (s stuck) Read([]byte) (int, error) { <-s.ctx.Done(); return 0, s.ctx.Err() }

func (h *hanging) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if h.take(&h.hangPuts) {
		io.ReadAll(in.Body) // the body goes out, then no reply comes
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return h.S3API.PutObject(ctx, in, opts...)
}

func hangingStore(t *testing.T) (*S3, *hanging) {
	t.Helper()
	old := backoff
	backoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoff = old })
	h := &hanging{S3API: s3fake.New("b")}
	s, err := NewS3(h, "b", "p")
	if err != nil {
		t.Fatal(err)
	}
	s.SetStall(50 * time.Millisecond)
	return s, h
}

// A request that hangs, a download whose body stops, a reset connection
// and an upload with no reply are each cancelled and retried, and the
// operation succeeds.
func TestStallsAreRetried(t *testing.T) {
	ctx := context.Background()
	s, h := hangingStore(t)
	h.hangPuts = 1
	if err := s.Put(ctx, "blocks/a", []byte("hello world")); err != nil {
		t.Fatalf("put after a stall: %v", err)
	}
	for _, c := range []struct {
		name string
		set  *int
	}{{"request", &h.hangGets}, {"body", &h.hangBody}, {"reset", &h.resetGets}} {
		*c.set = 1
		got, err := s.Get(ctx, "blocks/a")
		if err != nil || string(got) != "hello world" {
			t.Fatalf("%s: %q %v", c.name, got, err)
		}
		*c.set = 1
		part, err := s.GetRange(ctx, "blocks/a", 6, 5)
		if err != nil || string(part) != "world" {
			t.Fatalf("%s range: %q %v", c.name, part, err)
		}
	}
}

// A connection that never comes back fails after Attempts tries, saying
// it stalled, instead of hanging for good.
func TestStallGivesUp(t *testing.T) {
	ctx := context.Background()
	s, h := hangingStore(t)
	s.Put(ctx, "blocks/a", []byte("x"))
	h.hangGets = 100
	start := time.Now()
	_, err := s.Get(ctx, "blocks/a")
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("got %v, want ErrStalled", err)
	}
	if h.gets != Attempts {
		t.Fatalf("%d tries, want %d", h.gets, Attempts)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("took %s", took)
	}
}

// Answers aren't retried: not found, and a range past the end.
func TestAnswersAreNotRetried(t *testing.T) {
	ctx := context.Background()
	s, h := hangingStore(t)
	s.Put(ctx, "blocks/a", []byte("hello"))
	if _, err := s.Get(ctx, "blocks/missing"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	if _, err := s.GetRange(ctx, "blocks/a", 3, 10); err != io.ErrUnexpectedEOF {
		t.Fatalf("past the end: %v", err)
	}
	if h.gets != 2 {
		t.Fatalf("%d GETs; answers must not be retried", h.gets)
	}
}

// The caller's own cancellation stops at once, without retries.
func TestCancelIsNotRetried(t *testing.T) {
	s, h := hangingStore(t)
	s.SetStall(time.Hour)
	h.hangGets = 100
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Get(ctx, "blocks/a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if h.gets != 1 {
		t.Fatalf("%d tries after the caller gave up", h.gets)
	}
}

func TestParseStall(t *testing.T) {
	loc, err := ParseS3("s3://b?region=r&stall=90s")
	if err != nil || loc.Stall != 90*time.Second {
		t.Fatalf("%v %v", loc.Stall, err)
	}
	for _, bad := range []string{"s3://b?stall=x", "s3://b?stall=10ms"} {
		if _, err := ParseS3(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
