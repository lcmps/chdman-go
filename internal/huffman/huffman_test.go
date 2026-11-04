package huffman

import (
	"bytes"
	"errors"
	"github.com/lcmps/chdman-go/internal/chderr"
	"math/rand"
	"testing"

	"github.com/lcmps/chdman-go/internal/bitstream"
)

func inputs() map[string][]byte {
	rng := rand.New(rand.NewSource(1))
	random := make([]byte, 4096)
	rng.Read(random)
	skewed := make([]byte, 4096)
	for i := range skewed {
		// geometric-ish distribution over all 256 symbols
		v := 0
		for v < 255 && rng.Intn(2) == 0 {
			v++
		}
		skewed[i] = byte(v)
	}
	one := bytes.Repeat([]byte{7}, 100)
	return map[string][]byte{
		"zeros": make([]byte, 4096), "random": random, "skewed": skewed,
		"single": one, "empty": {},
	}
}

func TestDecode8RoundTrip(t *testing.T) {
	for name, in := range inputs() {
		t.Run(name, func(t *testing.T) {
			enc, err := Encode8(in)
			if err != nil {
				t.Fatal(err)
			}
			out := make([]byte, len(in))
			if err := Decode8(enc, out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(in, out) {
				t.Fatal("round trip mismatch")
			}
		})
	}
}

func TestDecode8TruncatedInput(t *testing.T) {
	in := inputs()["random"]
	enc, err := Encode8(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := Decode8(enc[:len(enc)/2], make([]byte, len(in))); err == nil {
		t.Fatal("expected error for truncated input")
	}
}

func TestCanonicalCodes(t *testing.T) {
	c := New(4, 8)
	for sym, n := range []int{4, 2, 1, 1} {
		for i := 0; i < n; i++ {
			c.HistoOne(uint32(sym))
		}
	}
	if err := c.ComputeTree(); err != nil {
		t.Fatal(err)
	}
	w := bitstream.NewWriter()
	for sym := uint32(0); sym < 4; sym++ {
		c.Encode(w, sym)
	}
	// lengths 1,2,3,3 -> codes 1, 01, 000, 001
	if got := w.Bytes(); !bytes.Equal(got, []byte{0xA0, 0x80}) {
		t.Fatalf("bytes = %08b", got)
	}
}

func buildTree(t *testing.T, numCodes, maxBits int, syms []uint32) *Context {
	t.Helper()
	c := New(numCodes, maxBits)
	for _, s := range syms {
		c.HistoOne(s)
	}
	if err := c.ComputeTree(); err != nil {
		t.Fatal(err)
	}
	return c
}

func roundTripTree(t *testing.T, numCodes, maxBits int, export func(*Context, *bitstream.Writer) error, imp func(*Context, *bitstream.Reader) error) {
	rng := rand.New(rand.NewSource(2))
	syms := make([]uint32, 5000)
	for i := range syms {
		v := uint32(0)
		for int(v) < numCodes-1 && rng.Intn(2) == 0 {
			v++
		}
		syms[i] = v
	}
	enc := buildTree(t, numCodes, maxBits, syms)

	tw := bitstream.NewWriter()
	if err := export(enc, tw); err != nil {
		t.Fatal(err)
	}
	dec := New(numCodes, maxBits)
	if err := imp(dec, bitstream.NewReader(tw.Bytes())); err != nil {
		t.Fatal(err)
	}

	dw := bitstream.NewWriter()
	for _, s := range syms {
		enc.Encode(dw, s)
	}
	r := bitstream.NewReader(dw.Bytes())
	for i, s := range syms {
		if got := dec.Decode(r); got != s {
			t.Fatalf("symbol %d: got %d want %d", i, got, s)
		}
	}
}

func TestTreeRLERoundTrip(t *testing.T) {
	for _, tc := range []struct{ codes, bits int }{{16, 8}, {256, 16}, {32, 6}} {
		roundTripTree(t, tc.codes, tc.bits,
			(*Context).ExportTreeRLE, (*Context).ImportTreeRLE)
	}
}

func TestTreeHuffmanRoundTrip(t *testing.T) {
	for _, tc := range []struct{ codes, bits int }{{16, 8}, {256, 16}} {
		roundTripTree(t, tc.codes, tc.bits,
			(*Context).ExportTreeHuffman, (*Context).ImportTreeHuffman)
	}
}

func TestNewPanicsOverMaxBits(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	New(256, 25)
}

func TestImportRejectsInvalidTrees(t *testing.T) {
	t.Run("rle oversubscribed", func(t *testing.T) {
		c := New(256, 8)
		err := c.ImportTreeRLE(bitstream.NewReader(bytes.Repeat([]byte{0x11}, 16)))
		if !errors.Is(err, chderr.CorruptData) {
			t.Fatalf("err = %v, want CorruptData", err)
		}
	})
	t.Run("three codes of length one", func(t *testing.T) {
		c := New(8, 8)
		for i := 0; i < 3; i++ {
			c.nodes[i].numBits = 1
		}
		if err := c.assignCanonicalCodes(); !errors.Is(err, chderr.CorruptData) {
			t.Fatalf("err = %v, want CorruptData", err)
		}
	})
}

func TestImportNeverPanicsOnGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		b := make([]byte, 1+rng.Intn(64))
		rng.Read(b)
		for _, imp := range []func(*Context, *bitstream.Reader) error{
			(*Context).ImportTreeRLE, (*Context).ImportTreeHuffman,
		} {
			c := New(256, 16)
			if err := imp(c, bitstream.NewReader(b)); err == nil {
				c.Decode(bitstream.NewReader(b))
			}
		}
	}
}
