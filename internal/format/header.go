// Package format holds the CHD v5 on-disk structures.
//
// Layout and parsing follow MAME's src/lib/util/chd.h and chd.cpp
// (BSD-3-Clause, copyright Aaron Giles and contributors); see NOTICE.
package format

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/lcmps/chdman-go/internal/chderr"
)

// HeaderSize is the length of a v5 header in bytes.
const HeaderSize = 124

const (
	magic   = "MComprHD"
	version = 5
)

// CodecTag is a four-character codec identifier stored big-endian.
type CodecTag uint32

// Codec tags, from chdcodec.h.
const (
	CodecNone    CodecTag = 0
	CodecZlib             = CodecTag('z')<<24 | CodecTag('l')<<16 | CodecTag('i')<<8 | CodecTag('b')
	CodecZstd             = CodecTag('z')<<24 | CodecTag('s')<<16 | CodecTag('t')<<8 | CodecTag('d')
	CodecLZMA             = CodecTag('l')<<24 | CodecTag('z')<<16 | CodecTag('m')<<8 | CodecTag('a')
	CodecHuffman          = CodecTag('h')<<24 | CodecTag('u')<<16 | CodecTag('f')<<8 | CodecTag('f')
	CodecFLAC             = CodecTag('f')<<24 | CodecTag('l')<<16 | CodecTag('a')<<8 | CodecTag('c')
	CodecCDZlib           = CodecTag('c')<<24 | CodecTag('d')<<16 | CodecTag('z')<<8 | CodecTag('l')
	CodecCDZstd           = CodecTag('c')<<24 | CodecTag('d')<<16 | CodecTag('z')<<8 | CodecTag('s')
	CodecCDLZMA           = CodecTag('c')<<24 | CodecTag('d')<<16 | CodecTag('l')<<8 | CodecTag('z')
	CodecCDFLAC           = CodecTag('c')<<24 | CodecTag('d')<<16 | CodecTag('f')<<8 | CodecTag('l')
	CodecAVHuff           = CodecTag('a')<<24 | CodecTag('v')<<16 | CodecTag('h')<<8 | CodecTag('u')
)

// String returns the four-character tag, or "none" for CodecNone.
func (c CodecTag) String() string {
	if c == CodecNone {
		return "none"
	}
	return string([]byte{byte(c >> 24), byte(c >> 16), byte(c >> 8), byte(c)})
}

// MaxHunkBytes is the largest hunk size chdman will create (HUNK_SIZE_MAX).
const MaxHunkBytes = 1024 * 1024

// Header is a decoded CHD v5 header.
type Header struct {
	Codecs       [4]CodecTag
	LogicalBytes uint64
	MapOffset    uint64
	MetaOffset   uint64
	HunkBytes    uint32
	UnitBytes    uint32
	RawSHA1      [20]byte
	SHA1         [20]byte
	ParentSHA1   [20]byte
}

// Compressed reports whether the file uses compression (and a compressed map).
func (h Header) Compressed() bool { return h.Codecs[0] != CodecNone }

// HunkCount returns the number of hunks needed to hold LogicalBytes.
func (h Header) HunkCount() uint32 {
	if h.HunkBytes == 0 {
		return 0
	}
	return uint32((h.LogicalBytes + uint64(h.HunkBytes) - 1) / uint64(h.HunkBytes))
}

// ParseHeader decodes a v5 header from b, which must hold at least HeaderSize
// bytes. Errors wrap chderr.CorruptData, chderr.UnsupportedVersion or
// chderr.ParentRequired.
func ParseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < 16 || string(b[:8]) != magic {
		return h, fmt.Errorf("%w: bad header magic", chderr.CorruptData)
	}
	if v := binary.BigEndian.Uint32(b[12:]); v != version {
		return h, fmt.Errorf("%w: version %d", chderr.UnsupportedVersion, v)
	}
	if len(b) < HeaderSize || binary.BigEndian.Uint32(b[8:]) != HeaderSize {
		return h, fmt.Errorf("%w: bad header length", chderr.CorruptData)
	}
	for i := range h.Codecs {
		h.Codecs[i] = CodecTag(binary.BigEndian.Uint32(b[16+4*i:]))
	}
	h.LogicalBytes = binary.BigEndian.Uint64(b[32:])
	h.MapOffset = binary.BigEndian.Uint64(b[40:])
	h.MetaOffset = binary.BigEndian.Uint64(b[48:])
	h.HunkBytes = binary.BigEndian.Uint32(b[56:])
	h.UnitBytes = binary.BigEndian.Uint32(b[60:])
	copy(h.RawSHA1[:], b[64:84])
	copy(h.SHA1[:], b[84:104])
	copy(h.ParentSHA1[:], b[104:124])
	if h.HunkBytes == 0 || h.UnitBytes == 0 {
		return Header{}, fmt.Errorf("%w: zero hunk or unit size", chderr.CorruptData)
	}
	if h.HunkBytes > MaxHunkBytes {
		return Header{}, fmt.Errorf("%w: hunk size %d exceeds %d", chderr.CorruptData, h.HunkBytes, MaxHunkBytes)
	}
	if hunks := (h.LogicalBytes + uint64(h.HunkBytes) - 1) / uint64(h.HunkBytes); hunks > math.MaxUint32 ||
		h.LogicalBytes > math.MaxUint64-uint64(h.HunkBytes) {
		return Header{}, fmt.Errorf("%w: %d bytes in %d byte hunks", chderr.CorruptData, h.LogicalBytes, h.HunkBytes)
	}
	if h.ParentSHA1 != [20]byte{} {
		return Header{}, fmt.Errorf("%w", chderr.ParentRequired)
	}
	return h, nil
}

// Marshal encodes the header in its on-disk form.
func (h Header) Marshal() [HeaderSize]byte {
	var b [HeaderSize]byte
	copy(b[:8], magic)
	binary.BigEndian.PutUint32(b[8:], HeaderSize)
	binary.BigEndian.PutUint32(b[12:], version)
	for i, c := range h.Codecs {
		binary.BigEndian.PutUint32(b[16+4*i:], uint32(c))
	}
	binary.BigEndian.PutUint64(b[32:], h.LogicalBytes)
	binary.BigEndian.PutUint64(b[40:], h.MapOffset)
	binary.BigEndian.PutUint64(b[48:], h.MetaOffset)
	binary.BigEndian.PutUint32(b[56:], h.HunkBytes)
	binary.BigEndian.PutUint32(b[60:], h.UnitBytes)
	copy(b[64:], h.RawSHA1[:])
	copy(b[84:], h.SHA1[:])
	copy(b[104:], h.ParentSHA1[:])
	return b
}
