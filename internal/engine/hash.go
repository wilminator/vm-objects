// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/wilminator/vm-objects/internal/source"
)

// HashSource reads a whole disk in order and returns its SHA-256, the
// same value sha256sum gives for the raw disk. It is one extra read of the
// disk, so backup only does it when asked (--hash). progress, if set, is
// told the bytes read so far.
func HashSource(ctx context.Context, src source.Source, progress func(done, total int64)) (string, error) {
	h := sha256.New()
	buf := make([]byte, 4<<20)
	size := src.Size()
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n := int64(len(buf))
		if size-off < n {
			n = size - off
		}
		got, err := src.ReadAt(buf[:n], off)
		if int64(got) < n {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
		h.Write(buf[:n])
		off += n
		if progress != nil {
			progress(off, size)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
