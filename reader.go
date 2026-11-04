package chdman

// Hunk decoding and map resolution follow read_hunk in MAME's
// src/lib/util/chd.cpp (BSD-3-Clause, copyright Aaron Giles and contributors);
// see NOTICE.

import (
	"container/list"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/codec"
	"github.com/lcmps/chdman-go/internal/format"
)

// DiscType is the kind of media a CHD holds, taken from its metadata tags.
type DiscType int

// Disc types.
const (
	DiscUnknown DiscType = iota
	DiscCD
	DiscDVD
	DiscGD
	DiscHD
)

func (d DiscType) String() string {
	switch d {
	case DiscCD:
		return "CD"
	case DiscDVD:
		return "DVD"
	case DiscGD:
		return "GD"
	case DiscHD:
		return "HD"
	}
	return "unknown"
}

// description names the kind of CHD for an error message, with its article.
func (d DiscType) description() string {
	switch d {
	case DiscCD:
		return "a CD CHD"
	case DiscDVD:
		return "a DVD CHD"
	case DiscGD:
		return "a GD-ROM CHD"
	case DiscHD:
		return "a hard disk CHD"
	}
	return "a CHD of unknown type"
}

// MetadataInfo is one metadata record of a CHD.
type MetadataInfo struct {
	Tag      string // four characters, such as "CHT2"
	Checksum bool   // the record contributes to the overall SHA-1
	Data     []byte
}

// Info describes an open CHD.
type Info struct {
	Version uint32
	// Codecs lists the configured codec tags, or "none" for an uncompressed file.
	Codecs       []string
	LogicalBytes uint64
	HunkBytes    uint32
	UnitBytes    uint32
	HunkCount    uint32
	// CompressedBytes is the total size of the stored hunk data. Hunks that
	// refer to an earlier identical hunk add nothing.
	CompressedBytes int64
	// RawSHA1 and SHA1 are the stored hashes in hex, or all zeros when the
	// file stores none (uncompressed CHDs).
	RawSHA1, SHA1 string
	Metadata      []MetadataInfo
	Disc          DiscType
}

const (
	cacheBytes   = 16 << 20 // decoded hunks kept in memory
	cacheMinKeep = 4
	cacheMaxKeep = 1 << 12 // bounds the entry count when hunks are tiny
)

// Reader gives random access to the logical content of a CHD and verifies it.
// A Reader is safe for concurrent use.
type Reader struct {
	src    io.ReaderAt
	closer io.Closer

	hdr     format.Header
	entries *format.Map
	meta    []format.MetadataEntry
	info    Info

	decoders [4]sync.Pool // codec.Decompressor per map slot
	stored   sync.Pool    // *[]byte scratch for compressed hunk data

	tocOnce sync.Once // guards toc and tocErr, filled by cdTOC
	toc     cdrom.TOC
	tocErr  error

	mu    sync.Mutex
	lru   *list.List // of *cachedHunk, most recent first
	cache map[uint32]*list.Element
	keep  int
}

type cachedHunk struct {
	n    uint32
	data []byte
}

// Open opens the CHD at path.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := NewReader(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	r.closer = f
	return r, nil
}

// NewReader reads the CHD held in the first size bytes of src. The caller keeps
// ownership of src; Close does not close it.
func NewReader(src io.ReaderAt, size int64) (*Reader, error) {
	if size < format.HeaderSize {
		return nil, fmt.Errorf("%w: %d bytes is too short for a header", chderr.CorruptData, size)
	}
	var raw [format.HeaderSize]byte
	if err := format.ReadFullAt(src, raw[:], 0); err != nil {
		return nil, format.ReadError("reading header", err)
	}
	hdr, err := format.ParseHeader(raw[:])
	if err != nil {
		return nil, err
	}
	entries, err := format.ReadMap(src, hdr, size)
	if err != nil {
		return nil, err
	}
	meta, err := format.ReadMetadata(src, hdr, size)
	if err != nil {
		return nil, err
	}
	r := &Reader{
		src: src, hdr: hdr, entries: entries, meta: meta,
		lru: list.New(), cache: map[uint32]*list.Element{},
		keep: min(cacheMaxKeep, max(cacheMinKeep, cacheBytes/int(hdr.HunkBytes))),
	}
	// Fail early on codecs this library lacks.
	for _, tag := range hdr.Codecs {
		if tag == format.CodecNone {
			continue
		}
		if _, err := codec.NewDecompressor(tag, hdr.HunkBytes); err != nil {
			return nil, err
		}
	}
	r.info = buildInfo(hdr, entries, meta)
	return r, nil
}

func buildInfo(h format.Header, entries *format.Map, meta []format.MetadataEntry) Info {
	info := Info{
		Version:      5,
		LogicalBytes: h.LogicalBytes,
		HunkBytes:    h.HunkBytes,
		UnitBytes:    h.UnitBytes,
		HunkCount:    h.HunkCount(),
		RawSHA1:      hex.EncodeToString(h.RawSHA1[:]),
		SHA1:         hex.EncodeToString(h.SHA1[:]),
	}
	if !h.Compressed() {
		info.Codecs = []string{"none"}
	}
	for _, c := range h.Codecs {
		if c != format.CodecNone {
			info.Codecs = append(info.Codecs, c.String())
		}
	}
	info.CompressedBytes = entries.StoredBytes()
	for _, m := range meta {
		info.Metadata = append(info.Metadata, MetadataInfo{
			Tag: format.CodecTag(m.Tag).String(), Checksum: m.Flags&format.FlagChecksum != 0, Data: m.Data,
		})
		if info.Disc == DiscUnknown {
			info.Disc = discType(m.Tag)
		}
	}
	return info
}

func discType(tag uint32) DiscType {
	switch tag {
	case format.TagCHT2, format.TagCHTR:
		return DiscCD
	case format.TagDVD:
		return DiscDVD
	case format.TagCHGD, format.TagCHGT:
		return DiscGD
	case format.TagGDDD:
		return DiscHD
	}
	return DiscUnknown
}

// Info describes the CHD. The slices in the result are copies, so changing
// them does not affect the Reader.
func (r *Reader) Info() Info {
	info := r.info
	info.Codecs = slices.Clone(info.Codecs)
	info.Metadata = slices.Clone(info.Metadata)
	for i := range info.Metadata {
		info.Metadata[i].Data = slices.Clone(info.Metadata[i].Data)
	}
	return info
}

// Close releases the file opened by Open. It does nothing for a Reader made
// by NewReader.
func (r *Reader) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// ReadAt reads logical bytes at off, as io.ReaderAt does. A hunk whose
// content does not match its stored CRC-16 yields an error wrapping
// ErrCorruptData.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("chdman: negative offset")
	}
	total := r.hdr.LogicalBytes
	n := 0
	for n < len(p) && uint64(off) < total {
		hunk := uint32(uint64(off) / uint64(r.hdr.HunkBytes))
		data, err := r.cachedHunk(hunk)
		if err != nil {
			return n, err
		}
		in := uint64(off) % uint64(r.hdr.HunkBytes)
		// Compare as uint64: total-off can exceed an int on 32-bit builds.
		end := min(uint64(len(data)), in+(total-uint64(off)))
		c := copy(p[n:], data[in:end])
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// cachedHunk returns the decoded hunk n. The result is shared and must not
// be modified.
func (r *Reader) cachedHunk(n uint32) ([]byte, error) {
	r.mu.Lock()
	if el, ok := r.cache[n]; ok {
		r.lru.MoveToFront(el)
		data := el.Value.(*cachedHunk).data
		r.mu.Unlock()
		return data, nil
	}
	r.mu.Unlock()

	data := make([]byte, r.hdr.HunkBytes)
	if err := r.decodeHunk(n, data); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.cache[n]; ok { // another reader got there first
		r.lru.MoveToFront(el)
		return el.Value.(*cachedHunk).data, nil
	}
	r.cache[n] = r.lru.PushFront(&cachedHunk{n, data})
	for r.lru.Len() > r.keep {
		old := r.lru.Remove(r.lru.Back()).(*cachedHunk)
		delete(r.cache, old.n)
	}
	return data, nil
}

// decodeHunk fills dst (HunkBytes long) with the content of hunk n, like
// read_hunk in chd.cpp.
func (r *Reader) decodeHunk(n uint32, dst []byte) error {
	e := r.entries.Entry(n)
	if e.Type == format.CompSelf { // the map resolves chains: this is the data
		n = uint32(e.Offset)
		e = r.entries.Entry(n)
	}
	switch e.Type {
	case format.CompType0, format.CompType1, format.CompType2, format.CompType3:
		if err := r.decompress(e, dst); err != nil {
			return fmt.Errorf("hunk %d: %w", n, err)
		}
	case format.CompNone:
		if e.CompLen == 0 { // all-zero hunk with no stored data
			clear(dst)
			return nil
		}
		if err := format.ReadFullAt(r.src, dst, int64(e.Offset)); err != nil {
			return format.ReadError(fmt.Sprintf("hunk %d", n), err)
		}
	default: // CompParent: parents are not supported
		return fmt.Errorf("%w: hunk %d has map type %d", chderr.CorruptData, n, e.Type)
	}
	// Only compressed files store a CRC-16 for each hunk.
	if r.hdr.Compressed() {
		if crc := format.CRC16(dst); crc != e.CRC16 {
			return fmt.Errorf("%w: hunk %d CRC-16 %04x, want %04x", chderr.CorruptData, n, crc, e.CRC16)
		}
	}
	return nil
}

func (r *Reader) decompress(e format.MapEntry, dst []byte) error {
	buf, _ := r.stored.Get().(*[]byte)
	if buf == nil {
		buf = new([]byte)
	}
	defer r.stored.Put(buf)
	if cap(*buf) < int(e.CompLen) {
		*buf = make([]byte, e.CompLen)
	}
	src := (*buf)[:e.CompLen]
	if err := format.ReadFullAt(r.src, src, int64(e.Offset)); err != nil {
		return format.ReadError("reading stored data", err)
	}

	pool := &r.decoders[e.Type]
	d, _ := pool.Get().(codec.Decompressor)
	if d == nil {
		var err error
		if d, err = codec.NewDecompressor(r.hdr.Codecs[e.Type], r.hdr.HunkBytes); err != nil {
			return err
		}
	}
	err := d.Decompress(dst, src)
	if err != nil {
		if errors.Is(err, chderr.UnsupportedCodec) {
			return err
		}
		if !errors.Is(err, chderr.CorruptData) {
			err = fmt.Errorf("%w: %v", chderr.CorruptData, err)
		}
		return err // a decoder that failed may be in an odd state: drop it
	}
	pool.Put(d)
	return nil
}
