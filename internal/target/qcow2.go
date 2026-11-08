// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package target

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wilminator/vm-objects/internal/source"
)

// QCOW2 writes a new qcow2 image (version 3), optionally as an overlay on
// a backing file: only the clusters written go into it, and the rest reads
// through to the backing file (or as zeros without one). That is how QNAP
// Virtualization Station keeps a snapshot, and why a replayed history
// needs this: each snapshot becomes a small overlay holding just what
// changed.
//
// How the file is laid out. Data clusters are appended as they're first
// written, after the header in cluster 0. Close then appends the L2
// tables, the L1 table, the refcount blocks and the refcount table, and
// writes the header that points at them. Nothing is ever freed, so every
// cluster in the file is used exactly once: refcount 1 everywhere, no
// leaks, which is what qemu-img check wants to see.
//
// It reads as well as writes (ReadAt), so a restore can compare what is
// there. It is not safe for concurrent use.
type QCOW2 struct {
	f         *os.File
	path      string
	size      int64
	cbits     uint
	backing   source.Source // nil without one
	bname     string        // backing file name as stored (relative to this file's folder when possible)
	bformat   string
	l2        map[uint64][]uint64 // L1 index -> L2 entries
	next      int64               // next free host cluster
	closed    bool
	buf       []byte
	l2Bits    uint
	nClusters uint64
}

// QCOW2Options configure CreateQCOW2.
type QCOW2Options struct {
	// Backing is the image under this one, or "" for none. Its format
	// (qcow2 or raw) is recorded in the header, so readers never have to
	// guess it from the contents.
	Backing       string
	BackingFormat string // "qcow2" or "raw"
	// ClusterBits is log2 of the cluster size: 16 (64 KiB, QEMU's
	// default) unless set.
	ClusterBits uint
}

const (
	qcowMagic     = 0x514649fb
	qcowCopied    = uint64(1) << 63 // refcount is exactly 1
	qcowZero      = uint64(1)       // reads as zeros, whatever is below
	qcowOffMask   = 0x00fffffffffffe00
	extBackingFmt = 0xE2792ACA
	refcountOrder = 4 // 16-bit refcounts
	headerLen     = 104
)

// CreateQCOW2 creates path (which must not exist) as an empty image of
// size bytes. With a backing file, the image starts out reading exactly
// as the backing file does.
func CreateQCOW2(path string, size int64, o QCOW2Options) (*QCOW2, error) {
	if size <= 0 || size%512 != 0 {
		// QEMU handles a qcow2 image's size in 512-byte sectors, and a
		// size between two would read differently there than here.
		return nil, fmt.Errorf("qcow2: size %d is not a positive multiple of 512 bytes", size)
	}
	if o.ClusterBits == 0 {
		o.ClusterBits = 16
	}
	if o.ClusterBits < 12 || o.ClusterBits > 21 {
		return nil, fmt.Errorf("qcow2: cluster size 2^%d is out of range", o.ClusterBits)
	}
	q := &QCOW2{path: path, size: size, cbits: o.ClusterBits, l2: map[uint64][]uint64{}, next: 1}
	q.l2Bits = q.cbits - 3
	q.buf = make([]byte, 1<<q.cbits)
	if o.Backing != "" {
		switch o.BackingFormat {
		case "qcow2", "raw":
		default:
			return nil, fmt.Errorf("qcow2: backing format must be qcow2 or raw, not %q", o.BackingFormat)
		}
		b, err := source.Open(o.Backing, o.BackingFormat)
		if err != nil {
			return nil, fmt.Errorf("qcow2: backing file: %w", err)
		}
		if b.Size() > size {
			b.Close()
			return nil, fmt.Errorf("qcow2: backing file %s (%d bytes) is larger than the new image", o.Backing, b.Size())
		}
		q.backing, q.bformat = b, o.BackingFormat
		// Store the name relative to this image when they share a folder,
		// as qemu-img does with -b and a plain name, so the pair can move.
		q.bname = o.Backing
		if filepath.Dir(filepath.Clean(o.Backing)) == filepath.Dir(filepath.Clean(path)) {
			q.bname = filepath.Base(o.Backing)
		}
		if len(q.bname) > 1023 {
			b.Close()
			return nil, errors.New("qcow2: backing file name is too long")
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		if q.backing != nil {
			q.backing.Close()
		}
		return nil, err
	}
	q.f = f
	q.nClusters = uint64((size + int64(len(q.buf)) - 1) >> q.cbits)
	// Cluster 0 is the header; write a placeholder so a crash leaves a file
	// that isn't mistaken for a valid image.
	if _, err := f.WriteAt(q.buf, 0); err != nil {
		q.abort()
		return nil, err
	}
	return q, nil
}

// Size is the image's virtual size.
func (q *QCOW2) Size() int64 { return q.size }

func (q *QCOW2) csize() int64 { return 1 << q.cbits }

func (q *QCOW2) entry(c uint64) uint64 {
	t := q.l2[c>>q.l2Bits]
	if t == nil {
		return 0
	}
	return t[c&(1<<q.l2Bits-1)]
}

func (q *QCOW2) setEntry(c uint64, e uint64) {
	i := c >> q.l2Bits
	t := q.l2[i]
	if t == nil {
		t = make([]uint64, 1<<q.l2Bits)
		q.l2[i] = t
	}
	t[c&(1<<q.l2Bits-1)] = e
}

// ReadAt reads guest bytes: from this image where written, else from the
// backing file, else zeros.
func (q *QCOW2) ReadAt(p []byte, off int64) (int, error) {
	if q.closed {
		return 0, os.ErrClosed
	}
	if off < 0 || off >= q.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < q.size {
		c := uint64(off >> q.cbits)
		in := off & (q.csize() - 1)
		k := min(int64(len(p)-n), q.csize()-in, q.size-off)
		if err := q.readCluster(p[n:n+int(k)], c, in); err != nil {
			return n, err
		}
		n += int(k)
		off += k
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (q *QCOW2) readCluster(p []byte, c uint64, in int64) error {
	e := q.entry(c)
	switch {
	case e&qcowOffMask != 0:
		_, err := q.f.ReadAt(p, int64(e&qcowOffMask)+in)
		return err
	case e&qcowZero != 0 || q.backing == nil:
		clear(p)
		return nil
	}
	return q.readBacking(p, int64(c)<<q.cbits+in)
}

// readBacking reads from the backing file; past its end (it may be
// smaller than this image) the image reads as zeros.
func (q *QCOW2) readBacking(p []byte, off int64) error {
	clear(p)
	bs := q.backing.Size()
	if off >= bs {
		return nil
	}
	end := min(int64(len(p)), bs-off)
	_, err := q.backing.ReadAt(p[:end], off)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return err
}

// WriteAt writes guest bytes. A cluster written for the first time is
// allocated whole: the part not being written is filled from what was
// there (the backing file or zeros), as QEMU does.
func (q *QCOW2) WriteAt(p []byte, off int64) (int, error) {
	if q.closed {
		return 0, os.ErrClosed
	}
	if off < 0 || off+int64(len(p)) > q.size {
		return 0, fmt.Errorf("qcow2: write of %d bytes at %d is outside the %d-byte image", len(p), off, q.size)
	}
	n := 0
	for n < len(p) {
		c := uint64(off >> q.cbits)
		in := off & (q.csize() - 1)
		k := min(int64(len(p)-n), q.csize()-in)
		if err := q.writeCluster(p[n:n+int(k)], c, in); err != nil {
			return n, err
		}
		n += int(k)
		off += k
	}
	return n, nil
}

func (q *QCOW2) writeCluster(p []byte, c uint64, in int64) error {
	e := q.entry(c)
	if e&qcowOffMask != 0 {
		_, err := q.f.WriteAt(p, int64(e&qcowOffMask)+in)
		return err
	}
	whole := in == 0 && int64(len(p)) == q.csize()
	// Zeros over zeros need no cluster: without a backing file an
	// unwritten cluster reads as zeros already, and with one the zero
	// flag says so without any data.
	if isZero(p) {
		if whole && q.backing != nil {
			q.setEntry(c, qcowZero)
			return nil
		}
		if q.backing == nil || e&qcowZero != 0 {
			return nil
		}
	}
	buf := q.buf
	if !whole {
		if err := q.readCluster(buf, c, 0); err != nil {
			return err
		}
	}
	copy(buf[in:], p)
	host := q.next << q.cbits
	if _, err := q.f.WriteAt(buf, host); err != nil {
		return err
	}
	q.next++
	q.setEntry(c, uint64(host)|qcowCopied)
	return nil
}

func isZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// Allocated reports how many guest clusters this image holds data for.
func (q *QCOW2) Allocated() int { return int(q.next - 1) }

// Close writes the tables and header and closes the file. Until it
// returns nil, the file isn't a valid image.
func (q *QCOW2) Close() error {
	if q.closed {
		return nil
	}
	err := q.finish()
	q.closed = true
	if q.backing != nil {
		err = errors.Join(err, q.backing.Close())
	}
	return errors.Join(err, q.f.Close())
}

// abort closes and removes an image that won't be finished.
func (q *QCOW2) abort() {
	q.closed = true
	if q.backing != nil {
		q.backing.Close()
	}
	q.f.Close()
	os.Remove(q.path)
}

// Abort stops without finishing: the file is removed.
func (q *QCOW2) Abort() {
	if !q.closed {
		q.abort()
	}
}

func (q *QCOW2) finish() error {
	cs := q.csize()
	be := binary.BigEndian
	entriesPerL2 := uint64(1) << q.l2Bits
	l1Size := (q.nClusters + entriesPerL2 - 1) / entriesPerL2
	l1 := make([]uint64, l1Size)

	// L2 tables, in L1 order.
	tbl := make([]byte, cs)
	for i := uint64(0); i < l1Size; i++ {
		t := q.l2[i]
		if t == nil {
			continue
		}
		for j, e := range t {
			be.PutUint64(tbl[j*8:], e)
		}
		host := q.next << q.cbits
		if _, err := q.f.WriteAt(tbl, host); err != nil {
			return err
		}
		q.next++
		l1[i] = uint64(host) | qcowCopied
	}

	// The L1 table.
	l1Clusters := int64((l1Size*8 + uint64(cs) - 1) / uint64(cs))
	l1Off := q.next << q.cbits
	l1Bytes := make([]byte, l1Clusters*cs)
	for i, e := range l1 {
		be.PutUint64(l1Bytes[i*8:], e)
	}
	if _, err := q.f.WriteAt(l1Bytes, l1Off); err != nil {
		return err
	}
	q.next += l1Clusters

	// Refcounts: one block covers cs/2 clusters (16-bit counts). The
	// blocks and the table count themselves too, so grow until it fits.
	perBlock := cs / 2
	used := q.next // clusters 0..next-1 are in use so far
	var blocks, tableClusters int64
	for {
		total := used + blocks + tableClusters
		nb := (total + perBlock - 1) / perBlock
		nt := (nb*8 + cs - 1) / cs
		if nb == blocks && nt == tableClusters {
			break
		}
		blocks, tableClusters = nb, nt
	}
	total := used + blocks + tableClusters
	blockOff := used << q.cbits
	tableOff := (used + blocks) << q.cbits
	rc := make([]byte, blocks*cs)
	for c := int64(0); c < total; c++ {
		be.PutUint16(rc[c*2:], 1)
	}
	if _, err := q.f.WriteAt(rc, blockOff); err != nil {
		return err
	}
	rt := make([]byte, tableClusters*cs)
	for b := int64(0); b < blocks; b++ {
		be.PutUint64(rt[b*8:], uint64(blockOff+b*cs))
	}
	if _, err := q.f.WriteAt(rt, tableOff); err != nil {
		return err
	}

	// The header, last.
	h := make([]byte, cs)
	be.PutUint32(h[0:], qcowMagic)
	be.PutUint32(h[4:], 3)
	be.PutUint32(h[20:], uint32(q.cbits))
	be.PutUint64(h[24:], uint64(q.size))
	be.PutUint32(h[36:], uint32(l1Size))
	be.PutUint64(h[40:], uint64(l1Off))
	be.PutUint64(h[48:], uint64(tableOff))
	be.PutUint32(h[56:], uint32(tableClusters))
	be.PutUint32(h[96:], refcountOrder)
	be.PutUint32(h[100:], headerLen)
	pos := headerLen
	if q.backing != nil {
		be.PutUint32(h[pos:], extBackingFmt)
		be.PutUint32(h[pos+4:], uint32(len(q.bformat)))
		copy(h[pos+8:], q.bformat)
		pos += 8 + (len(q.bformat)+7)/8*8
	}
	pos += 8 // end of extensions: type 0, length 0 (already zero)
	if q.backing != nil {
		be.PutUint64(h[8:], uint64(pos))
		be.PutUint32(h[16:], uint32(len(q.bname)))
		copy(h[pos:], q.bname)
	}
	if _, err := q.f.WriteAt(h, 0); err != nil {
		return err
	}
	if err := q.f.Truncate(total << q.cbits); err != nil {
		return err
	}
	return q.f.Sync()
}
