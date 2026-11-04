package flac

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/lcmps/chdman-go/internal/bitstream"
)

// ---- signal generators ----

func signals(n int) map[string][]int16 {
	rng := rand.New(rand.NewPCG(7, 9))
	sil := make([]int16, 2*n)
	sine := make([]int16, 2*n)
	noise := make([]int16, 2*n)
	square := make([]int16, 2*n)
	for i := range n {
		sine[2*i] = int16(12000 * math.Sin(float64(i)*0.05))
		sine[2*i+1] = int16(9000*math.Sin(float64(i)*0.031) + 300*math.Sin(float64(i)*1.3))
		noise[2*i] = int16(rng.Uint32())
		noise[2*i+1] = int16(rng.Uint32())
		v := int16(32767)
		if i/37%2 == 1 {
			v = -32768
		}
		square[2*i], square[2*i+1] = v, -v-1
	}
	return map[string][]int16{"silence": sil, "sine": sine, "noise": noise, "square": square}
}

func TestFLACRoundTrip(t *testing.T) {
	for _, bs := range []int{576, 588, 1024, 2048} {
		for _, n := range []int{bs * 4, bs*3 + 37, 5} {
			for name, s := range signals(n) {
				enc, err := Encode(s, bs)
				if err != nil {
					t.Fatal(err)
				}
				out := make([]int16, len(s))
				used, err := Decode(enc, out, bs)
				if err != nil {
					t.Fatalf("%s bs=%d n=%d: %v", name, bs, n, err)
				}
				if used != len(enc) {
					t.Errorf("%s bs=%d n=%d: consumed %d of %d", name, bs, n, used, len(enc))
				}
				if !slices.Equal(out, s) {
					t.Fatalf("%s bs=%d n=%d: mismatch", name, bs, n)
				}
			}
		}
	}
}

func TestEncodeCompresses(t *testing.T) {
	s := signals(2048)
	for _, name := range []string{"silence", "sine"} {
		enc, _ := Encode(s[name], 1024)
		if len(enc) >= len(s[name])*2/2 {
			t.Errorf("%s: %d bytes for %d raw", name, len(enc), len(s[name])*2)
		}
	}
}

func TestDecodeTrailingDataAndTruncation(t *testing.T) {
	s := signals(1024)["sine"]
	enc, _ := Encode(s, 1024)
	out := make([]int16, len(s))
	used, err := Decode(append(slices.Clone(enc), 1, 2, 3), out, 1024)
	if err != nil || used != len(enc) {
		t.Fatalf("trailing: used=%d err=%v", used, err)
	}
	if _, err := Decode(enc[:len(enc)-3], out, 1024); err == nil {
		t.Error("truncated stream decoded")
	}
	bad := slices.Clone(enc)
	bad[len(bad)/2] ^= 0x10
	if _, err := Decode(bad, out, 1024); err == nil {
		t.Error("corrupt stream decoded")
	}
}

// ---- independent test-side FLAC frame builder (written from the spec) ----

func tcrc8(b []byte) byte {
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

func tcrc16(b []byte) uint16 {
	var c uint16
	for _, v := range b {
		c ^= uint16(v) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
	}
	return c
}

type bw struct {
	w *bitstream.Writer
	n int
}

func newBW() *bw { return &bw{w: bitstream.NewWriter()} }
func (b *bw) put(v uint32, n int) {
	b.w.Write(v, n)
	b.n += n
}
func (b *bw) signed(v int32, n int) { b.put(uint32(v), n) }
func (b *bw) align() {
	if b.n%8 != 0 {
		b.put(0, 8-b.n%8)
	}
}

// residual writes a partitioned Rice residual. params has one entry per
// partition; a negative entry means "escape with raw width -entry-1 bits"
// (so -1 is a zero-width escape).
func (b *bw) residual(res []int32, predOrder, method, partOrder int, params []int, total int) {
	b.put(uint32(method), 2)
	b.put(uint32(partOrder), 4)
	pbits, esc := 4, 15
	if method == 1 {
		pbits, esc = 5, 31
	}
	pos := 0
	for p := range 1 << partOrder {
		cnt := total >> partOrder
		if p == 0 {
			cnt -= predOrder
		}
		part := res[pos : pos+cnt]
		pos += cnt
		k := params[p]
		if k < 0 {
			w := -k - 1
			b.put(uint32(esc), pbits)
			b.put(uint32(w), 5)
			for _, v := range part {
				if w == 0 && v != 0 {
					panic("zero-width escape with nonzero residual")
				}
				b.signed(v, w)
			}
			continue
		}
		b.put(uint32(k), pbits)
		for _, v := range part {
			u := uint32(v<<1) ^ uint32(v>>31)
			q := int(u >> k)
			for range q {
				b.put(0, 1)
			}
			b.put(1, 1)
			b.put(u&(1<<k-1), k)
		}
	}
}

func subHeader(b *bw, typ, wasted int) {
	b.put(0, 1)
	b.put(uint32(typ), 6)
	if wasted == 0 {
		b.put(0, 1)
		return
	}
	b.put(1, 1)
	for range wasted - 1 {
		b.put(0, 1)
	}
	b.put(1, 1)
}

func shiftDown(x []int32, wasted int) []int32 {
	y := make([]int32, len(x))
	for i, v := range x {
		if v&(1<<wasted-1) != 0 {
			panic("sample not a multiple of 2^wasted")
		}
		y[i] = v >> wasted
	}
	return y
}

func (b *bw) constant(v int32, bps, wasted int) {
	subHeader(b, 0, wasted)
	b.signed(v>>wasted, bps-wasted)
}

func (b *bw) verbatim(x []int32, bps, wasted int) {
	subHeader(b, 1, wasted)
	for _, v := range shiftDown(x, wasted) {
		b.signed(v, bps-wasted)
	}
}

// bestParams picks a mean-based Rice parameter for each partition.
func bestParams(res []int32, predOrder, partOrder, total int) []int {
	out := make([]int, 1<<partOrder)
	pos := 0
	for p := range out {
		cnt := total >> partOrder
		if p == 0 {
			cnt -= predOrder
		}
		var sum uint64
		for _, v := range res[pos : pos+cnt] {
			sum += uint64(uint32(v<<1) ^ uint32(v>>31))
		}
		pos += cnt
		k := 0
		if cnt > 0 {
			k = max(0, bits.Len64(sum/uint64(cnt)+1)-1)
		}
		out[p] = k
	}
	return out
}

func (b *bw) fixed(x []int32, order, bps, wasted, method, partOrder int, params []int) {
	y := shiftDown(x, wasted)
	bp := bps - wasted
	subHeader(b, 8+order, wasted)
	for _, v := range y[:order] {
		b.signed(v, bp)
	}
	coefs := [][]int64{{}, {1}, {2, -1}, {3, -3, 1}, {4, -6, 4, -1}}[order]
	res := make([]int32, len(y)-order)
	for i := order; i < len(y); i++ {
		var pred int64
		for j, c := range coefs {
			pred += c * int64(y[i-1-j])
		}
		res[i-order] = y[i] - int32(pred)
	}
	if params == nil {
		params = bestParams(res, order, partOrder, len(y))
	}
	b.residual(res, order, method, partOrder, params, len(y))
}

func (b *bw) lpc(x []int32, coefs []int32, prec, shift, bps, wasted, method, partOrder int, params []int) {
	y := shiftDown(x, wasted)
	bp := bps - wasted
	order := len(coefs)
	subHeader(b, 32+order-1, wasted)
	for _, v := range y[:order] {
		b.signed(v, bp)
	}
	b.put(uint32(prec-1), 4)
	b.put(uint32(shift), 5)
	for _, c := range coefs {
		b.signed(c, prec)
	}
	res := make([]int32, len(y)-order)
	for i := order; i < len(y); i++ {
		var pred int64
		for j, c := range coefs {
			pred += int64(c) * int64(y[i-1-j])
		}
		res[i-order] = y[i] - int32(pred>>shift)
	}
	if params == nil {
		params = bestParams(res, order, partOrder, len(y))
	}
	b.residual(res, order, method, partOrder, params, len(y))
}

func utf8Num(n uint64) []byte {
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

// frame wraps subframe bits in a header and footer. srCode selects the
// sample-rate code (extra bytes appended when it needs them).
func frame(frameNum uint64, bsCode, blocksize, chCode, srCode int, subs func(b *bw)) []byte {
	var h []byte
	h = append(h, 0xff, 0xf8, byte(bsCode<<4|srCode), byte(chCode<<4|4<<1))
	h = append(h, utf8Num(frameNum)...)
	switch bsCode {
	case 6:
		h = append(h, byte(blocksize-1))
	case 7:
		h = binary.BigEndian.AppendUint16(h, uint16(blocksize-1))
	}
	switch srCode {
	case 12:
		h = append(h, 44)
	case 13, 14:
		h = append(h, 0xAC, 0x44)
	}
	h = append(h, tcrc8(h))
	b := newBW()
	subs(b)
	b.align()
	body := b.w.Bytes()
	out := append(h, body...)
	return binary.BigEndian.AppendUint16(out, tcrc16(out))
}

func toI32(s []int16, ch int) []int32 {
	out := make([]int32, len(s)/2)
	for i := range out {
		out[i] = int32(s[2*i+ch])
	}
	return out
}

func smooth(n int, seed uint64, mult int32) (l, r []int32) {
	rng := rand.New(rand.NewPCG(seed, 1))
	l, r = make([]int32, n), make([]int32, n)
	for i := range n {
		l[i] = (int32(9000*math.Sin(float64(i)*0.07)) + int32(rng.IntN(40)-20)) * mult
		r[i] = (int32(7000*math.Sin(float64(i)*0.045+1)) + int32(rng.IntN(40)-20)) * mult
	}
	return
}

func interleave(l, r []int32) []int16 {
	out := make([]int16, 2*len(l))
	for i := range l {
		out[2*i], out[2*i+1] = int16(l[i]), int16(r[i])
	}
	return out
}

func decodeExpect(t *testing.T, name string, stream []byte, want []int16) {
	t.Helper()
	out := make([]int16, len(want))
	used, err := Decode(stream, out, len(want)/2)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if used != len(stream) {
		t.Errorf("%s: consumed %d of %d", name, used, len(stream))
	}
	if !slices.Equal(out, want) {
		t.Fatalf("%s: samples differ", name)
	}
}

func TestDecodeHandBuiltStereoModes(t *testing.T) {
	const n = 256
	l, r := smooth(n, 1, 1)
	mid, side := make([]int32, n), make([]int32, n)
	for i := range n {
		mid[i] = (l[i] + r[i]) >> 1
		side[i] = l[i] - r[i]
	}
	want := interleave(l, r)
	lpcCoefs := []int32{5, -9, 7, -3} // arbitrary valid predictor, shift 2
	type enc func(b *bw, x []int32, bps int)
	encs := map[string]enc{
		"verbatim": func(b *bw, x []int32, bps int) { b.verbatim(x, bps, 0) },
		"fixed3":   func(b *bw, x []int32, bps int) { b.fixed(x, 3, bps, 0, 0, 3, nil) },
		"fixed1m1": func(b *bw, x []int32, bps int) { b.fixed(x, 1, bps, 0, 1, 0, nil) },
		"lpc4":     func(b *bw, x []int32, bps int) { b.lpc(x, lpcCoefs, 6, 2, bps, 0, 0, 2, nil) },
		"lpc4m1":   func(b *bw, x []int32, bps int) { b.lpc(x, lpcCoefs, 14, 9, bps, 0, 1, 1, nil) },
	}
	modes := []struct {
		name string
		code int
		a, b []int32
		bpsA int
		bpsB int
	}{
		{"independent", 1, l, r, 16, 16},
		{"left-side", 8, l, side, 16, 17},
		{"side-right", 9, side, r, 17, 16},
		{"mid-side", 10, mid, side, 16, 17},
	}
	for _, m := range modes {
		for en, e := range encs {
			fr := frame(0, 7, n, m.code, 9, func(b *bw) {
				e(b, m.a, m.bpsA)
				e(b, m.b, m.bpsB)
			})
			decodeExpect(t, m.name+"/"+en, fr, want)
		}
	}
}

func TestDecodeMidSideOddSide(t *testing.T) {
	// L=3,R=0 -> mid=1, side=3: the dropped low bit must come back from side.
	l := []int32{3, -3, 32767, -32768, 1, 0}
	r := []int32{0, 0, -32768, 32767, 2, 1}
	n := len(l)
	mid, side := make([]int32, n), make([]int32, n)
	for i := range n {
		mid[i] = (l[i] + r[i]) >> 1
		side[i] = l[i] - r[i]
	}
	fr := frame(0, 7, n, 10, 9, func(b *bw) {
		b.verbatim(mid, 16, 0)
		b.verbatim(side, 17, 0)
	})
	decodeExpect(t, "ms", fr, interleave(l, r))
}

func TestDecodeRiceEscapes(t *testing.T) {
	const n = 64
	l, r := smooth(n, 3, 1)
	want := interleave(l, r)
	// order 0 fixed: residual == samples; 4 partitions of 16. Make partition
	// 0 a raw escape (width 15), partition 2 too, others Rice.
	for _, method := range []int{0, 1} {
		fr := frame(0, 7, n, 1, 9, func(b *bw) {
			b.fixed(l, 0, 16, 0, method, 2, []int{-16, 12, -16, 12})
			b.fixed(r, 0, 16, 0, method, 2, []int{12, -15, 12, -16})
		})
		decodeExpect(t, "escape", fr, want)
	}
	// Zero-width escape: a constant-zero tail.
	z := make([]int32, n)
	for i := range 16 {
		z[i] = l[i]
	}
	fr := frame(0, 7, n, 1, 9, func(b *bw) {
		b.fixed(z, 0, 16, 0, 0, 2, []int{-16, -1, -1, -1})
		b.fixed(z, 0, 16, 0, 1, 2, []int{-16, -1, -1, -1})
	})
	decodeExpect(t, "zero-escape", fr, interleave(z, z))
}

func TestDecodeWastedBits(t *testing.T) {
	const n = 128
	l, r := smooth(n, 5, 4) // multiples of 4
	l8, _ := smooth(n, 6, 8)
	want := interleave(l, r)
	fr := frame(0, 7, n, 1, 9, func(b *bw) {
		b.verbatim(l, 16, 2)
		b.fixed(r, 2, 16, 2, 0, 1, nil)
	})
	decodeExpect(t, "wasted-2", fr, want)
	fr = frame(0, 7, n, 1, 9, func(b *bw) {
		b.lpc(l8, []int32{3, -3, 1}, 5, 0, 16, 3, 0, 2, nil)
		b.constant(-4096, 16, 3)
	})
	c := make([]int32, n)
	for i := range c {
		c[i] = -4096
	}
	decodeExpect(t, "wasted-3 lpc+const", fr, interleave(l8, c))
	// Wasted bits on a side channel (bps 17 - 1).
	mid, side := make([]int32, n), make([]int32, n)
	for i := range n {
		side[i] = (l[i] - r[i]) &^ 1
		mid[i] = (l[i] + r[i]) >> 1
	}
	// Rebuild l/r from mid/side the way a decoder must.
	ll, rr := make([]int32, n), make([]int32, n)
	for i := range n {
		m := mid[i]<<1 | side[i]&1
		ll[i], rr[i] = (m+side[i])>>1, (m-side[i])>>1
	}
	fr = frame(0, 7, n, 10, 9, func(b *bw) {
		b.verbatim(mid, 16, 0)
		b.verbatim(side, 17, 1)
	})
	decodeExpect(t, "wasted side", fr, interleave(ll, rr))
}

func TestDecodeHeaderVariants(t *testing.T) {
	// Several frames with different block-size codes, sample-rate codes and
	// multi-byte UTF-8 frame numbers, in one stream.
	type fv struct {
		num              uint64
		bsCode, size, sr int
	}
	vars := []fv{
		{0, 1, 192, 9}, {1, 2, 576, 0}, {127, 3, 1152, 12}, {128, 6, 100, 13},
		{300, 7, 777, 14}, {70000, 8, 256, 1}, {2000000, 12, 4096, 9}, {1 << 28, 10, 1024, 9},
	}
	var stream []byte
	var want []int16
	for i, v := range vars {
		l, r := smooth(v.size, uint64(i), 1)
		want = append(want, interleave(l, r)...)
		stream = append(stream, frame(v.num, v.bsCode, v.size, 1, v.sr, func(b *bw) {
			b.fixed(l, 2, 16, 0, 0, 0, nil)
			b.lpc(r, []int32{2, -1}, 4, 0, 16, 0, 0, 0, nil)
		})...)
	}
	decodeExpect(t, "headers", stream, want)
}

func TestDecodeRejectsBadFrames(t *testing.T) {
	const n = 64
	l, r := smooth(n, 1, 1)
	good := frame(0, 7, n, 1, 9, func(b *bw) { b.verbatim(l, 16, 0); b.verbatim(r, 16, 0) })
	out := make([]int16, 2*n)
	if _, err := Decode(good, out, n); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func([]byte){
		"crc8":      func(b []byte) { b[4] ^= 1 },
		"crc16":     func(b []byte) { b[len(b)-1] ^= 1 },
		"sync":      func(b []byte) { b[1] = 0x00 },
		"truncated": nil,
	} {
		bad := slices.Clone(good)
		if mut == nil {
			bad = bad[:len(bad)-10]
		} else {
			mut(bad)
		}
		if _, err := Decode(bad, out, n); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// Three channels are not stereo.
	f3 := frame(0, 7, n, 2, 9, func(b *bw) {
		b.verbatim(l, 16, 0)
		b.verbatim(r, 16, 0)
		b.verbatim(r, 16, 0)
	})
	if _, err := Decode(f3, out, n); err == nil {
		t.Error("3-channel frame accepted")
	}
}

// ---- libFLAC cross-checks, when the flac tool exists ----

func TestAgainstFlacTool(t *testing.T) {
	flacBin, err := exec.LookPath("flac")
	if err != nil {
		t.Skip("flac tool not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(flacBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("flac %v: %v\n%s", args, err, out)
		}
	}
	wav := func(s []int16) []byte {
		data := make([]byte, 2*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
		}
		h := []byte("RIFF")
		h = binary.LittleEndian.AppendUint32(h, uint32(36+len(data)))
		h = append(h, "WAVEfmt "...)
		h = binary.LittleEndian.AppendUint32(h, 16)
		h = binary.LittleEndian.AppendUint16(h, 1)
		h = binary.LittleEndian.AppendUint16(h, 2)
		h = binary.LittleEndian.AppendUint32(h, 44100)
		h = binary.LittleEndian.AppendUint32(h, 44100*4)
		h = binary.LittleEndian.AppendUint16(h, 4)
		h = binary.LittleEndian.AppendUint16(h, 16)
		h = append(h, "data"...)
		h = binary.LittleEndian.AppendUint32(h, uint32(len(data)))
		return append(h, data...)
	}
	// stripMeta returns the audio frames of a .flac file.
	stripMeta := func(b []byte) []byte {
		pos := 4
		for {
			last := b[pos]&0x80 != 0
			l := int(b[pos+1])<<16 | int(b[pos+2])<<8 | int(b[pos+3])
			pos += 4 + l
			if last {
				return b[pos:]
			}
		}
	}
	for _, bs := range []int{576, 1024, 2048, 4096} {
		for name, s := range signals(bs*3 + 101) {
			in := filepath.Join(dir, "in.wav")
			fl := filepath.Join(dir, "x.flac")
			os.WriteFile(in, wav(s), 0o644)
			run("-s", "-f", "-8", "-l", "12", "--lax", "-b", strconv.Itoa(bs), "--no-padding", "--no-seektable", "-o", fl, in)
			b, _ := os.ReadFile(fl)
			frames := stripMeta(b)
			out := make([]int16, len(s))
			used, err := Decode(frames, out, bs)
			if err != nil {
				t.Fatalf("libFLAC stream %s/%d: %v", name, bs, err)
			}
			if used != len(frames) || !slices.Equal(out, s) {
				t.Fatalf("libFLAC stream %s/%d: used %d/%d equal=%v", name, bs, used, len(frames), slices.Equal(out, s))
			}
			// And the reverse: libFLAC must accept our frames.
			enc, _ := Encode(s, bs)
			hdr := mameHeader(bs)
			ours := filepath.Join(dir, "ours.flac")
			os.WriteFile(ours, append(hdr, enc...), 0o644)
			back := filepath.Join(dir, "back.wav")
			run("-s", "-f", "-d", "-o", back, ours)
			w, _ := os.ReadFile(back)
			if !bytes.Equal(w[44:], wav(s)[44:]) {
				t.Fatalf("libFLAC decode of our stream differs: %s/%d", name, bs)
			}
		}
	}
	// Mixed material that makes libFLAC choose LPC, wasted bits and all stereo modes.
	mix := make([]int16, 2*8192)
	rng := rand.New(rand.NewPCG(4, 4))
	for i := range 8192 {
		base := 8000 * math.Sin(float64(i)*0.02)
		switch {
		case i < 2048: // identical channels: left-side / mid-side
			mix[2*i], mix[2*i+1] = int16(base), int16(base)
		case i < 4096: // multiples of 16: wasted bits
			mix[2*i], mix[2*i+1] = int16(base)&^15, int16(base/2)&^15
		case i < 6144: // right = -left
			mix[2*i], mix[2*i+1] = int16(base)+int16(rng.IntN(8)), -int16(base)
		default:
			mix[2*i], mix[2*i+1] = int16(base+float64(rng.IntN(200))), int16(3000*math.Sin(float64(i)*0.3))
		}
	}
	in := filepath.Join(dir, "mix.wav")
	fl := filepath.Join(dir, "mix.flac")
	os.WriteFile(in, wav(mix), 0o644)
	run("-s", "-f", "-8", "--lax", "-b", "1024", "--no-padding", "-o", fl, in)
	b, _ := os.ReadFile(fl)
	frames := stripMeta(b)
	out := make([]int16, len(mix))
	if used, err := Decode(frames, out, 1024); err != nil || used != len(frames) || !slices.Equal(out, mix) {
		t.Fatalf("mixed libFLAC stream: used=%d/%d err=%v", used, len(frames), err)
	}
}

// mameHeader builds the 42-byte stream header chdman synthesizes.
func mameHeader(bs int) []byte {
	h := []byte{
		0x66, 0x4C, 0x61, 0x43, 0x80, 0, 0, 0x22,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0x0A, 0xC4, 0x42, 0xF0, 0, 0, 0, 0,
	}
	h = append(h, make([]byte, 16)...)
	binary.BigEndian.PutUint16(h[8:], uint16(bs))
	binary.BigEndian.PutUint16(h[10:], uint16(bs))
	return h
}
