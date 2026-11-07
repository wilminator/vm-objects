// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package engine

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wilminator/vm-objects/internal/repo"
)

// A journal holds a staged restore's changes outside the target until the
// result has been checked. It is one file:
//
//	"VMOJ" 0x01, a 16-byte id, then records:
//	kind (1 byte), offset (8), data length (4), sealed length (4), sealed bytes
//
// Each record is sealed with the repository's key (compressed, encrypted
// and authenticated), labelled with the journal id, its sequence number,
// kind, offset and length, so records can't be altered, swapped or
// reordered unnoticed. Kinds: 'H' the header (JSON), 'R' a run of bytes for
// the target at offset, 'C' the commit (JSON), written once the merged
// result checked out. A journal without a commit was never applied.
type journal struct {
	r      *repo.Repo
	f      *os.File
	path   string
	id     string
	seq    int
	end    int64 // file size so far
	header journalHeader
	runs   []journalRun
	byBlk  map[int][]int // block number -> run indexes, in order written
	bs     int64
	commit *journalCommit
	// Free space kept back on the journal's filesystem.
	reserve int64
}

type journalHeader struct {
	VM       string `json:"vm"`
	Manifest string `json:"manifest"`
	Base     string `json:"base,omitempty"`
	DiskSize int64  `json:"disk_size"`
	Target   string `json:"target"`
}

type journalCommit struct {
	Runs   int    `json:"runs"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type journalRun struct {
	seq    int
	off    int64
	length int
	pos    int64 // where the sealed bytes start in the file
	slen   int
}

const (
	journalMagic   = "VMOJ\x01"
	journalHdrLen  = len(journalMagic) + 16
	journalRecHead = 1 + 8 + 4 + 4
)

// ErrJournalFull means the journal's filesystem ran low; the target was
// not touched.
var ErrJournalFull = errors.New("not enough room for the restore journal")

func (j *journal) label(seq int, kind byte, off int64, n int) string {
	return fmt.Sprintf("journal/%s/%d/%c/%d/%d", j.id, seq, kind, off, n)
}

func createJournal(r *repo.Repo, path string, h journalHeader, bs int64) (*journal, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		f.Close()
		return nil, err
	}
	j := &journal{r: r, f: f, path: path, id: hex.EncodeToString(id[:]), header: h, byBlk: map[int][]int{}, bs: bs, reserve: 256 << 20}
	if _, err := f.Write(append([]byte(journalMagic), id[:]...)); err != nil {
		j.close()
		return nil, err
	}
	j.end = int64(journalHdrLen)
	data, _ := json.Marshal(h)
	if err := j.append('H', 0, data); err != nil {
		j.close()
		return nil, err
	}
	return j, nil
}

// openJournal reads an existing journal's records (checking each one's
// seal) and returns it ready to apply or discard.
func openJournal(r *repo.Repo, path string, bs int64) (*journal, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	j := &journal{r: r, f: f, path: path, byBlk: map[int][]int{}, bs: bs, reserve: 256 << 20}
	br := bufio.NewReader(f)
	head := make([]byte, journalHdrLen)
	if _, err := io.ReadFull(br, head); err != nil || string(head[:len(journalMagic)]) != journalMagic {
		j.close()
		return nil, fmt.Errorf("%s is not a vmobj restore journal", path)
	}
	j.id = hex.EncodeToString(head[len(journalMagic):])
	j.end = int64(journalHdrLen)
	rec := make([]byte, journalRecHead)
	for {
		if _, err := io.ReadFull(br, rec); err == io.EOF {
			break
		} else if err != nil {
			// A record cut short by a crash: ignore it and everything after.
			break
		}
		kind := rec[0]
		off := int64(binary.BigEndian.Uint64(rec[1:]))
		n := int(binary.BigEndian.Uint32(rec[9:]))
		slen := int(binary.BigEndian.Uint32(rec[13:]))
		sealed := make([]byte, slen)
		if _, err := io.ReadFull(br, sealed); err != nil {
			break
		}
		data, err := r.OpenLocal(j.label(j.seq, kind, off, n), sealed)
		if err != nil {
			j.close()
			return nil, fmt.Errorf("journal %s: record %d failed its check (damaged, or sealed with another key)", path, j.seq)
		}
		switch kind {
		case 'H':
			if err := json.Unmarshal(data, &j.header); err != nil {
				j.close()
				return nil, fmt.Errorf("journal %s: bad header: %w", path, err)
			}
		case 'R':
			if len(data) != n {
				j.close()
				return nil, fmt.Errorf("journal %s: record %d holds %d bytes, its header says %d", path, j.seq, len(data), n)
			}
			j.addRun(journalRun{seq: j.seq, off: off, length: n, pos: j.end + journalRecHead, slen: slen})
		case 'C':
			var c journalCommit
			if err := json.Unmarshal(data, &c); err != nil {
				j.close()
				return nil, fmt.Errorf("journal %s: bad commit: %w", path, err)
			}
			j.commit = &c
		default:
			j.close()
			return nil, fmt.Errorf("journal %s: unknown record kind %q", path, kind)
		}
		j.seq++
		j.end += int64(journalRecHead + slen)
	}
	if j.header.Manifest == "" {
		j.close()
		return nil, fmt.Errorf("journal %s has no header", path)
	}
	// Drop anything after the last whole record, so new records follow it.
	if err := f.Truncate(j.end); err != nil {
		j.close()
		return nil, err
	}
	return j, nil
}

func (j *journal) addRun(run journalRun) {
	j.runs = append(j.runs, run)
	b := int(run.off / j.bs)
	j.byBlk[b] = append(j.byBlk[b], len(j.runs)-1)
}

// append seals data and adds it as a record, checking for room first.
func (j *journal) append(kind byte, off int64, data []byte) error {
	sealed, err := j.r.SealLocal(j.label(j.seq, kind, off, len(data)), data)
	if err != nil {
		return err
	}
	if free, ok := freeSpace(filepath.Dir(j.path)); ok && free < int64(len(sealed))+j.reserve {
		return fmt.Errorf("%w: %s has %d bytes free", ErrJournalFull, filepath.Dir(j.path), free)
	}
	rec := make([]byte, journalRecHead, journalRecHead+len(sealed))
	rec[0] = kind
	binary.BigEndian.PutUint64(rec[1:], uint64(off))
	binary.BigEndian.PutUint32(rec[9:], uint32(len(data)))
	binary.BigEndian.PutUint32(rec[13:], uint32(len(sealed)))
	rec = append(rec, sealed...)
	if _, err := j.f.WriteAt(rec, j.end); err != nil {
		return err
	}
	if kind == 'R' {
		j.addRun(journalRun{seq: j.seq, off: off, length: len(data), pos: j.end + journalRecHead, slen: len(sealed)})
	}
	j.seq++
	j.end += int64(len(rec))
	return nil
}

// WriteAt adds a run of bytes for the target, so writeDiff can write to a
// journal as to a file.
func (j *journal) WriteAt(p []byte, off int64) (int, error) {
	if err := j.append('R', off, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (j *journal) run(i int) ([]byte, error) {
	run := j.runs[i]
	sealed := make([]byte, run.slen)
	if _, err := j.f.ReadAt(sealed, run.pos); err != nil {
		return nil, err
	}
	return j.r.OpenLocal(j.label(run.seq, 'R', run.off, run.length), sealed)
}

// overlay lays the journal's runs for block b, oldest first, over buf,
// which holds the target's bytes for that block (starting at off).
func (j *journal) overlay(b int, buf []byte, off int64) error {
	for _, i := range j.byBlk[b] {
		data, err := j.run(i)
		if err != nil {
			return err
		}
		ro := j.runs[i].off - off
		if ro < 0 || ro+int64(len(data)) > int64(len(buf)) {
			return fmt.Errorf("journal run at %d lies outside block %d", j.runs[i].off, b)
		}
		copy(buf[ro:], data)
	}
	return nil
}

func (j *journal) bytes() (n int64) {
	for _, r := range j.runs {
		n += int64(r.length)
	}
	return n
}

func (j *journal) commitWith(sha string) error {
	c := journalCommit{Runs: len(j.runs), Bytes: j.bytes(), SHA256: sha}
	data, _ := json.Marshal(c)
	if err := j.f.Sync(); err != nil {
		return err
	}
	if err := j.append('C', 0, data); err != nil {
		return err
	}
	if err := j.f.Sync(); err != nil {
		return err
	}
	j.commit = &c
	return syncDir(filepath.Dir(j.path))
}

func (j *journal) close() {
	if j.f != nil {
		j.f.Close()
		j.f = nil
	}
}

func (j *journal) remove() error {
	j.close()
	if err := os.Remove(j.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(j.path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
