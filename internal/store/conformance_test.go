// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Go note: "package store_test" (with _test) is an external test package.
// It can only use store's exported names, like any other caller, and it
// may import storetest, which itself imports store, without a cycle.
package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
	"github.com/wilminator/vm-objects/internal/store/storetest"
)

func TestLocalConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Backend {
		b, err := store.NewLocal(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func TestS3FakeConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Backend {
		b, err := store.NewS3(s3fake.New("bucket"), "bucket", "repos/test")
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

// TestS3Conformance runs the suite against a real S3-compatible service.
// It is skipped unless VMOBJ_TEST_S3 names a bucket, for example:
//
//	VMOBJ_TEST_S3='s3://vmobj-test?endpoint=http://127.0.0.1:9000' go test ./internal/store/
//
// Each run works under a fresh prefix and deletes what it wrote.
func TestS3Conformance(t *testing.T) {
	base := os.Getenv("VMOBJ_TEST_S3")
	if base == "" {
		t.Skip("set VMOBJ_TEST_S3 to run against a real S3 service")
	}
	ctx := context.Background()
	loc, err := store.ParseS3(base)
	if err != nil {
		t.Fatal(err)
	}
	client, err := store.NewS3Client(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	run := time.Now().UTC().Format("20060102T150405.000000000")
	n := 0
	storetest.Run(t, func(t *testing.T) store.Backend {
		n++
		prefix := strings.Trim(loc.Prefix+fmt.Sprintf("/conformance-%s-%d", run, n), "/")
		b, err := store.NewS3(client, loc.Bucket, prefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			keys, _ := b.List(ctx, "")
			for _, k := range keys {
				b.Delete(ctx, k)
			}
		})
		return b
	})
}

func TestS3FakeLocks(t *testing.T) {
	storetest.RunLocks(t, func(t *testing.T) storetest.LockBackend {
		srv := s3fake.New()
		srv.AddLockedBucket("locked")
		b, err := store.NewS3(srv, "locked", "repo")
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

// TestS3Locks runs the lock checks against a real service. VMOBJ_TEST_S3
// must name a bucket created with Object Lock enabled.
func TestS3Locks(t *testing.T) {
	base := os.Getenv("VMOBJ_TEST_S3")
	if base == "" || os.Getenv("VMOBJ_TEST_S3_LOCKS") == "" {
		t.Skip("set VMOBJ_TEST_S3 and VMOBJ_TEST_S3_LOCKS=1 to test Object Lock on a real service")
	}
	ctx := context.Background()
	loc, err := store.ParseS3(base)
	if err != nil {
		t.Fatal(err)
	}
	client, err := store.NewS3Client(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	storetest.RunLocks(t, func(t *testing.T) storetest.LockBackend {
		prefix := strings.Trim(loc.Prefix+"/locks-"+time.Now().UTC().Format("20060102T150405.000000000"), "/")
		b, err := store.NewS3(client, loc.Bucket, prefix)
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func TestS3FakeLockingDisabled(t *testing.T) {
	b, _ := store.NewS3(s3fake.New("plain"), "plain", "")
	if on, err := b.LockingEnabled(context.Background()); on || err != nil {
		t.Fatalf("LockingEnabled on a plain bucket = %v, %v", on, err)
	}
	err := b.PutWith(context.Background(), "x", nil, store.PutOptions{LockUntil: time.Now().Add(time.Hour)})
	if err == nil {
		t.Fatal("a locked put to a bucket without Object Lock succeeded")
	}
}
