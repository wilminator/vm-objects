// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package seal compresses and encrypts every object vmobj stores.
//
// Layout of a sealed object:
//
//	"VMOS" | version (1 byte) | flags (1 byte) | nonce (12 bytes) | ciphertext + 16-byte tag
//
// The cipher is AES-256-GCM with a fresh random nonce per object. The
// header and a caller-supplied label (the object's content ID or name) are
// bound in as additional authenticated data, so an object can't be swapped
// for another one without the decryption failing.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

const (
	magic     = "VMOS"
	version   = 1
	flagZstd  = 1 << 0
	headerLen = 4 + 1 + 1
	nonceLen  = 12
	// Overhead is the bytes added to every object besides compression.
	Overhead = headerLen + nonceLen + 16
)

// ErrCorrupt means an object failed authentication: damaged, tampered
// with, sealed under a different key, or stored under the wrong name.
var ErrCorrupt = errors.New("object failed authentication")

// Sealer seals and opens objects with one key. It is safe for concurrent use.
type Sealer struct {
	aead cipher.AEAD
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// New creates a Sealer for a 32-byte key.
func New(key [32]byte) (*Sealer, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// EncodeAll and DecodeAll on these are safe to call from many goroutines.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, enc: enc, dec: dec}, nil
}

// Seal compresses plaintext (when that helps) and encrypts it. label is
// authenticated but not stored; Open must be given the same label.
func (s *Sealer) Seal(label, plaintext []byte) ([]byte, error) {
	body := plaintext
	var flags byte
	if c := s.enc.EncodeAll(plaintext, nil); len(c) < len(plaintext) {
		body, flags = c, flagZstd
	}
	out := make([]byte, headerLen+nonceLen, headerLen+nonceLen+len(body)+s.aead.Overhead())
	copy(out, magic)
	out[4] = version
	out[5] = flags
	nonce := out[headerLen : headerLen+nonceLen]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(out, nonce, body, aad(out[:headerLen], label)), nil
}

// Open authenticates, decrypts and decompresses an object.
func (s *Sealer) Open(label, sealed []byte) ([]byte, error) {
	if len(sealed) < Overhead || string(sealed[:4]) != magic {
		return nil, fmt.Errorf("%w: not a vmobj object", ErrCorrupt)
	}
	if sealed[4] != version {
		return nil, fmt.Errorf("object format version %d is not supported", sealed[4])
	}
	header := sealed[:headerLen]
	nonce := sealed[headerLen : headerLen+nonceLen]
	body, err := s.aead.Open(nil, nonce, sealed[headerLen+nonceLen:], aad(header, label))
	if err != nil {
		return nil, ErrCorrupt
	}
	if sealed[5]&flagZstd != 0 {
		plain, err := s.dec.DecodeAll(body, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: decompression failed", ErrCorrupt)
		}
		return plain, nil
	}
	return body, nil
}

func aad(header, label []byte) []byte {
	out := make([]byte, 0, len(header)+len(label))
	return append(append(out, header...), label...)
}
