package flac

import (
	"errors"
	"math/bits"
)

// bitWriter accumulates bits MSB first.
type bitWriter struct {
	buf  []byte
	acc  uint64
	nacc int // pending bits in acc
	n    int // total bits written
}

// put appends the low n bits (0 <= n <= 32) of v.
func (w *bitWriter) put(v uint32, n int) {
	w.acc = w.acc<<uint(n) | uint64(v)&(1<<uint(n)-1)
	w.nacc += n
	w.n += n
	for w.nacc >= 8 {
		w.nacc -= 8
		w.buf = append(w.buf, byte(w.acc>>uint(w.nacc)))
	}
	w.acc &= 1<<uint(w.nacc) - 1
}

func (w *bitWriter) putSigned(v int32, n int) { w.put(uint32(v), n) }

func (w *bitWriter) zeros(n int) {
	for ; n >= 32; n -= 32 {
		w.put(0, 32)
	}
	w.put(0, n)
}

func (w *bitWriter) alignByte() {
	if r := w.n % 8; r != 0 {
		w.put(0, 8-r)
	}
}

// appendBits copies all bits of o into w.
func (w *bitWriter) appendBits(o *bitWriter) {
	for _, b := range o.buf {
		w.put(uint32(b), 8)
	}
	if o.nacc > 0 {
		w.put(uint32(o.acc), o.nacc)
	}
}

const maxPartitionOrder = 6

// subframe is a fully encoded subframe and its length in bits.
type subframe struct{ w bitWriter }

// encoder holds scratch buffers reused between blocks.
type encoder struct {
	shifted []int32
	res     [maxFixedOrd + 1][]int32
	sums    []uint64
}

// Encode compresses interleaved 16-bit stereo samples into headerless FLAC
// frames of blockSize samples per channel (the last frame may be shorter).
// Each channel pair is coded with whichever of left/right, left/side,
// side/right and mid/side is smallest, using constant, verbatim or fixed
// predictor subframes with partitioned Rice residuals.
func Encode(samples []int16, blockSize int) ([]byte, error) {
	if len(samples)%channels != 0 {
		return nil, errors.New("flac: odd number of samples for a stereo stream")
	}
	if blockSize < 1 || blockSize > maxBlockSize {
		return nil, errors.New("flac: block size out of range")
	}
	var e encoder
	var out []byte
	total := len(samples) / channels
	l := make([]int32, 0, blockSize)
	r := make([]int32, 0, blockSize)
	mid := make([]int32, 0, blockSize)
	side := make([]int32, 0, blockSize)
	for start, num := 0, uint64(0); start < total; start, num = start+blockSize, num+1 {
		n := min(blockSize, total-start)
		l, r, mid, side = l[:n], r[:n], mid[:n], side[:n]
		for i := range n {
			a, b := int32(samples[(start+i)*2]), int32(samples[(start+i)*2+1])
			l[i], r[i] = a, b
			mid[i], side[i] = (a+b)>>1, a-b
		}
		sl := e.subframe(l, sampleBits)
		sr := e.subframe(r, sampleBits)
		sm := e.subframe(mid, sampleBits)
		ss := e.subframe(side, sampleBits+1)

		best := struct {
			code int
			a, b *subframe
		}{1, sl, sr}
		bestBits := sl.w.n + sr.w.n
		for _, c := range []struct {
			code int
			a, b *subframe
		}{{8, sl, ss}, {9, ss, sr}, {10, sm, ss}} {
			if nb := c.a.w.n + c.b.w.n; nb < bestBits {
				best, bestBits = c, nb
			}
		}

		hdr := frameHeader(num, n, best.code)
		w := bitWriter{buf: hdr, n: len(hdr) * 8}
		w.appendBits(&best.a.w)
		w.appendBits(&best.b.w)
		w.alignByte()
		c := crc16(w.buf)
		w.put(uint32(c), 16)
		out = append(out, w.buf...)
	}
	return out, nil
}

var stdBlockSizes = map[int]int{
	192: 1, 576: 2, 1152: 3, 2304: 4, 4608: 5,
	256: 8, 512: 9, 1024: 10, 2048: 11, 4096: 12, 8192: 13, 16384: 14, 32768: 15,
}

// frameHeader returns a complete frame header including its CRC-8: fixed
// block size, 44.1 kHz, 16 bits per sample.
func frameHeader(frameNum uint64, size, chCode int) []byte {
	code, ok := stdBlockSizes[size]
	if !ok {
		code = 7
		if size <= 256 {
			code = 6
		}
	}
	h := []byte{0xff, 0xf8, byte(code<<4 | 9), byte(chCode<<4 | 4<<1)}
	h = append(h, utf8Number(frameNum)...)
	switch code {
	case 6:
		h = append(h, byte(size-1))
	case 7:
		h = append(h, byte((size-1)>>8), byte(size-1))
	}
	return append(h, crc8(h))
}

// utf8Number encodes n in the FLAC variant of UTF-8 (up to 36 bits).
func utf8Number(n uint64) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	nb := 2
	for n >= 1<<(5*nb+1) {
		nb++
	}
	out := make([]byte, nb)
	for i := nb - 1; i > 0; i-- {
		out[i] = 0x80 | byte(n&0x3f)
		n >>= 6
	}
	out[0] = byte(0xff<<(8-nb)) | byte(n)
	return out
}

// subframe encodes x, whose samples need bps bits, choosing the smallest of
// constant, verbatim and fixed-predictor coding.
func (e *encoder) subframe(x []int32, bps int) *subframe {
	sf := &subframe{}
	w := &sf.w

	constant, or := true, int32(0)
	for _, v := range x {
		or |= v
		if v != x[0] {
			constant = false
		}
	}
	wasted := 0
	if or != 0 {
		wasted = bits.TrailingZeros32(uint32(or))
	}
	if constant {
		writeSubHeader(w, 0, wasted)
		w.putSigned(x[0]>>wasted, bps-wasted)
		return sf
	}

	y := x
	if wasted > 0 {
		e.shifted = append(e.shifted[:0], x...)
		for i := range e.shifted {
			e.shifted[i] >>= wasted
		}
		y = e.shifted
	}
	bp := bps - wasted

	bestOrder := -1 // verbatim
	bestBits := 8 + wasted + len(y)*bp
	var bestPart partitioning
	for order := 0; order <= maxFixedOrd && order < len(y); order++ {
		res := e.residual(y, order)
		part := e.bestPartitioning(res, len(y), order)
		if nb := 8 + wasted + order*bp + part.bits; nb < bestBits {
			bestOrder, bestBits, bestPart = order, nb, part
		}
	}

	if bestOrder < 0 {
		writeSubHeader(w, 1, wasted)
		for _, v := range y {
			w.putSigned(v, bp)
		}
		return sf
	}
	writeSubHeader(w, 8+bestOrder, wasted)
	for _, v := range y[:bestOrder] {
		w.putSigned(v, bp)
	}
	writeResidual(w, e.res[bestOrder], len(y), bestOrder, bestPart)
	return sf
}

func writeSubHeader(w *bitWriter, typ, wasted int) {
	w.put(0, 1)
	w.put(uint32(typ), 6)
	if wasted == 0 {
		w.put(0, 1)
		return
	}
	w.put(1, 1)
	w.zeros(wasted - 1)
	w.put(1, 1)
}

// residual computes the fixed predictor residual of the given order.
func (e *encoder) residual(y []int32, order int) []int32 {
	res := e.res[order][:0]
	coefs := fixedCoefs[order]
	for i := order; i < len(y); i++ {
		var pred int64
		for j, c := range coefs {
			pred += c * int64(y[i-1-j])
		}
		res = append(res, y[i]-int32(pred))
	}
	e.res[order] = res
	return res
}

func zigzag(v int32) uint32 { return uint32(v<<1) ^ uint32(v>>31) }

// partitioning is a chosen Rice partitioning with its estimated bit cost.
type partitioning struct {
	order  int
	params []int
	bits   int
}

// bestPartitioning picks the partition order and per-partition Rice
// parameters that minimize the estimated residual size.
func (e *encoder) bestPartitioning(res []int32, blockSize, predOrder int) partitioning {
	// Largest usable order: blockSize must divide evenly and the first
	// partition must hold at least predOrder samples.
	maxOrder := 0
	for o := 1; o <= maxPartitionOrder; o++ {
		if blockSize&(1<<o-1) != 0 || blockSize>>o < predOrder {
			break
		}
		maxOrder = o
	}
	parts := 1 << maxOrder
	e.sums = e.sums[:0]
	counts := make([]int, parts)
	pos := 0
	for p := range parts {
		cnt := blockSize >> maxOrder
		if p == 0 {
			cnt -= predOrder
		}
		var s uint64
		for _, v := range res[pos : pos+cnt] {
			s += uint64(zigzag(v))
		}
		pos += cnt
		e.sums = append(e.sums, s)
		counts[p] = cnt
	}

	var best partitioning
	best.bits = -1
	sums, cnts := append([]uint64(nil), e.sums...), counts
	for o := maxOrder; o >= 0; o-- {
		params := make([]int, len(sums))
		total := 0
		for p, s := range sums {
			k, b := bestRice(s, cnts[p])
			params[p] = k
			total += b
		}
		total += 6 // method and partition order
		if best.bits < 0 || total < best.bits {
			best = partitioning{o, params, total}
		}
		if o == 0 {
			break
		}
		half := len(sums) / 2
		ns, nc := make([]uint64, half), make([]int, half)
		for p := range half {
			ns[p], nc[p] = sums[2*p]+sums[2*p+1], cnts[2*p]+cnts[2*p+1]
		}
		sums, cnts = ns, nc
	}
	return best
}

// bestRice returns the Rice parameter minimizing the estimated size of a
// partition of cnt samples whose zigzagged values sum to sum, and that size
// including the parameter field.
func bestRice(sum uint64, cnt int) (k, nbits int) {
	nbits = -1
	for c := 0; c <= 30; c++ {
		b := cnt*(c+1) + int(sum>>uint(c))
		if nbits < 0 || b < nbits {
			k, nbits = c, b
		}
		if sum>>uint(c) == 0 {
			break
		}
	}
	return k, nbits + 5
}

func writeResidual(w *bitWriter, res []int32, blockSize, predOrder int, p partitioning) {
	method, paramBits := 0, 4
	for _, k := range p.params {
		if k > 14 {
			method, paramBits = 1, 5
		}
	}
	w.put(uint32(method), 2)
	w.put(uint32(p.order), 4)
	pos := 0
	for i, k := range p.params {
		cnt := blockSize >> p.order
		if i == 0 {
			cnt -= predOrder
		}
		w.put(uint32(k), paramBits)
		for _, v := range res[pos : pos+cnt] {
			u := zigzag(v)
			w.zeros(int(u >> uint(k)))
			w.put(1, 1)
			w.put(u, k)
		}
		pos += cnt
	}
}
