// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/store"
)

func setup(t *testing.T) (context.Context, store.Backend, keys.Set) {
	t.Helper()
	k, err := keys.Derive(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return context.Background(), b, k
}

func TestInitOpen(t *testing.T) {
	ctx, b, k := setup(t)
	r, err := Init(ctx, b, k, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, b, k, DefaultConfig()); !errors.Is(err, ErrExists) {
		t.Fatalf("second Init: %v", err)
	}
	r2, err := Open(ctx, b, k)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Config.RepoID != r.Config.RepoID {
		t.Fatal("Open returned a different repository")
	}
}

// Changing any setting in config.json, even with a valid-looking value,
// must be caught by the MAC.
func TestConfigTamperDetected(t *testing.T) {
	ctx, b, k := setup(t)
	if _, err := Init(ctx, b, k, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.BlockSize = 8 << 20 },
		func(c *Config) { c.RepoID = "../../../../tmp" },
		func(c *Config) { c.PackTarget = 2 << 20 },
	} {
		data, _ := b.Get(ctx, configKey)
		var c Config
		json.Unmarshal(data, &c)
		original := data
		change(&c)
		altered, _ := json.Marshal(c)
		b.Put(ctx, configKey, altered)
		if _, err := Open(ctx, b, k); err == nil {
			t.Fatalf("Open accepted an altered config: %s", altered)
		}
		b.Put(ctx, configKey, original)
	}
}

func TestConfigLimits(t *testing.T) {
	ctx, b, k := setup(t)
	for _, c := range []Config{
		{BlockSize: 128 << 20, ExtentSize: 64 << 10, PackTarget: 128 << 20}, // block too big
		{BlockSize: 4 << 20, ExtentSize: 1 << 10, PackTarget: 128 << 20},    // extent too small
		{BlockSize: 4 << 20, ExtentSize: 96 << 10, PackTarget: 128 << 20},   // not a multiple
		{BlockSize: 4 << 20, ExtentSize: 64 << 10, PackTarget: 1 << 10},     // pack too small
	} {
		if _, err := Init(ctx, b, k, c); err == nil {
			t.Fatalf("Init accepted %+v", c)
		}
	}
}

func TestBlockRoundTripAndHashCheck(t *testing.T) {
	ctx, b, k := setup(t)
	r, _ := Init(ctx, b, k, DefaultConfig())
	data := []byte("block contents")
	h := r.Hasher().Sum(data)
	if _, err := r.PutBlock(ctx, h, data); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetBlock(ctx, h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("GetBlock = %q, %v", got, err)
	}
	// Storing one block's object under another block's name must fail.
	other := r.Hasher().Sum([]byte("other"))
	sealed, _ := b.Get(ctx, BlockKey(h))
	b.Put(ctx, BlockKey(other), sealed)
	if _, err := r.GetBlock(ctx, other); err == nil {
		t.Fatal("a block stored under the wrong name was accepted")
	}
}
