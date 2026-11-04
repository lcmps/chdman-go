package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/flac"
	"github.com/lcmps/chdman-go/internal/format"
)

// Translated from MAME's src/lib/util/chdcodec.cpp (BSD-3-Clause, copyright
// Aaron Giles and contributors); see NOTICE.

const (
	cdSubcodeSize = 96
	cdFrameSize   = cdrom.SectorSize + cdSubcodeSize
)

// cdSyncHeader is the 12-byte sync pattern of a data sector.
var cdSyncHeader = [12]byte{0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0}

// cdHeaderSizes returns the number of ECC flag bytes and of base-length
// bytes in the header of a CD hunk of the given size.
func cdHeaderSizes(frames int, hunkBytes uint32) (ecc, lenBytes int) {
	lenBytes = 3
	if hunkBytes < 65536 {
		lenBytes = 2
	}
	return (frames + 7) / 8, lenBytes
}

func checkCDHunk(hunkBytes uint32) error {
	if hunkBytes == 0 || hunkBytes%cdFrameSize != 0 {
		return fmt.Errorf("codec: CD hunk size %d is not a multiple of %d", hunkBytes, cdFrameSize)
	}
	return nil
}

// cdSplit separates frames into audio-area bytes followed by subcode bytes.
func cdSplit(buf, src []byte, frames int) {
	for f := range frames {
		copy(buf[f*cdrom.SectorSize:], src[f*cdFrameSize:f*cdFrameSize+cdrom.SectorSize])
		copy(buf[frames*cdrom.SectorSize+f*cdSubcodeSize:], src[f*cdFrameSize+cdrom.SectorSize:(f+1)*cdFrameSize])
	}
}

// cdJoin is the inverse of cdSplit.
func cdJoin(dst, buf []byte, frames int) {
	for f := range frames {
		copy(dst[f*cdFrameSize:], buf[f*cdrom.SectorSize:(f+1)*cdrom.SectorSize])
		copy(dst[f*cdFrameSize+cdrom.SectorSize:], buf[frames*cdrom.SectorSize+f*cdSubcodeSize:][:cdSubcodeSize])
	}
}

// cdCompressor is chd_cd_compressor: sector data and subcode are compressed
// separately; sectors with a valid sync header and ECC lose both and are
// flagged in the header instead.
type cdCompressor struct {
	hunkBytes uint32
	base, sub Compressor
	buf       []byte
}

func newCDCompressor(baseTag, subTag format.CodecTag, hunkBytes uint32) (Compressor, error) {
	if err := checkCDHunk(hunkBytes); err != nil {
		return nil, err
	}
	frames := hunkBytes / cdFrameSize
	base, err := NewCompressor(baseTag, frames*cdrom.SectorSize)
	if err != nil {
		return nil, err
	}
	sub, err := NewCompressor(subTag, frames*cdSubcodeSize)
	if err != nil {
		return nil, err
	}
	return &cdCompressor{hunkBytes: hunkBytes, base: base, sub: sub, buf: make([]byte, hunkBytes)}, nil
}

func (c *cdCompressor) Compress(dst, src []byte) ([]byte, error) {
	if len(src) != int(c.hunkBytes) {
		return nil, fmt.Errorf("codec: CD hunk is %d bytes, want %d", len(src), c.hunkBytes)
	}
	frames := len(src) / cdFrameSize
	eccBytes, lenBytes := cdHeaderSizes(frames, uint32(len(src)))
	header := eccBytes + lenBytes

	out := append(dst[:0], make([]byte, header)...)
	cdSplit(c.buf, src, frames)
	for f := range frames {
		sector := c.buf[f*cdrom.SectorSize : (f+1)*cdrom.SectorSize]
		if bytes.Equal(sector[:len(cdSyncHeader)], cdSyncHeader[:]) && cdrom.ECCVerify(sector) {
			out[f/8] |= 1 << (f % 8)
			clear(sector[:len(cdSyncHeader)])
			cdrom.ECCClear(sector)
		}
	}

	base, err := c.base.Compress(nil, c.buf[:frames*cdrom.SectorSize])
	if err != nil {
		return nil, err
	}
	if len(base) >= len(src) {
		return nil, fmt.Errorf("codec: CD base data does not compress")
	}
	if lenBytes == 3 {
		out[eccBytes], out[eccBytes+1], out[eccBytes+2] = byte(len(base)>>16), byte(len(base)>>8), byte(len(base))
	} else {
		binary.BigEndian.PutUint16(out[eccBytes:], uint16(len(base)))
	}
	out = append(out, base...)

	sub, err := c.sub.Compress(nil, c.buf[frames*cdrom.SectorSize:])
	if err != nil {
		return nil, err
	}
	return append(out, sub...), nil
}

// cdDecompressor is chd_cd_decompressor.
type cdDecompressor struct {
	base, sub Decompressor
	buf       []byte
}

func newCDDecompressor(baseTag, subTag format.CodecTag, hunkBytes uint32) (Decompressor, error) {
	if err := checkCDHunk(hunkBytes); err != nil {
		return nil, err
	}
	frames := hunkBytes / cdFrameSize
	base, err := NewDecompressor(baseTag, frames*cdrom.SectorSize)
	if err != nil {
		return nil, err
	}
	sub, err := NewDecompressor(subTag, frames*cdSubcodeSize)
	if err != nil {
		return nil, err
	}
	return &cdDecompressor{base: base, sub: sub, buf: make([]byte, hunkBytes)}, nil
}

func (d *cdDecompressor) Decompress(dst, src []byte) error {
	if len(dst) == 0 || len(dst) != len(d.buf) {
		return fmt.Errorf("codec: CD hunk is %d bytes, want %d", len(dst), len(d.buf))
	}
	frames := len(dst) / cdFrameSize
	eccBytes, lenBytes := cdHeaderSizes(frames, uint32(len(dst)))
	header := eccBytes + lenBytes
	if len(src) < header {
		return fmt.Errorf("%w: CD codec: %d byte input is shorter than its %d byte header", chderr.CorruptData, len(src), header)
	}
	var baseLen int
	if lenBytes == 3 {
		baseLen = int(src[eccBytes])<<16 | int(src[eccBytes+1])<<8 | int(src[eccBytes+2])
	} else {
		baseLen = int(binary.BigEndian.Uint16(src[eccBytes:]))
	}
	if header+baseLen > len(src) {
		return fmt.Errorf("%w: CD codec: base length %d exceeds input", chderr.CorruptData, baseLen)
	}

	if err := d.base.Decompress(d.buf[:frames*cdrom.SectorSize], src[header:header+baseLen]); err != nil {
		return err
	}
	if err := d.sub.Decompress(d.buf[frames*cdrom.SectorSize:], src[header+baseLen:]); err != nil {
		return err
	}
	cdJoin(dst, d.buf, frames)
	for f := range frames {
		if src[f/8]&(1<<(f%8)) != 0 {
			sector := dst[f*cdFrameSize : f*cdFrameSize+cdrom.SectorSize]
			copy(sector, cdSyncHeader[:])
			cdrom.ECCGenerate(sector)
		}
	}
	return nil
}

// cdFLACCompressor is chd_cd_flac_compressor: FLAC over the big-endian
// audio of every sector, followed by the deflated subcode. ECC is not
// touched.
type cdFLACCompressor struct {
	hunkBytes uint32
	sub       Compressor
	samples   []int16
	subcode   []byte
}

func newCDFLACCompressor(hunkBytes uint32) (Compressor, error) {
	if err := checkCDHunk(hunkBytes); err != nil {
		return nil, err
	}
	frames := int(hunkBytes / cdFrameSize)
	sub, err := newDeflateCompressor()
	if err != nil {
		return nil, err
	}
	return &cdFLACCompressor{
		hunkBytes: hunkBytes,
		sub:       sub,
		samples:   make([]int16, frames*cdrom.SectorSize/2),
		subcode:   make([]byte, frames*cdSubcodeSize),
	}, nil
}

func (c *cdFLACCompressor) Compress(dst, src []byte) ([]byte, error) {
	if len(src) != int(c.hunkBytes) {
		return nil, fmt.Errorf("codec: CD hunk is %d bytes, want %d", len(src), c.hunkBytes)
	}
	frames := len(src) / cdFrameSize
	for f := range frames {
		fr := src[f*cdFrameSize:]
		readSamples(c.samples[f*cdrom.SectorSize/2:(f+1)*cdrom.SectorSize/2], fr[:cdrom.SectorSize], binary.BigEndian)
		copy(c.subcode[f*cdSubcodeSize:], fr[cdrom.SectorSize:cdFrameSize])
	}
	audio, err := flac.Encode(c.samples, int(cdFlacBlockSize(uint32(frames*cdrom.SectorSize))))
	if err != nil {
		return nil, err
	}
	sub, err := c.sub.Compress(nil, c.subcode)
	if err != nil {
		return nil, err
	}
	if len(audio)+len(sub) >= len(src) {
		return nil, fmt.Errorf("codec: CD FLAC data does not compress")
	}
	return append(append(dst[:0], audio...), sub...), nil
}

// cdFLACDecompressor is chd_cd_flac_decompressor.
type cdFLACDecompressor struct {
	sub     Decompressor
	samples []int16
	subcode []byte
}

func newCDFLACDecompressor(hunkBytes uint32) (Decompressor, error) {
	if err := checkCDHunk(hunkBytes); err != nil {
		return nil, err
	}
	frames := int(hunkBytes / cdFrameSize)
	return &cdFLACDecompressor{
		sub:     newDeflateDecompressor(),
		samples: make([]int16, frames*cdrom.SectorSize/2),
		subcode: make([]byte, frames*cdSubcodeSize),
	}, nil
}

func (d *cdFLACDecompressor) Decompress(dst, src []byte) error {
	frames := len(dst) / cdFrameSize
	if frames*cdFrameSize != len(dst) || len(d.samples) != frames*cdrom.SectorSize/2 {
		return fmt.Errorf("codec: CD hunk is %d bytes, want %d", len(dst), len(d.samples)/(cdrom.SectorSize/2)*cdFrameSize)
	}
	used, err := flac.Decode(src, d.samples, int(cdFlacBlockSize(uint32(frames*cdrom.SectorSize))))
	if err != nil {
		return err
	}
	if err := d.sub.Decompress(d.subcode, src[used:]); err != nil {
		return err
	}
	for f := range frames {
		fr := dst[f*cdFrameSize:]
		writeSamples(fr[:cdrom.SectorSize], d.samples[f*cdrom.SectorSize/2:(f+1)*cdrom.SectorSize/2], binary.BigEndian)
		copy(fr[cdrom.SectorSize:cdFrameSize], d.subcode[f*cdSubcodeSize:])
	}
	return nil
}
