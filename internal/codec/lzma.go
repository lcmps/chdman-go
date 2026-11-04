package codec

// Translated from MAME's src/lib/util/chdcodec.cpp (BSD-3-Clause, copyright
// Aaron Giles); see NOTICE.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/ulikunitz/xz/lzma"
)

const (
	lzmaHeaderSize = 13
	// lzmaPropsByte encodes lc=3, lp=0, pb=2: (pb*5+lp)*9+lc.
	lzmaPropsByte = (2*5+0)*9 + 3
)

// LZMADictSize returns the dictionary size chdman's level 8 encoder uses
// for hunks of n bytes (LzmaEncProps_Normalize with reduceSize = n).
func LZMADictSize(n uint32) uint32 {
	const levelDict = 1 << 26 // level 8
	const reduceMin = 1 << 12
	if levelDict > n {
		return max(n, reduceMin)
	}
	return levelDict
}

// lzmaCompressor emits a raw LZMA stream (no 13-byte .lzma header, no end
// marker), matching chd_lzma_compressor.
type lzmaCompressor struct {
	dictSize uint32
	buf      bytes.Buffer
}

func newLZMACompressor(hunkBytes uint32) *lzmaCompressor {
	return &lzmaCompressor{dictSize: LZMADictSize(hunkBytes)}
}

func (c *lzmaCompressor) Compress(dst, src []byte) ([]byte, error) {
	c.buf.Reset()
	w, err := lzma.WriterConfig{
		Properties:   &lzma.Properties{LC: 3, LP: 0, PB: 2},
		DictCap:      int(c.dictSize),
		Matcher:      lzma.HashTable4,
		SizeInHeader: true,
		Size:         int64(len(src)),
		EOSMarker:    false,
	}.NewWriter(&c.buf)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return append(dst[:0], c.buf.Bytes()[lzmaHeaderSize:]...), nil
}

// lzmaDecompressor decodes a raw LZMA stream by synthesizing the classic
// header the library expects.
type lzmaDecompressor struct {
	dictSize uint32
	buf      bytes.Buffer
}

func newLZMADecompressor(hunkBytes uint32) *lzmaDecompressor {
	return &lzmaDecompressor{dictSize: LZMADictSize(hunkBytes)}
}

func (d *lzmaDecompressor) Decompress(dst, src []byte) error {
	var hdr [lzmaHeaderSize]byte
	hdr[0] = lzmaPropsByte
	binary.LittleEndian.PutUint32(hdr[1:], d.dictSize)
	binary.LittleEndian.PutUint64(hdr[5:], uint64(len(dst)))
	d.buf.Reset()
	d.buf.Write(hdr[:])
	d.buf.Write(src)
	r, err := lzma.ReaderConfig{DictCap: int(d.dictSize)}.NewReader(&d.buf)
	if err != nil {
		return fmt.Errorf("%w: lzma: %v", chderr.CorruptData, err)
	}
	if _, err := io.ReadFull(r, dst); err != nil {
		return fmt.Errorf("%w: lzma: %v", chderr.CorruptData, err)
	}
	return nil
}
