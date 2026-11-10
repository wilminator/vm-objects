// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package source

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// QCOW2 reads a qcow2 image (QEMU's own disk format, used by QNAP
// Virtualization Station, libvirt and Proxmox) as the guest sees it,
// following its chain of backing files. It is read-only and needs no QEMU
// tools, kernel modules or root.
//
// How qcow2 maps the disk: the guest disk is cut into clusters (64 KiB by
// default). A two-level table, L1 then L2, maps each guest cluster to
// where its data sits in the file. A cluster with no mapping is read from
// the backing file, or is zeros when there is none. A cluster may also be
// stored compressed, or marked as all zeros.
//
// Images the reader doesn't fully understand are refused, never guessed
// at: encryption, external data files, extended L2 entries, and images
// marked corrupt. An image marked dirty (open in a running VM, or not
// closed cleanly) is refused too, because reading it gives an inconsistent
// disk; snapshot the VM first.
type QCOW2 struct {
	f           *os.File
	path        string
	size        int64
	clusterBits uint
	l2Bits      uint
	l1          []uint64
	zstd        bool
	backing     Source // nil when there is none
	backingPath string // the backing file, as opened (absolute, or relative to the working folder)
	backingFmt  string // its recorded format: qcow2 or raw

	mu      sync.Mutex
	l2Cache map[uint64][]uint64 // L2 table offset -> entries
	l2Order []uint64            // for evicting the oldest
}

const (
	qcowMagic = 0x514649fb // "QFI\xfb"

	l1OffsetMask  = 0x00fffffffffffe00 // bits 9-55
	l2OffsetMask  = 0x00fffffffffffe00
	l2Compressed  = 1 << 62
	l2ZeroFlag    = 1 << 0
	l2CacheTables = 64

	featDirty        = 1 << 0
	featCorrupt      = 1 << 1
	featExternalData = 1 << 2
	featCompression  = 1 << 3
	featExtendedL2   = 1 << 4
	featKnown        = featDirty | featCorrupt | featExternalData | featCompression | featExtendedL2

	maxBackingDepth = 64
)

// ErrNotQCOW2 is returned when a file isn't a qcow2 image.
var ErrNotQCOW2 = errors.New("not a qcow2 image")

// IsQCOW2 reports whether the file at path starts with the qcow2 magic.
func IsQCOW2(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var m [4]byte
	if _, err := io.ReadFull(f, m[:]); err != nil {
		return false, nil // too short to be qcow2
	}
	return binary.BigEndian.Uint32(m[:]) == qcowMagic, nil
}

// OpenQCOW2 opens a qcow2 image and its backing chain.
func OpenQCOW2(path string) (*QCOW2, error) { return openQCOW2(path, 0) }

func openQCOW2(path string, depth int) (*QCOW2, error) {
	if depth > maxBackingDepth {
		return nil, fmt.Errorf("%s: backing chain is longer than %d images", path, maxBackingDepth)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	q := &QCOW2{f: f, path: path, l2Cache: map[uint64][]uint64{}}
	if err := q.readHeader(depth); err != nil {
		f.Close()
		if q.backing != nil {
			q.backing.Close()
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return q, nil
}

func (q *QCOW2) readHeader(depth int) error {
	h := make([]byte, 112)
	n, err := q.f.ReadAt(h, 0)
	if n < 72 {
		if err == nil || errors.Is(err, io.EOF) {
			return ErrNotQCOW2
		}
		return err
	}
	be := binary.BigEndian
	if be.Uint32(h[0:]) != qcowMagic {
		return ErrNotQCOW2
	}
	version := be.Uint32(h[4:])
	if version != 2 && version != 3 {
		return fmt.Errorf("qcow2 version %d is not supported", version)
	}
	headerLen := uint32(72) // version 2 headers are fixed at 72 bytes
	backingOff, backingLen := be.Uint64(h[8:]), be.Uint32(h[16:])
	q.clusterBits = uint(be.Uint32(h[20:]))
	q.size = int64(be.Uint64(h[24:]))
	crypt := be.Uint32(h[32:])
	l1Size := be.Uint32(h[36:])
	l1Off := be.Uint64(h[40:])

	if q.clusterBits < 9 || q.clusterBits > 21 {
		return fmt.Errorf("cluster size 2^%d is out of range", q.clusterBits)
	}
	q.l2Bits = q.clusterBits - 3
	if crypt != 0 {
		return errors.New("encrypted qcow2 images are not supported")
	}
	if q.size <= 0 {
		return errors.New("image has no size")
	}

	if version == 3 {
		if n < 104 {
			return errors.New("version 3 header is truncated")
		}
		incompat := be.Uint64(h[72:])
		switch {
		case incompat&^featKnown != 0:
			return fmt.Errorf("image uses qcow2 features this reader doesn't know (0x%x)", incompat&^featKnown)
		case incompat&featCorrupt != 0:
			return errors.New("image is marked corrupt; check it with qemu-img check")
		case incompat&featDirty != 0:
			return errors.New("image is marked dirty: it is open in a running VM or wasn't closed cleanly; back up a snapshot instead")
		case incompat&featExternalData != 0:
			return errors.New("images with an external data file are not supported")
		case incompat&featExtendedL2 != 0:
			return errors.New("images with extended L2 entries (subclusters) are not supported")
		}
		headerLen = be.Uint32(h[100:])
		if incompat&featCompression != 0 {
			if headerLen < 105 || n < 105 {
				return errors.New("compression type is missing from the header")
			}
			switch h[104] {
			case 0:
			case 1:
				q.zstd = true
			default:
				return fmt.Errorf("compression type %d is not supported", h[104])
			}
		}
	}

	// The L1 table must cover the whole disk.
	need := (uint64(q.size) + (1 << (q.clusterBits + q.l2Bits)) - 1) >> (q.clusterBits + q.l2Bits)
	if uint64(l1Size) < need || l1Size > 1<<25 {
		return fmt.Errorf("L1 table has %d entries, need %d", l1Size, need)
	}
	raw := make([]byte, 8*int(l1Size))
	if _, err := q.f.ReadAt(raw, int64(l1Off)); err != nil {
		return fmt.Errorf("reading L1 table: %w", err)
	}
	q.l1 = make([]uint64, l1Size)
	for i := range q.l1 {
		q.l1[i] = be.Uint64(raw[8*i:])
	}

	if backingOff != 0 && backingLen > 0 {
		if backingLen > 1023 {
			return errors.New("backing file name is too long")
		}
		name := make([]byte, backingLen)
		if _, err := q.f.ReadAt(name, int64(backingOff)); err != nil {
			return fmt.Errorf("reading backing file name: %w", err)
		}
		bpath := string(name)
		if !filepath.IsAbs(bpath) {
			bpath = filepath.Join(filepath.Dir(q.path), bpath)
		}
		bformat, err := q.backingFormat(int64(headerLen))
		if err != nil {
			return err
		}
		q.backingPath, q.backingFmt = bpath, bformat
		if q.backing, err = open(bpath, bformat, depth+1); err != nil {
			if errors.Is(err, ErrAmbiguousFormat) {
				return fmt.Errorf("backing file %s has no recorded format; record it (metadata only, no data changes) with: "+
					"qemu-img rebase -u -F qcow2 -b %s %s", bpath, bpath, q.path)
			}
			return fmt.Errorf("backing file: %w", err)
		}
	}
	return nil
}

// backingFormat reads the "backing file format" header extension, which
// records whether the backing file is qcow2 or raw. Images made by
// qemu-img with -F (and by libvirt) have it. Without it the format is
// decided by the backing file's name, never by its contents.
func (q *QCOW2) backingFormat(start int64) (string, error) {
	const extBackingFormat = 0xE2792ACA
	clusterEnd := int64(1) << q.clusterBits
	off := start
	for off+8 <= clusterEnd {
		var hdr [8]byte
		if _, err := q.f.ReadAt(hdr[:], off); err != nil {
			return "", fmt.Errorf("reading header extensions: %w", err)
		}
		typ, length := binary.BigEndian.Uint32(hdr[:4]), int64(binary.BigEndian.Uint32(hdr[4:]))
		if typ == 0 {
			return "", nil
		}
		if length > clusterEnd {
			return "", errors.New("header extension is too long")
		}
		if typ == extBackingFormat {
			name := make([]byte, length)
			if _, err := q.f.ReadAt(name, off+8); err != nil {
				return "", err
			}
			switch f := string(name); f {
			case "raw", "qcow2":
				return f, nil
			default:
				return "", fmt.Errorf("backing file format %q is not supported", f)
			}
		}
		off += 8 + (length+7)&^7
	}
	return "", nil
}

// Size is the guest disk size.
func (q *QCOW2) Size() int64 { return q.size }

// Backing names the image's backing file and its format, or "" when it
// has none.
func (q *QCOW2) Backing() (path, format string) { return q.backingPath, q.backingFmt }

// Layer is one file of a qcow2 backing chain.
type Layer struct {
	Path   string
	Format string // qcow2 or raw
}

// QCOW2Chain lists a qcow2 image's backing chain, the image itself first
// and the bottom-most file last. Only each file's header is read, and the
// dirty flag is ignored: this is how vmobj finds the layer a snapshot froze
// under the live top of a running VM, and that top is dirty for as long as
// the VM runs. Reading data (Open, OpenQCOW2) still refuses a dirty image.
func QCOW2Chain(path string) ([]Layer, error) {
	chain := []Layer{{Path: path, Format: "qcow2"}}
	for cur := path; ; {
		if len(chain) > maxBackingDepth {
			return nil, fmt.Errorf("%s: backing chain is longer than %d images", path, maxBackingDepth)
		}
		bpath, bfmt, err := readBackingInfo(cur)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cur, err)
		}
		if bpath == "" {
			return chain, nil
		}
		if _, err := os.Stat(bpath); err != nil {
			return nil, fmt.Errorf("%s: backing file: %w", cur, err)
		}
		chain = append(chain, Layer{Path: bpath, Format: bfmt})
		switch {
		case bfmt == "raw":
			return chain, nil // a raw file ends the chain
		case bfmt == "" && !hasExt(bpath, ".qcow2", ".qcow"):
			is, err := IsQCOW2(bpath)
			if err != nil {
				return nil, err
			}
			if is {
				return nil, fmt.Errorf("%s: backing file %s has no recorded format: %w", cur, bpath, ErrAmbiguousFormat)
			}
			return chain, nil
		}
		cur = bpath
	}
}

// readBackingInfo reads one qcow2 image's header: its backing file (made
// absolute) and that file's recorded format, both "" when there is none.
func readBackingInfo(path string) (backing, format string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := make([]byte, 112)
	n, rerr := f.ReadAt(h, 0)
	if n < 72 {
		if rerr == nil || errors.Is(rerr, io.EOF) {
			return "", "", ErrNotQCOW2
		}
		return "", "", rerr
	}
	be := binary.BigEndian
	if be.Uint32(h[0:]) != qcowMagic {
		return "", "", ErrNotQCOW2
	}
	version := be.Uint32(h[4:])
	if version != 2 && version != 3 {
		return "", "", fmt.Errorf("qcow2 version %d is not supported", version)
	}
	headerLen := uint32(72)
	if version == 3 {
		if n < 104 {
			return "", "", errors.New("version 3 header is truncated")
		}
		headerLen = be.Uint32(h[100:])
	}
	bits := uint(be.Uint32(h[20:]))
	if bits < 9 || bits > 21 {
		return "", "", fmt.Errorf("cluster size 2^%d is out of range", bits)
	}
	boff, blen := be.Uint64(h[8:]), be.Uint32(h[16:])
	if boff == 0 || blen == 0 {
		return "", "", nil
	}
	if blen > 1023 {
		return "", "", errors.New("backing file name is too long")
	}
	name := make([]byte, blen)
	if _, err := f.ReadAt(name, int64(boff)); err != nil {
		return "", "", fmt.Errorf("reading backing file name: %w", err)
	}
	backing = string(name)
	if !filepath.IsAbs(backing) {
		backing = filepath.Join(filepath.Dir(path), backing)
	}
	q := &QCOW2{f: f, path: path, clusterBits: bits}
	if format, err = q.backingFormat(int64(headerLen)); err != nil {
		return "", "", err
	}
	return backing, format, nil
}

// Close closes the image and its backing chain.
func (q *QCOW2) Close() error {
	err := q.f.Close()
	if q.backing != nil {
		if berr := q.backing.Close(); err == nil {
			err = berr
		}
	}
	return err
}

// ReadAt reads guest bytes, one cluster at a time.
func (q *QCOW2) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	total := 0
	for len(p) > 0 {
		if off >= q.size {
			return total, io.EOF
		}
		csize := int64(1) << q.clusterBits
		within := off & (csize - 1)
		n := min(int64(len(p)), csize-within, q.size-off)
		if err := q.readCluster(p[:n], off); err != nil {
			return total, err
		}
		total += int(n)
		p, off = p[n:], off+n
	}
	return total, nil
}

// readCluster fills p (which lies inside one cluster) from guest offset off.
func (q *QCOW2) readCluster(p []byte, off int64) error {
	entry, err := q.l2Entry(off)
	if err != nil {
		return err
	}
	csize := int64(1) << q.clusterBits
	within := off & (csize - 1)
	switch {
	case entry&l2Compressed != 0:
		data, err := q.decompress(entry)
		if err != nil {
			return fmt.Errorf("%s: compressed cluster at guest offset %d: %w", q.path, off, err)
		}
		copy(p, data[within:])
		return nil
	case entry&l2ZeroFlag != 0:
		clear(p)
		return nil
	case entry&l2OffsetMask == 0:
		// Not allocated here: the backing file has it, or it is zeros.
		if q.backing == nil {
			clear(p)
			return nil
		}
		// A backing file may be smaller than this image; past its end the
		// guest sees zeros.
		bsize := q.backing.Size()
		if off >= bsize {
			clear(p)
			return nil
		}
		n := min(int64(len(p)), bsize-off)
		if _, err := q.backing.ReadAt(p[:n], off); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		clear(p[n:])
		return nil
	default:
		host := int64(entry&l2OffsetMask) + within
		if _, err := q.f.ReadAt(p, host); err != nil {
			return fmt.Errorf("%s: reading cluster at host offset %d: %w", q.path, host, err)
		}
		return nil
	}
}

// l2Entry finds the L2 entry for a guest offset; 0 means unallocated.
func (q *QCOW2) l2Entry(off int64) (uint64, error) {
	l1Index := uint64(off) >> (q.clusterBits + q.l2Bits)
	l2Index := (uint64(off) >> q.clusterBits) & (1<<q.l2Bits - 1)
	if l1Index >= uint64(len(q.l1)) {
		return 0, fmt.Errorf("%s: guest offset %d is beyond the L1 table", q.path, off)
	}
	tableOff := q.l1[l1Index] & l1OffsetMask
	if tableOff == 0 {
		return 0, nil
	}
	table, err := q.l2Table(tableOff)
	if err != nil {
		return 0, err
	}
	return table[l2Index], nil
}

// l2Table reads (and caches) the L2 table at a host offset. The scan
// reads the disk in order, so a small cache hits almost every time.
func (q *QCOW2) l2Table(off uint64) ([]uint64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, ok := q.l2Cache[off]; ok {
		return t, nil
	}
	raw := make([]byte, 1<<q.clusterBits)
	if _, err := q.f.ReadAt(raw, int64(off)); err != nil {
		return nil, fmt.Errorf("%s: reading L2 table at %d: %w", q.path, off, err)
	}
	t := make([]uint64, len(raw)/8)
	for i := range t {
		t[i] = binary.BigEndian.Uint64(raw[8*i:])
	}
	if len(q.l2Order) >= l2CacheTables {
		delete(q.l2Cache, q.l2Order[0])
		q.l2Order = q.l2Order[1:]
	}
	q.l2Cache[off] = t
	q.l2Order = append(q.l2Order, off)
	return t, nil
}

// decompress reads one compressed cluster. Its L2 entry packs the host
// offset in the low bits and, above them, how many extra 512-byte sectors
// the compressed data spans.
func (q *QCOW2) decompress(entry uint64) ([]byte, error) {
	x := 62 - (q.clusterBits - 8)
	hostOff := int64(entry & (1<<x - 1))
	sectors := int64((entry >> x) & (1<<(62-x) - 1))
	length := (sectors+1)*512 - (hostOff & 511)
	if length <= 0 || length > 2<<q.clusterBits {
		return nil, fmt.Errorf("compressed length %d is out of range", length)
	}
	buf := make([]byte, length)
	n, err := q.f.ReadAt(buf, hostOff)
	if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
		return nil, err
	}
	buf = buf[:n] // the last compressed cluster may end before the sector does
	csize := 1 << q.clusterBits
	out := make([]byte, csize)
	if q.zstd {
		d, err := zstd.NewReader(bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		defer d.Close()
		if _, err := io.ReadFull(d, out); err != nil {
			return nil, fmt.Errorf("zstd: cluster does not decompress to its full size: %w", err)
		}
		return out, nil
	}
	// qcow2's default compression is raw deflate (no zlib header).
	r := flate.NewReader(bytes.NewReader(buf))
	defer r.Close()
	// Like QEMU, require a whole cluster: a short result means the image
	// is damaged or truncated, and padding it with zeros would hand back
	// wrong data without an error.
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("deflate: cluster does not decompress to its full size: %w", err)
	}
	return out, nil
}
