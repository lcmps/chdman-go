// Package chdwriter writes CHD v5 files, compressing hunks in parallel.
//
// The layout and the self-reference deduplication follow MAME's
// src/lib/util/chd.cpp (BSD-3-Clause, copyright Aaron Giles and
// contributors); see NOTICE.
package chdwriter

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"runtime"
	"sync"

	"github.com/lcmps/chdman-go/internal/codec"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/pipeline"
)

// Options describe the CHD to write.
type Options struct {
	// Codecs lists the hunk codecs, with unused slots left as CodecNone. All
	// CodecNone writes an uncompressed CHD.
	Codecs       [4]format.CodecTag
	HunkBytes    uint32
	UnitBytes    uint32
	LogicalBytes uint64
	// Metadata is written in order, right after the header.
	Metadata []format.MetadataEntry
	// Workers is the number of compression goroutines; zero means GOMAXPROCS.
	Workers int
	// Progress, if set, is called after each hunk is written with the number
	// of input bytes done and the total.
	Progress func(done, total uint64)
}

// Source supplies the hunks to write.
type Source interface {
	// ReadHunk fills dst, which is exactly HunkBytes long, with hunk index,
	// zero-padded past LogicalBytes. It is called from a single goroutine, in
	// index order.
	ReadHunk(index uint32, dst []byte) error
}

// hunk is one unit of work: the input bytes of a hunk.
type hunk struct {
	index uint32
	buf   *[]byte
}

// stored is the outcome for one hunk, handed to the sink in hunk order.
type stored struct {
	crc  uint16
	sum  [20]byte
	typ  uint8   // codec slot, or format.CompNone
	zero bool    // uncompressed files only: the hunk is all zeros
	buf  *[]byte // holds the bytes to store; released by the sink
	data []byte
}

type dedupeKey struct {
	crc uint16
	sum [20]byte
}

// Write writes a CHD v5 file to f, which must be positioned at its start and
// empty, and returns the raw and overall SHA-1. Both are zero for an
// uncompressed file, which chdman also leaves unhashed.
func Write(ctx context.Context, f io.WriteSeeker, src Source, opts Options) (rawSHA1, sha [20]byte, err error) {
	if err := validate(opts); err != nil {
		return rawSHA1, sha, err
	}
	w := &writer{f: f, src: src, opts: opts}
	return w.run(ctx)
}

func validate(o Options) error {
	switch {
	case o.HunkBytes == 0 || o.HunkBytes > format.MaxHunkBytes:
		return fmt.Errorf("chdwriter: hunk size %d out of range", o.HunkBytes)
	case o.UnitBytes == 0 || o.HunkBytes%o.UnitBytes != 0:
		return fmt.Errorf("chdwriter: hunk size %d is not a multiple of unit size %d", o.HunkBytes, o.UnitBytes)
	case o.LogicalBytes == 0:
		return errors.New("chdwriter: no data to write")
	case (o.LogicalBytes+uint64(o.HunkBytes)-1)/uint64(o.HunkBytes) > 1<<32-1:
		return fmt.Errorf("chdwriter: %d bytes need too many %d byte hunks", o.LogicalBytes, o.HunkBytes)
	}
	for i, c := range o.Codecs {
		if c == format.CodecNone && i+1 < len(o.Codecs) && o.Codecs[i+1] != format.CodecNone {
			return errors.New("chdwriter: codec slots must be filled from the first")
		}
	}
	return nil
}

type writer struct {
	f    io.WriteSeeker
	src  Source
	opts Options

	out *bufio.Writer
	pos uint64 // file offset of the next byte written to out

	rawHash hash.Hash // compressed files only
	pool    sync.Pool // *[]byte of HunkBytes capacity

	// seen maps the identity of each hunk stored so far to its index. Only the
	// sink writes it; workers read it to skip compressing a hunk that an
	// earlier one already holds. The first occurrence of a hunk always reaches
	// the sink before any later copy, so the output does not depend on timing.
	seenMu sync.RWMutex
	seen   map[dedupeKey]uint32
}

func (w *writer) getBuf() *[]byte {
	if b, ok := w.pool.Get().(*[]byte); ok {
		return b
	}
	b := make([]byte, w.opts.HunkBytes)
	return &b
}

func (w *writer) write(b []byte) error {
	n, err := w.out.Write(b)
	w.pos += uint64(n)
	return err
}

func (w *writer) pad(n uint64) error {
	var zeros [4096]byte
	for n > 0 {
		c := min(n, uint64(len(zeros)))
		if err := w.write(zeros[:c]); err != nil {
			return err
		}
		n -= c
	}
	return nil
}

func (w *writer) run(ctx context.Context) (rawSHA1, sha [20]byte, err error) {
	o := w.opts
	compressed := o.Codecs[0] != format.CodecNone
	hunkCount := uint32((o.LogicalBytes + uint64(o.HunkBytes) - 1) / uint64(o.HunkBytes))

	hdr := format.Header{
		Codecs: o.Codecs, LogicalBytes: o.LogicalBytes,
		HunkBytes: o.HunkBytes, UnitBytes: o.UnitBytes,
	}
	if !compressed {
		hdr.MapOffset = format.HeaderSize
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return rawSHA1, sha, err
	}
	w.out = bufio.NewWriterSize(w.f, 1<<20)

	// Placeholder header, the reserved uncompressed map, then the metadata
	// chain; each entry points at the one after it.
	hb := hdr.Marshal()
	if err := w.write(hb[:]); err != nil {
		return rawSHA1, sha, err
	}
	if !compressed {
		if err := w.pad(4 * uint64(hunkCount)); err != nil {
			return rawSHA1, sha, err
		}
	}
	if len(o.Metadata) > 0 {
		hdr.MetaOffset = w.pos
	}
	for i, m := range o.Metadata {
		var next uint64
		raw := format.MarshalMetadata(m, 0)
		if i+1 < len(o.Metadata) {
			next = w.pos + uint64(len(raw))
		}
		binary.BigEndian.PutUint64(raw[8:], next)
		if err := w.write(raw); err != nil {
			return rawSHA1, sha, err
		}
	}
	if !compressed {
		// Stored hunks are hunk-aligned, so their map entries are hunk indices.
		if err := w.pad((uint64(o.HunkBytes) - w.pos%uint64(o.HunkBytes)) % uint64(o.HunkBytes)); err != nil {
			return rawSHA1, sha, err
		}
	} else {
		w.rawHash = sha1.New()
	}
	dataStart := w.pos

	var entries []format.MapEntry // compressed files
	var rawMap []byte             // uncompressed files: 4 bytes per hunk
	w.seen = map[dedupeKey]uint32{}
	if compressed {
		entries = make([]format.MapEntry, 0, hunkCount)
	} else {
		rawMap = make([]byte, 4*int(hunkCount))
	}

	workers := o.Workers
	if workers < 1 {
		workers = runtime.GOMAXPROCS(0)
	}
	var done uint32
	sink := func(s stored) error {
		defer w.pool.Put(s.buf)
		index := done
		done++
		var err error
		if compressed {
			var e format.MapEntry
			w.seenMu.RLock()
			ref, ok := w.seen[dedupeKey{s.crc, s.sum}]
			w.seenMu.RUnlock()
			if ok {
				e = format.MapEntry{Type: format.CompSelf, Offset: uint64(ref)}
			} else {
				e = format.MapEntry{Type: s.typ, CompLen: uint32(len(s.data)), Offset: w.pos, CRC16: s.crc}
				w.seenMu.Lock()
				w.seen[dedupeKey{s.crc, s.sum}] = index
				w.seenMu.Unlock()
				err = w.write(s.data)
			}
			entries = append(entries, e)
		} else if !s.zero {
			binary.BigEndian.PutUint32(rawMap[4*index:], uint32(w.pos/uint64(o.HunkBytes)))
			err = w.write(s.data)
		}
		if err == nil && o.Progress != nil {
			o.Progress(min(uint64(done)*uint64(o.HunkBytes), o.LogicalBytes), o.LogicalBytes)
		}
		return err
	}
	if err := pipeline.Ordered(ctx, workers, w.produce(hunkCount), w.newWorker(compressed), sink); err != nil {
		return rawSHA1, sha, err
	}

	if compressed {
		mapBytes, err := format.EncodeCompressedMap(entries, dataStart)
		if err != nil {
			return rawSHA1, sha, err
		}
		hdr.MapOffset = w.pos
		if err := w.write(mapBytes); err != nil {
			return rawSHA1, sha, err
		}
		rawSHA1 = [20]byte(w.rawHash.Sum(nil))
		sha = format.OverallSHA1(rawSHA1, o.Metadata)
		hdr.RawSHA1, hdr.SHA1 = rawSHA1, sha
	}
	if err := w.out.Flush(); err != nil {
		return rawSHA1, sha, err
	}
	if !compressed {
		if _, err := w.f.Seek(format.HeaderSize, io.SeekStart); err != nil {
			return rawSHA1, sha, err
		}
		if _, err := w.f.Write(rawMap); err != nil {
			return rawSHA1, sha, err
		}
	}
	hb = hdr.Marshal()
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return rawSHA1, sha, err
	}
	if _, err := w.f.Write(hb[:]); err != nil {
		return rawSHA1, sha, err
	}
	_, err = w.f.Seek(0, io.SeekEnd)
	return rawSHA1, sha, err
}

// produce reads the hunks in order, hashing the logical bytes of a compressed
// file as it goes.
func (w *writer) produce(hunkCount uint32) func(yield func(hunk) bool) error {
	o := w.opts
	return func(yield func(hunk) bool) error {
		remaining := o.LogicalBytes
		for i := range hunkCount {
			buf := w.getBuf()
			if err := w.src.ReadHunk(i, *buf); err != nil {
				return fmt.Errorf("chdwriter: reading hunk %d: %w", i, err)
			}
			if w.rawHash != nil {
				w.rawHash.Write((*buf)[:min(remaining, uint64(o.HunkBytes))])
			}
			remaining -= min(remaining, uint64(o.HunkBytes))
			if !yield(hunk{i, buf}) {
				return nil
			}
		}
		return nil
	}
}

// newWorker returns the per-worker constructor. A compressed file's workers
// hash each hunk and pick its best codec; an uncompressed file's only look
// for all-zero hunks, which take no space.
func (w *writer) newWorker(compressed bool) func() (func(hunk) (stored, error), error) {
	return func() (func(hunk) (stored, error), error) {
		if !compressed {
			return func(h hunk) (stored, error) {
				data := *h.buf
				return stored{zero: allZero(data), buf: h.buf, data: data}, nil
			}, nil
		}
		group, err := codec.NewGroup(w.opts.Codecs, w.opts.HunkBytes)
		if err != nil {
			return nil, err
		}
		return func(h hunk) (stored, error) {
			data := *h.buf
			s := stored{crc: format.CRC16(data), sum: sha1.Sum(data)}
			w.seenMu.RLock()
			_, dup := w.seen[dedupeKey{s.crc, s.sum}]
			w.seenMu.RUnlock()
			if dup { // the sink will store a reference, not these bytes
				s.buf = h.buf
				return s, nil
			}
			typ, best, err := group.Best(data)
			if err != nil {
				return stored{}, err
			}
			s.typ = typ
			if typ == format.CompNone {
				s.buf, s.data = h.buf, data
				return s, nil
			}
			// The group owns best; copy it out and free the input buffer.
			s.buf = w.getBuf()
			s.data = (*s.buf)[:copy(*s.buf, best)]
			w.pool.Put(h.buf)
			return s, nil
		}, nil
	}
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
