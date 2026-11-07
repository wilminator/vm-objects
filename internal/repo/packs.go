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
	"fmt"
	"strings"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/store"
)

// Packs hold the small extents of incremental and differential runs. Each
// extent is sealed on its own and appended to the pack, so a restore can
// fetch just the byte range it needs. A small table of contents (TOC) per
// pack lists what is inside; the TOCs alone are enough to rebuild the
// index without reading any pack data.

// TOCEntry locates one sealed extent inside a pack.
type TOCEntry struct {
	Hash   extent.Hash `json:"h"`
	Offset int64       `json:"o"`
	Length int64       `json:"l"`
}

// Location is where an extent's sealed bytes live.
type Location struct {
	Pack   string // object key of the pack
	Offset int64
	Length int64
}

// PackKey and TOCKey name a pack and its table of contents.
func PackKey(chain, id string) string { return "packs/" + chain + "/" + id + ".pack" }

// TOCKey names a pack's table of contents.
func TOCKey(chain, id string) string { return "packs/" + chain + "/" + id + ".toc" }

// PackWriter collects extents and writes them out as packs.
type PackWriter struct {
	repo    *Repo
	chain   string
	opts    store.PutOptions // for packs; TOCs always use the default class
	buf     bytes.Buffer
	entries []TOCEntry
	// Written counts sealed bytes stored, Packs counts packs written.
	Written int64
	Packs   int
	// Index gains an entry for every extent written, so the caller can
	// find extents from this run without reloading TOCs.
	Index map[extent.Hash]Location
}

// NewPackWriter starts writing packs for a chain. Packs are stored with
// opts; their TOCs go to the default (Standard) class, because the next
// incremental and every restore plan must read them without waiting.
func (r *Repo) NewPackWriter(chain string, opts store.PutOptions) *PackWriter {
	return &PackWriter{repo: r, chain: chain, opts: opts, Index: map[extent.Hash]Location{}}
}

// Add seals one extent into the current pack, writing the pack out when it
// reaches the repository's target size.
func (w *PackWriter) Add(ctx context.Context, h extent.Hash, data []byte) error {
	sealed, err := w.repo.sealer.Seal(h[:], data)
	if err != nil {
		return err
	}
	w.entries = append(w.entries, TOCEntry{Hash: h, Offset: int64(w.buf.Len()), Length: int64(len(sealed))})
	w.buf.Write(sealed)
	if int64(w.buf.Len()) >= w.repo.Config.PackTarget {
		return w.Flush(ctx)
	}
	return nil
}

// Flush writes the current pack, then its TOC. The TOC goes second: a TOC
// only ever names a pack that is already stored.
func (w *PackWriter) Flush(ctx context.Context) error {
	if len(w.entries) == 0 {
		return nil
	}
	id := NewID()
	packKey := PackKey(w.chain, id)
	if err := store.PutWith(ctx, w.repo.Backend, packKey, w.buf.Bytes(), w.opts); err != nil {
		return err
	}
	toc, err := json.Marshal(w.entries)
	if err != nil {
		return err
	}
	tocKey := TOCKey(w.chain, id)
	sealedTOC, err := w.repo.sealer.Seal([]byte(tocKey), toc)
	if err != nil {
		return err
	}
	if err := w.repo.Backend.Put(ctx, tocKey, sealedTOC); err != nil {
		return err
	}
	for _, e := range w.entries {
		w.Index[e.Hash] = Location{Pack: packKey, Offset: e.Offset, Length: e.Length}
	}
	w.Written += int64(w.buf.Len()) + int64(len(sealedTOC))
	w.Packs++
	w.buf.Reset()
	w.entries = nil
	return nil
}

// ChainIndex reads every TOC in a chain and maps extent hashes to their
// locations.
func (r *Repo) ChainIndex(ctx context.Context, chain string) (map[extent.Hash]Location, error) {
	keys, err := r.Backend.List(ctx, "packs/"+chain+"/")
	if err != nil {
		return nil, err
	}
	idx := map[extent.Hash]Location{}
	for _, k := range keys {
		if !strings.HasSuffix(k, ".toc") {
			continue
		}
		sealed, err := r.Backend.Get(ctx, k)
		if err != nil {
			return nil, err
		}
		data, err := r.sealer.Open([]byte(k), sealed)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		var entries []TOCEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		packKey := strings.TrimSuffix(k, ".toc") + ".pack"
		for _, e := range entries {
			idx[e.Hash] = Location{Pack: packKey, Offset: e.Offset, Length: e.Length}
		}
	}
	return idx, nil
}

// GetExtent fetches one extent from a pack and checks its hash.
func (r *Repo) GetExtent(ctx context.Context, loc Location, h extent.Hash) ([]byte, error) {
	sealed, err := r.Backend.GetRange(ctx, loc.Pack, loc.Offset, loc.Length)
	if err != nil {
		return nil, fmt.Errorf("extent %s in %s: %w", h, loc.Pack, err)
	}
	return r.openContent(h, sealed)
}
