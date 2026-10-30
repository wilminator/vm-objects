// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"context"
	"errors"
	"testing"
)

func TestLocal(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Put(ctx, "blocks/ab/abc", []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get(ctx, "blocks/ab/abc"); string(got) != "hello world" {
		t.Fatalf("Get = %q", got)
	}
	if got, _ := l.GetRange(ctx, "blocks/ab/abc", 6, 5); string(got) != "world" {
		t.Fatalf("GetRange = %q", got)
	}
	if _, err := l.GetRange(ctx, "blocks/ab/abc", 6, 50); err == nil {
		t.Fatal("GetRange past the end should fail")
	}
	if _, err := l.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	keys, _ := l.List(ctx, "blocks/")
	if len(keys) != 1 || keys[0] != "blocks/ab/abc" {
		t.Fatalf("List = %v", keys)
	}
	for _, bad := range []string{"../x", "/abs", "a//b", "a/./b", "dir/"} {
		if err := l.Put(ctx, bad, nil); err == nil {
			t.Fatalf("accepted bad key %q", bad)
		}
	}
	if err := l.Delete(ctx, "blocks/ab/abc"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.Exists(ctx, "blocks/ab/abc"); ok {
		t.Fatal("object still exists after Delete")
	}
}
