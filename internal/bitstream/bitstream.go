// Package bitstream implements MSB-first bit readers and writers.
//
// Translated from MAME's src/lib/util/bitstream.h (BSD-3-Clause, copyright
// Aaron Giles and contributors); see NOTICE.
package bitstream

// Reader reads bits MSB first. Bits past the end of the data read as zero and
// set the overflow condition, as in MAME's bitstream_in.
type Reader struct {
	buf []byte
	pos uint64 // bit position of the next bit to read
}

// NewReader returns a Reader over b.
func NewReader(b []byte) *Reader { return &Reader{buf: b} }

// Peek returns the next n bits (0 <= n <= 32) without consuming them.
func (r *Reader) Peek(n int) uint32 {
	if n == 0 {
		return 0
	}
	idx := int(r.pos >> 3)
	off := uint(r.pos & 7)
	var v uint64
	for k := 0; k < 5; k++ {
		v <<= 8
		if idx+k < len(r.buf) {
			v |= uint64(r.buf[idx+k])
		}
	}
	return uint32(v>>(40-off-uint(n))) & uint32(1<<uint(n)-1)
}

// Remove consumes n bits.
func (r *Reader) Remove(n int) { r.pos += uint64(n) }

// Read consumes and returns the next n bits (0 <= n <= 32).
func (r *Reader) Read(n int) uint32 {
	v := r.Peek(n)
	r.Remove(n)
	return v
}

// Overflow reports whether more bits were consumed than the data holds.
func (r *Reader) Overflow() bool { return r.pos > uint64(len(r.buf))*8 }

// Writer writes bits MSB first into a growing buffer.
type Writer struct {
	out   []byte
	acc   uint64 // pending bits, right-aligned
	nbits int    // number of pending bits (always < 8 between calls)
}

// NewWriter returns an empty Writer.
func NewWriter() *Writer { return &Writer{} }

// Write appends the low n bits (0 <= n <= 32) of v.
func (w *Writer) Write(v uint32, n int) {
	if n == 0 {
		return
	}
	w.acc = w.acc<<uint(n) | uint64(v)&(1<<uint(n)-1)
	w.nbits += n
	for w.nbits >= 8 {
		w.nbits -= 8
		w.out = append(w.out, byte(w.acc>>uint(w.nbits)))
	}
	w.acc &= 1<<uint(w.nbits) - 1
}

// Bytes returns everything written so far, with a final partial byte padded
// with zero bits. It does not alter the Writer.
func (w *Writer) Bytes() []byte {
	b := append([]byte(nil), w.out...)
	if w.nbits > 0 {
		b = append(b, byte(w.acc<<uint(8-w.nbits)))
	}
	return b
}
