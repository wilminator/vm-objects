// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/policy"
	"github.com/wilminator/vm-objects/internal/repo"
	"github.com/wilminator/vm-objects/internal/store"
)

// slowBackend makes every block upload take a while, as one to a remote
// service does, and records how many were in flight at once.
type slowBackend struct {
	store.Backend
	delay   time.Duration
	failKey string

	mu       sync.Mutex
	inFlight int
	most     int
}

func (s *slowBackend) Put(ctx context.Context, key string, data []byte) error {
	if !strings.HasPrefix(key, "blocks/") {
		return s.Backend.Put(ctx, key, data)
	}
	s.mu.Lock()
	s.inFlight++
	s.most = max(s.most, s.inFlight)
	fail := s.failKey != "" && strings.HasSuffix(key, s.failKey)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.inFlight--; s.mu.Unlock() }()
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if fail {
		return errors.New("simulated upload failure")
	}
	return s.Backend.Put(ctx, key, data)
}

func slowDest(t *testing.T, keyByte byte, delay time.Duration) (*repo.Repo, *slowBackend) {
	t.Helper()
	inner, err := store.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sb := &slowBackend{Backend: inner, delay: delay}
	k, _ := keys.Derive([]byte(strings.Repeat(string(rune(keyByte)), 32)))
	r, err := repo.Init(context.Background(), sb, k, repo.Config{BlockSize: 64 << 10, ExtentSize: 4 << 10, PackTarget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return r, sb
}

// Uploads overlap, up to the limit, and the copy is still exact; with the
// same key and with another key (blocks re-encrypted).
func TestCopyUploadsOverlap(t *testing.T) {
	for _, keyByte := range []byte{7, 9} {
		h := newHarness(t)
		disk := make([]byte, 2<<20) // 32 blocks
		rand.New(rand.NewSource(31)).Read(disk)
		h.write(disk)
		name := h.backup(manifest.Full).Manifest
		to, sb := slowDest(t, keyByte, 20*time.Millisecond)
		prof, _ := policy.Lookup("standard")
		st := &CopyStats{}
		start := time.Now()
		err := CopyManifest(h.ctx, CopyOptions{From: h.repo, To: to, Profile: &prof, Uploads: 8, Now: func() time.Time { return h.clock }}, "win11", name, st)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		if st.BlocksCopied != 32 {
			t.Fatalf("key %d: copied %d blocks", keyByte, st.BlocksCopied)
		}
		if sb.most < 4 || sb.most > 8 {
			t.Fatalf("key %d: at most %d uploads at once; want several, never more than 8", keyByte, sb.most)
		}
		// One at a time would take 32 x 20 ms.
		if took > 400*time.Millisecond {
			t.Fatalf("key %d: took %s; uploads don't seem to overlap", keyByte, took)
		}
		restoreFrom(t, h, to, "win11", name, disk)
	}
}

// One failed upload stops the copy, and no manifest is written, so the
// destination never lists a backup it doesn't fully hold.
func TestCopyUploadFailureWritesNoManifest(t *testing.T) {
	h := newHarness(t)
	disk := make([]byte, 1<<20)
	rand.New(rand.NewSource(32)).Read(disk)
	h.write(disk)
	name := h.backup(manifest.Full).Manifest
	m, _ := h.repo.GetManifest(h.ctx, "win11", name)
	to, sb := slowDest(t, 7, 5*time.Millisecond)
	sb.failKey = m.Blocks[9].Hash.String()
	prof, _ := policy.Lookup("standard")
	err := CopyManifest(h.ctx, CopyOptions{From: h.repo, To: to, Profile: &prof, Now: func() time.Time { return h.clock }}, "win11", name, &CopyStats{})
	if err == nil {
		t.Fatal("a failed upload was not reported")
	}
	if ok, _ := to.Backend.Exists(h.ctx, repo.ManifestKey("win11", name)); ok {
		t.Fatal("a manifest was written after a failed upload")
	}
	// Running it again finishes the job.
	sb.failKey = ""
	if err := CopyManifest(h.ctx, CopyOptions{From: h.repo, To: to, Profile: &prof, Now: func() time.Time { return h.clock }}, "win11", name, &CopyStats{}); err != nil {
		t.Fatal(err)
	}
	restoreFrom(t, h, to, "win11", name, disk)
}
