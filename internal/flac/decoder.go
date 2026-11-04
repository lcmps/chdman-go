// Package flac implements the headerless 16-bit stereo FLAC streams that CHD
// stores: a sequence of FLAC frames with no "fLaC" marker or metadata.
//
// The decoder follows the FLAC format specification in full (constant,
// verbatim, fixed and LPC subframes, every stereo mode, both Rice residual
// codings with escapes, wasted bits), so it reads streams written by libFLAC
// as used by chdman. The format is described in RFC 9639; chdman's wrapper is
// src/lib/util/flac.cpp (BSD-3-Clause, copyright Aaron Giles and
// contributors; see NOTICE).
package flac

import (
	"fmt"
	"math/bits"
	"slices"

	"github.com/lcmps/chdman-go/internal/chderr"
)

const (
	channels     = 2
	sampleBits   = 16
	maxFixedOrd  = 4
	maxBlockSize = 65535
)

var errTruncated = fmt.Errorf("%w: flac: truncated stream", chderr.CorruptData)

func corrupt(format string, a ...any) error {
	return fmt.Errorf("%w: flac: %s", chderr.CorruptData, fmt.Sprintf(format, a...))
}

// bitReader reads bits MSB first and records the position, which Decode
// needs to report how many bytes a stream consumed.
type bitReader struct {
	buf []byte
	pos int // bit position
}

func (r *bitReader) overrun() bool { return r.pos > len(r.buf)*8 }

// read returns the next n bits (0 <= n <= 32). Past the end it returns zero
// bits; callers check overrun.
func (r *bitReader) read(n int) uint32 {
	var v uint64
	idx, off := r.pos>>3, r.pos&7
	for k := range 5 {
		v <<= 8
		if idx+k < len(r.buf) {
			v |= uint64(r.buf[idx+k])
		}
	}
	r.pos += n
	if n == 0 {
		return 0
	}
	return uint32(v>>(40-off-n)) & uint32(1<<uint(n)-1)
}

// readSigned reads an n-bit two's complement value (0 <= n <= 32).
func (r *bitReader) readSigned(n int) int32 {
	if n == 0 {
		return 0
	}
	return int32(r.read(n)<<(32-n)) >> (32 - n)
}

// unary counts zero bits up to and including the next one bit.
func (r *bitReader) unary() uint32 {
	var q uint32
	for !r.overrun() {
		// Look at up to 32 bits at once.
		v := r.read(32)
		if v == 0 {
			q += 32
			continue
		}
		z := bits.LeadingZeros32(v)
		r.pos -= 32 - z - 1
		return q + uint32(z)
	}
	return q
}

func (r *bitReader) alignByte() { r.pos = (r.pos + 7) &^ 7 }

func crc8(b []byte) byte {
	var c byte
	for _, v := range b {
		c ^= v
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

var crc16Table = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

func crc16(b []byte) uint16 {
	var c uint16
	for _, v := range b {
		c = c<<8 ^ crc16Table[byte(c>>8)^v]
	}
	return c
}

// decoder holds scratch buffers reused between frames.
type decoder struct {
	r  bitReader
	ch [channels][]int32
}

// Decode decodes headerless FLAC frames from src into dst, which holds
// interleaved stereo samples (len(dst)/2 per channel). It stops once dst is
// full and returns the number of bytes of src the decoded frames occupy;
// anything after is left alone (CD FLAC appends deflated subcode there).
// blockSize is the encoder's block size, used only to size scratch space;
// the frame headers are authoritative.
func Decode(src []byte, dst []int16, blockSize int) (consumed int, err error) {
	d := &decoder{r: bitReader{buf: src}}
	for i := range d.ch {
		d.ch[i] = make([]int32, 0, blockSize)
	}
	want := len(dst) / channels
	for done := 0; done < want; {
		n, err := d.frame()
		if err != nil {
			return 0, err
		}
		for i := 0; i < n && done < want; i, done = i+1, done+1 {
			dst[done*2] = int16(d.ch[0][i])
			dst[done*2+1] = int16(d.ch[1][i])
		}
	}
	return d.r.pos / 8, nil
}

var fixedCoefs = [maxFixedOrd + 1][]int64{{}, {1}, {2, -1}, {3, -3, 1}, {4, -6, 4, -1}}

// frame decodes one frame into d.ch and returns its block size.
func (d *decoder) frame() (int, error) {
	r := &d.r
	start := r.pos / 8
	if start >= len(r.buf) {
		return 0, errTruncated
	}
	if r.read(14) != 0x3ffe {
		return 0, corrupt("bad frame sync")
	}
	if r.read(1) != 0 {
		return 0, corrupt("reserved header bit set")
	}
	r.read(1) // blocking strategy: frame or sample number, irrelevant here
	bsCode := int(r.read(4))
	srCode := int(r.read(4))
	chCode := int(r.read(4))
	bpsCode := int(r.read(3))
	if r.read(1) != 0 {
		return 0, corrupt("reserved header bit set")
	}
	if err := d.skipUTF8Number(); err != nil {
		return 0, err
	}

	var size int
	switch {
	case bsCode == 0:
		return 0, corrupt("reserved block size code")
	case bsCode == 1:
		size = 192
	case bsCode <= 5:
		size = 576 << (bsCode - 2)
	case bsCode == 6:
		size = int(r.read(8)) + 1
	case bsCode == 7:
		size = int(r.read(16)) + 1
	default:
		size = 256 << (bsCode - 8)
	}
	switch srCode {
	case 12:
		r.read(8)
	case 13, 14:
		r.read(16)
	case 15:
		return 0, corrupt("invalid sample rate code")
	}
	if r.overrun() {
		return 0, errTruncated
	}
	hdrEnd := r.pos / 8
	if got, want := byte(r.read(8)), crc8(r.buf[start:hdrEnd]); got != want {
		return 0, corrupt("frame header CRC-8 mismatch")
	}
	// Code 0 means "as in STREAMINFO", which for CHD is always 16 bits.
	if bpsCode != 0 && bpsCode != 4 {
		return 0, corrupt("unsupported sample size code %d", bpsCode)
	}
	if chCode < 1 || chCode > 10 {
		return 0, corrupt("unsupported channel assignment %d", chCode)
	}

	// Side channels carry one extra bit.
	bps := [channels]int{sampleBits, sampleBits}
	switch chCode {
	case 8, 10:
		bps[1]++
	case 9:
		bps[0]++
	}
	for c := range channels {
		d.ch[c] = slices.Grow(d.ch[c][:0], size)[:size]
		if err := d.subframe(d.ch[c], bps[c]); err != nil {
			return 0, err
		}
	}
	r.alignByte()
	if r.overrun() {
		return 0, errTruncated
	}
	end := r.pos / 8
	if got, want := uint16(r.read(16)), crc16(r.buf[start:end]); got != want || r.overrun() {
		if r.overrun() {
			return 0, errTruncated
		}
		return 0, corrupt("frame CRC-16 mismatch")
	}

	a, b := d.ch[0], d.ch[1]
	switch chCode {
	case 8: // left, side
		for i := range size {
			b[i] = a[i] - b[i]
		}
	case 9: // side, right
		for i := range size {
			a[i] += b[i]
		}
	case 10: // mid, side
		for i := range size {
			mid, side := a[i], b[i]
			mid = mid<<1 | side&1
			a[i], b[i] = (mid+side)>>1, (mid-side)>>1
		}
	}
	return size, nil
}

// skipUTF8Number steps over the UTF-8 coded frame or sample number.
func (d *decoder) skipUTF8Number() error {
	r := &d.r
	first := byte(r.read(8))
	n := bits.LeadingZeros8(^first) // total bytes
	switch {
	case n == 0:
		return nil
	case n == 1 || n > 7:
		return corrupt("bad UTF-8 frame number")
	}
	for range n - 1 {
		if r.read(8)&0xc0 != 0x80 {
			return corrupt("bad UTF-8 frame number")
		}
	}
	return nil
}

// subframe decodes one subframe of len(out) samples at the given bit depth.
func (d *decoder) subframe(out []int32, bps int) error {
	r := &d.r
	if r.read(1) != 0 {
		return corrupt("subframe padding bit set")
	}
	typ := int(r.read(6))
	wasted := 0
	if r.read(1) != 0 {
		wasted = int(r.unary()) + 1
	}
	bps -= wasted
	if bps <= 0 || r.overrun() {
		return corrupt("invalid wasted bits")
	}

	switch {
	case typ == 0:
		v := r.readSigned(bps)
		for i := range out {
			out[i] = v
		}
	case typ == 1:
		for i := range out {
			out[i] = r.readSigned(bps)
		}
	case typ >= 8 && typ <= 8+maxFixedOrd:
		order := typ - 8
		if err := d.warmup(out, order, bps); err != nil {
			return err
		}
		if err := d.residual(out, order); err != nil {
			return err
		}
		coefs := fixedCoefs[order]
		for i := order; i < len(out); i++ {
			var pred int64
			for j, c := range coefs {
				pred += c * int64(out[i-1-j])
			}
			out[i] += int32(pred)
		}
	case typ >= 32:
		order := typ - 31
		if err := d.warmup(out, order, bps); err != nil {
			return err
		}
		prec := int(r.read(4)) + 1
		if prec == 16 {
			return corrupt("invalid LPC precision")
		}
		shift := r.readSigned(5)
		if shift < 0 {
			return corrupt("negative LPC shift")
		}
		var coefs [32]int64
		for j := range order {
			coefs[j] = int64(r.readSigned(prec))
		}
		if err := d.residual(out, order); err != nil {
			return err
		}
		for i := order; i < len(out); i++ {
			var pred int64
			for j := range order {
				pred += coefs[j] * int64(out[i-1-j])
			}
			out[i] += int32(pred >> shift)
		}
	default:
		return corrupt("reserved subframe type %d", typ)
	}
	if r.overrun() {
		return errTruncated
	}
	if wasted > 0 {
		for i := range out {
			out[i] <<= wasted
		}
	}
	return nil
}

func (d *decoder) warmup(out []int32, order, bps int) error {
	if order > len(out) {
		return corrupt("predictor order %d exceeds block size %d", order, len(out))
	}
	for i := range order {
		out[i] = d.r.readSigned(bps)
	}
	return nil
}

// residual decodes the partitioned Rice residual into out[order:].
func (d *decoder) residual(out []int32, order int) error {
	r := &d.r
	method := r.read(2)
	if method > 1 {
		return corrupt("reserved residual coding method %d", method)
	}
	paramBits, escape := 4, uint32(15)
	if method == 1 {
		paramBits, escape = 5, 31
	}
	partOrder := int(r.read(4))
	parts := 1 << partOrder
	n := len(out)
	if n&(parts-1) != 0 || n>>partOrder < order {
		return corrupt("bad partition order %d for block size %d", partOrder, n)
	}
	pos := order
	for p := range parts {
		cnt := n >> partOrder
		if p == 0 {
			cnt -= order
		}
		param := r.read(paramBits)
		if param == escape {
			w := int(r.read(5))
			for i := range cnt {
				out[pos+i] = r.readSigned(w)
			}
		} else {
			k := int(param)
			for i := range cnt {
				u := r.unary()<<k | r.read(k)
				out[pos+i] = int32(u>>1) ^ -int32(u&1)
			}
		}
		pos += cnt
		if r.overrun() {
			return errTruncated
		}
	}
	return nil
}
