// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package seal

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func newSealer(t *testing.T, b byte) *Sealer {
	t.Helper()
	var key [32]byte
	key[0] = b
	s, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := newSealer(t, 1)
	random := make([]byte, 5000)
	rand.Read(random)
	for name, plain := range map[string][]byte{
		"compressible":   bytes.Repeat([]byte("vmobj "), 2000),
		"incompressible": random,
		"empty":          {},
	} {
		sealed, err := s.Seal([]byte("label"), plain)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Open([]byte("label"), sealed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("%s: round trip changed the data", name)
		}
	}
}

func TestTamperWrongLabelWrongKey(t *testing.T) {
	s := newSealer(t, 1)
	sealed, _ := s.Seal([]byte("a"), []byte("secret contents"))

	flipped := bytes.Clone(sealed)
	flipped[len(flipped)-1] ^= 1
	if _, err := s.Open([]byte("a"), flipped); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered object: got %v", err)
	}
	if _, err := s.Open([]byte("b"), sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong label: got %v", err)
	}
	if _, err := newSealer(t, 2).Open([]byte("a"), sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key: got %v", err)
	}
}
