package codec

import (
	"encoding/binary"
	"fmt"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/flac"
)

// Translated from MAME's src/lib/util/chdcodec.cpp (BSD-3-Clause, copyright
// Aaron Giles and contributors); see NOTICE.

// flacBlockSize is chd_flac_compressor::blocksize: a quarter of the byte
// count (samples per channel), halved until it is at most 2048.
func flacBlockSize(bytes uint32) uint32 {
	n := bytes / 4
	for n > 2048 {
		n /= 2
	}
	return n
}

// cdFlacBlockSize is chd_cd_flac_compressor::blocksize: as flacBlockSize but
// targeting one sector's worth of samples (2352).
func cdFlacBlockSize(bytes uint32) uint32 {
	n := bytes / 4
	for n > 2352 {
		n /= 2
	}
	return n
}

// readSamples reads src as 16-bit samples in the given byte order.
func readSamples(dst []int16, src []byte, order binary.ByteOrder) {
	for i := range dst {
		dst[i] = int16(order.Uint16(src[2*i:]))
	}
}

func writeSamples(dst []byte, src []int16, order binary.ByteOrder) {
	for i, s := range src {
		order.PutUint16(dst[2*i:], uint16(s))
	}
}

// flacCompressor is the generic "flac" codec: a lead byte 'L' or 'B' naming
// the byte order of the samples, then headerless FLAC frames. Both orders
// are tried and the smaller kept (little-endian on ties).
type flacCompressor struct {
	hunkBytes uint32
	samples   []int16
}

func newFLACCompressor(hunkBytes uint32) *flacCompressor {
	return &flacCompressor{hunkBytes: hunkBytes}
}

func (c *flacCompressor) Compress(dst, src []byte) ([]byte, error) {
	c.samples = append(c.samples[:0], make([]int16, len(src)/2)...)
	bs := int(flacBlockSize(c.hunkBytes))

	readSamples(c.samples, src, binary.LittleEndian)
	le, err := flac.Encode(c.samples, bs)
	if err != nil {
		return nil, err
	}
	readSamples(c.samples, src, binary.BigEndian)
	be, err := flac.Encode(c.samples, bs)
	if err != nil {
		return nil, err
	}

	lead, best := byte('L'), le
	if len(be) < len(le) {
		lead, best = 'B', be
	}
	if len(best)+1 >= int(c.hunkBytes) {
		return nil, fmt.Errorf("flac: %d bytes does not fit a %d byte hunk", len(best)+1, c.hunkBytes)
	}
	return append(append(dst[:0], lead), best...), nil
}

type flacDecompressor struct {
	samples []int16
}

func (d *flacDecompressor) Decompress(dst, src []byte) error {
	if len(src) == 0 {
		return fmt.Errorf("%w: flac: empty input", chderr.CorruptData)
	}
	var order binary.ByteOrder
	switch src[0] {
	case 'L':
		order = binary.LittleEndian
	case 'B':
		order = binary.BigEndian
	default:
		return fmt.Errorf("%w: flac: bad byte order marker %#x", chderr.CorruptData, src[0])
	}
	d.samples = append(d.samples[:0], make([]int16, len(dst)/2)...)
	if _, err := flac.Decode(src[1:], d.samples, int(flacBlockSize(uint32(len(dst))))); err != nil {
		return err
	}
	writeSamples(dst, d.samples, order)
	return nil
}
