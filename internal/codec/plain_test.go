package codec

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
)

const testHunk = 19584

func TestLZMADictSize(t *testing.T) {
	for _, c := range []struct{ in, want uint32 }{
		{4096, 4096}, {1000, 4096}, {18816, 18816}, {1 << 27, 1 << 26},
	} {
		if got := LZMADictSize(c.in); got != c.want {
			t.Errorf("LZMADictSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func shapes() map[string][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	random := make([]byte, testHunk)
	for i := range random {
		random[i] = byte(rng.Uint32())
	}
	return map[string][]byte{
		"zeros":  make([]byte, testHunk),
		"random": random,
		"text":   bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), testHunk/45+1)[:testHunk],
	}
}

func TestPlainRoundTrip(t *testing.T) {
	tags := []format.CodecTag{format.CodecZlib, format.CodecZstd, format.CodecLZMA, format.CodecHuffman}
	for _, tag := range tags {
		for name, src := range shapes() {
			t.Run(tag.String()+"/"+name, func(t *testing.T) {
				c, err := NewCompressor(tag, testHunk)
				if err != nil {
					t.Fatal(err)
				}
				d, err := NewDecompressor(tag, testHunk)
				if err != nil {
					t.Fatal(err)
				}
				// Twice, to prove instances reset between calls.
				for range 2 {
					comp, err := c.Compress(nil, src)
					if err != nil {
						t.Fatal(err)
					}
					out := make([]byte, len(src))
					if err := d.Decompress(out, comp); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(out, src) {
						t.Fatal("round trip mismatch")
					}
				}
			})
		}
	}
}

func TestUnsupportedCodec(t *testing.T) {
	for _, tag := range []format.CodecTag{format.CodecAVHuff, format.CodecNone, 0x12345678} {
		if _, err := NewCompressor(tag, testHunk); !errors.Is(err, chderr.UnsupportedCodec) {
			t.Errorf("NewCompressor(%v) err = %v", tag, err)
		}
		if _, err := NewDecompressor(tag, testHunk); !errors.Is(err, chderr.UnsupportedCodec) {
			t.Errorf("NewDecompressor(%v) err = %v", tag, err)
		}
	}
}

func TestLZMAStreamIsRaw(t *testing.T) {
	c, err := NewCompressor(format.CodecLZMA, testHunk)
	if err != nil {
		t.Fatal(err)
	}
	comp, err := c.Compress(nil, shapes()["text"])
	if err != nil {
		t.Fatal(err)
	}
	if comp[0] != 0x00 {
		t.Fatalf("first byte = %#x, want 0x00 (no classic header)", comp[0])
	}
}

func TestGroupPicksSmallest(t *testing.T) {
	tags := [4]format.CodecTag{format.CodecLZMA, format.CodecZlib, format.CodecHuffman, format.CodecZstd}
	g, err := NewGroup(tags, testHunk)
	if err != nil {
		t.Fatal(err)
	}
	src := shapes()["text"]
	typ, data, err := g.Best(src)
	if err != nil {
		t.Fatal(err)
	}
	if typ > format.CompType3 {
		t.Fatalf("type = %d, want a codec slot", typ)
	}
	for i, tag := range tags {
		c, _ := NewCompressor(tag, testHunk)
		other, err := c.Compress(nil, src)
		if err != nil {
			continue
		}
		if len(other) < len(data) {
			t.Errorf("slot %d (%v) gives %d bytes, smaller than chosen %d", i, tag, len(other), len(data))
		}
	}
	d, _ := NewDecompressor(tags[typ], testHunk)
	out := make([]byte, testHunk)
	if err := d.Decompress(out, data); err != nil || !bytes.Equal(out, src) {
		t.Fatalf("chosen result does not round trip: %v", err)
	}
}

func TestGroupFallsBackToNone(t *testing.T) {
	g, err := NewGroup([4]format.CodecTag{format.CodecZlib, format.CodecZstd, format.CodecLZMA, format.CodecHuffman}, testHunk)
	if err != nil {
		t.Fatal(err)
	}
	src := shapes()["random"]
	typ, data, err := g.Best(src)
	if err != nil {
		t.Fatal(err)
	}
	if typ != format.CompNone || !bytes.Equal(data, src) {
		t.Fatalf("type = %d, want CompNone with src", typ)
	}
}

func TestGroupSkipsEmptySlots(t *testing.T) {
	g, err := NewGroup([4]format.CodecTag{format.CodecZlib}, testHunk)
	if err != nil {
		t.Fatal(err)
	}
	typ, _, err := g.Best(shapes()["zeros"])
	if err != nil || typ != format.CompType0 {
		t.Fatalf("type = %d, err = %v", typ, err)
	}
}
