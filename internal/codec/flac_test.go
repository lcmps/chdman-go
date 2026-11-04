package codec

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"

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
