package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
)

func TestFLACBlockSizes(t *testing.T) {
	for _, c := range []struct{ in, want uint32 }{{4096, 1024}, {65536, 2048}, {19584, 1224}} {
		if got := flacBlockSize(c.in); got != c.want {
			t.Errorf("flacBlockSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
	if got := cdFlacBlockSize(18816); got != 2352 {
		t.Errorf("cdFlacBlockSize(18816) = %d, want 2352", got)
	}
}

func audioHunk(n int, seed uint64) []byte {
	rng := rand.New(rand.NewPCG(seed, 5))
	b := make([]byte, n)
	for i := 0; i+3 < n; i += 4 {
		binary.LittleEndian.PutUint16(b[i:], uint16(int16(10000*math.Sin(float64(i)*0.004))))
		binary.LittleEndian.PutUint16(b[i+2:], uint16(int16(6000*math.Sin(float64(i)*0.0031)))+uint16(rng.IntN(16)))
	}
	return b
}

func TestFLACCodecRoundTrip(t *testing.T) {
	for _, size := range []uint32{4096, 19584, 65536} {
		c, err := NewCompressor(format.CodecFLAC, size)
		if err != nil {
			t.Fatal(err)
		}
		d, err := NewDecompressor(format.CodecFLAC, size)
		if err != nil {
			t.Fatal(err)
		}
		le := audioHunk(int(size), 1)
		be := make([]byte, len(le))
		for i := 0; i < len(le); i += 2 {
			be[i], be[i+1] = le[i+1], le[i]
		}
		for name, src := range map[string][]byte{"le": le, "be": be, "zeros": make([]byte, size)} {
			comp, err := c.Compress(nil, src)
			if err != nil {
				t.Fatalf("%d/%s: %v", size, name, err)
			}
			if comp[0] != 'L' && comp[0] != 'B' {
				t.Fatalf("%d/%s: lead byte %q", size, name, comp[0])
			}
			if name == "be" && comp[0] != 'B' {
				t.Errorf("%d/be: chose %q", size, comp[0])
			}
			if name == "le" && comp[0] != 'L' {
				t.Errorf("%d/le: chose %q", size, comp[0])
			}
			out := make([]byte, len(src))
			if err := d.Decompress(out, comp); err != nil {
				t.Fatalf("%d/%s: %v", size, name, err)
			}
			if !bytes.Equal(out, src) {
				t.Fatalf("%d/%s: mismatch", size, name)
			}
		}
		if err := d.Decompress(make([]byte, size), []byte{'X', 0}); err == nil {
			t.Error("bad endian byte accepted")
		}
	}
}

func TestFLACTruncationIsCorruptData(t *testing.T) {
	const size = 4096
	c, err := NewCompressor(format.CodecFLAC, size)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDecompressor(format.CodecFLAC, size)
	if err != nil {
		t.Fatal(err)
	}
	comp, err := c.Compress(nil, audioHunk(size, 3))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, size)
	if err := d.Decompress(out, nil); !errors.Is(err, chderr.CorruptData) {
		t.Errorf("empty: err = %v, want CorruptData", err)
	}
	if err := d.Decompress(out, comp[:1]); !errors.Is(err, chderr.CorruptData) {
		t.Errorf("marker only: err = %v, want CorruptData", err)
	}
	for n := 1; n < len(comp); n += 7 {
		if err := d.Decompress(out, comp[:n]); err != nil && !errors.Is(err, chderr.CorruptData) {
			t.Fatalf("prefix %d: err = %v, want CorruptData", n, err)
		}
	}
	if err := d.Decompress(out, comp[:len(comp)/2]); !errors.Is(err, chderr.CorruptData) {
		t.Errorf("half: err = %v, want CorruptData", err)
	}
}

func TestCDFLACTruncationIsCorruptData(t *testing.T) {
	hunk := uint32(cdFrameSize * 8)
	c, err := NewCompressor(format.CodecCDFLAC, hunk)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDecompressor(format.CodecCDFLAC, hunk)
	if err != nil {
		t.Fatal(err)
	}
	comp, err := c.Compress(nil, make([]byte, hunk))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, hunk)
	for _, n := range []int{0, 1, len(comp) / 2} {
		if err := d.Decompress(out, comp[:n]); !errors.Is(err, chderr.CorruptData) {
			t.Errorf("prefix %d: err = %v, want CorruptData", n, err)
		}
	}
}

func TestFLACCorruptionErrorsAreCorruptData(t *testing.T) {
	const size = 4096
	c, _ := NewCompressor(format.CodecFLAC, size)
	d, _ := NewDecompressor(format.CodecFLAC, size)
	comp, err := c.Compress(nil, audioHunk(size, 4))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, size)
	for i := 1; i < len(comp); i++ {
		bad := bytes.Clone(comp)
		bad[i] ^= 0xff
		if err := d.Decompress(out, bad); err != nil && !errors.Is(err, chderr.CorruptData) {
			t.Fatalf("flip byte %d: err = %v, want CorruptData", i, err)
		}
	}
}
