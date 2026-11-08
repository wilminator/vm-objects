// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wilminator/vm-objects/internal/store"
	"github.com/wilminator/vm-objects/internal/store/s3fake"
)

func TestParseS3(t *testing.T) {
	good := map[string]store.S3Location{
		"s3://bkt":                      {Bucket: "bkt"},
		"s3://bkt/":                     {Bucket: "bkt"},
		"s3://bkt/a/b?region=us-west-2": {Bucket: "bkt", Prefix: "a/b", Region: "us-west-2"},
		"s3://bkt?endpoint=http://nas:8010&profile=qnap": {
			Bucket: "bkt", Endpoint: "http://nas:8010", Profile: "qnap", PathStyle: true},
		"s3://bkt?endpoint=https://s3.example&path-style=false": {
			Bucket: "bkt", Endpoint: "https://s3.example"},
		"s3://bkt?profile=p&credentials=/root/backup-secrets/credentials": {
			Bucket: "bkt", Profile: "p", Credentials: "/root/backup-secrets/credentials"},
	}
	for in, want := range good {
		got, err := store.ParseS3(in)
		if err != nil || got != want {
			t.Errorf("ParseS3(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"s3:///nobucket",
		"http://bkt/x",
		"s3://key:secret@bkt",        // credentials don't belong in the address
		"s3://bkt?regoin=us-west-2",  // misspelt option
		"s3://bkt?endpoint=nas:8010", // no scheme
		"s3://bkt?path-style=yes",
		"s3://bkt?credentials=relative/path", // must be absolute
		"s3://bkt/../escape",
	} {
		loc, err := store.ParseS3(bad)
		if err == nil {
			// A bad prefix is caught when the backend is built.
			_, err = store.NewS3(s3fake.New(), loc.Bucket, loc.Prefix)
		}
		if err == nil {
			t.Errorf("ParseS3(%q) accepted it", bad)
		}
	}
}

// The prefix keeps repositories that share a bucket apart.
func TestS3PrefixIsolation(t *testing.T) {
	ctx := context.Background()
	srv := s3fake.New("shared")
	a, _ := store.NewS3(srv, "shared", "host-a")
	b, _ := store.NewS3(srv, "shared", "host-b/")
	if err := a.Put(ctx, "config.json", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Exists(ctx, "config.json"); ok {
		t.Fatal("repository b can see repository a's object")
	}
	if srv.Object("shared", "host-a/config.json") == nil {
		t.Fatal("object not stored under its prefix")
	}
	keys, _ := a.List(ctx, "")
	if len(keys) != 1 || keys[0] != "config.json" {
		t.Fatalf("List = %v; the prefix should be hidden from callers", keys)
	}
}

// Packs are read one extent at a time; that must be a ranged request,
// not a download of the whole pack.
func TestS3GetRangeIsRanged(t *testing.T) {
	ctx := context.Background()
	srv := s3fake.New("b")
	s, _ := store.NewS3(srv, "b", "")
	s.Put(ctx, "packs/c/p.pack", []byte(strings.Repeat("x", 1000)))
	if _, err := s.GetRange(ctx, "packs/c/p.pack", 100, 10); err != nil {
		t.Fatal(err)
	}
	if srv.Calls["GetObject.Range"] != 1 {
		t.Fatalf("GetRange made %d ranged requests, want 1", srv.Calls["GetObject.Range"])
	}
}

func TestS3MissingBucket(t *testing.T) {
	s, _ := store.NewS3(s3fake.New(), "absent", "")
	if err := s.Put(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Fatalf("Put to a missing bucket: %v", err)
	}
}

// A credentials file named in the address is read instead of ~/.aws.
func TestS3CredentialsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "creds")
	if err := os.WriteFile(f, []byte("[p]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = notreal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir) // no ~/.aws to fall back on
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	loc, err := store.ParseS3("s3://bkt?region=us-east-2&profile=p&credentials=" + f)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.NewS3Client(context.Background(), loc)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := c.Options().Credentials.Retrieve(context.Background())
	if err != nil || cr.AccessKeyID != "AKIDEXAMPLE" {
		t.Fatalf("credentials = %q, %v", cr.AccessKeyID, err)
	}
	loc.Credentials = filepath.Join(dir, "missing")
	if _, err := store.NewS3Client(context.Background(), loc); err == nil {
		t.Fatal("a missing credentials file was accepted")
	}
}
