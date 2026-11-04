package codec

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/flac"
	"github.com/lcmps/chdman-go/internal/format"
)

const (
	frameSize = 2448 // 2352 sector bytes + 96 subcode bytes
	cdHunk    = 8 * frameSize
)

var syncHeader = []byte{0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0}

// cdHunkShapes builds hunks of frames sectors mixing ECC-valid data sectors,
// sync-prefixed sectors with bad ECC, audio-like sectors and zeros, all with
// non-zero subcode.
func cdHunkShapes(frames int) map[string][]byte {
	rng := rand.New(rand.NewPCG(11, 12))
	mixed := make([]byte, frames*frameSize)
	for f := range frames {
		fr := mixed[f*frameSize : (f+1)*frameSize]
		sector := fr[:cdrom.SectorSize]
		switch f % 4 {
		case 0: // valid mode 1
			copy(sector, syncHeader)
			sector[15] = 1
			for i := 16; i < 2064; i++ {
				sector[i] = byte(rng.Uint32())
			}
			cdrom.ECCGenerate(sector)
		case 1: // sync header but wrong ECC
			copy(sector, syncHeader)
			sector[15] = 1
			for i := 16; i < cdrom.SectorSize; i++ {
				sector[i] = byte(rng.Uint32())
			}
			if cdrom.ECCVerify(sector) {
				sector[cdrom.SectorSize-1] ^= 1
			}
		case 2: // audio-like
			for i := 0; i < cdrom.SectorSize; i += 4 {
				binary.BigEndian.PutUint16(sector[i:], uint16(int16(8000*math.Sin(float64(f*600+i)*0.01))))
				binary.BigEndian.PutUint16(sector[i+2:], uint16(int16(rng.IntN(64))))
			}
		}
		for i := cdrom.SectorSize; i < frameSize; i++ {
			fr[i] = byte(i*7 + f)
		}
	}
	random := make([]byte, frames*frameSize)
	for i := range random {
		random[i] = byte(rng.Uint32())
	}
	return map[string][]byte{"mixed": mixed, "random": random, "zeros": make([]byte, frames*frameSize)}
}

func TestCDCodecRoundTrip(t *testing.T) {
	tags := []format.CodecTag{format.CodecCDZlib, format.CodecCDZstd, format.CodecCDLZMA, format.CodecCDFLAC}
	for _, tag := range tags {
		for name, src := range cdHunkShapes(8) {
			t.Run(tag.String()+"/"+name, func(t *testing.T) {
				c, err := NewCompressor(tag, cdHunk)
				if err != nil {
					t.Fatal(err)
				}
				d, err := NewDecompressor(tag, cdHunk)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					comp, err := c.Compress(nil, src)
					if err != nil {
						if name == "random" {
							return // incompressible hunks are rejected, as in chdman
						}
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

func TestCDECCFlagsAndClearing(t *testing.T) {
	src := cdHunkShapes(8)["mixed"]
	c, _ := NewCompressor(format.CodecCDZlib, cdHunk)
	comp, err := c.Compress(nil, src)
	if err != nil {
		t.Fatal(err)
	}
	// Sectors 0 and 4 are ECC-valid; 1, 5 have a sync header but bad ECC.
	if comp[0] != 1<<0|1<<4 {
		t.Errorf("ecc flags = %08b", comp[0])
	}
}

func TestCDHeaderSizes(t *testing.T) {
	for _, c := range []struct{ frames, hunk, ecc, lenBytes int }{
		{8, 8 * frameSize, 1, 2},
		{27, 27 * frameSize, 4, 3}, // 66096 >= 65536
		{26, 26 * frameSize, 4, 2}, // 63648 < 65536
		{1, frameSize, 1, 2},
	} {
		if e, l := cdHeaderSizes(c.frames, uint32(c.hunk)); e != c.ecc || l != c.lenBytes {
			t.Errorf("cdHeaderSizes(%d,%d) = %d,%d want %d,%d", c.frames, c.hunk, e, l, c.ecc, c.lenBytes)
		}
	}
	// And the real stream follows it: the base length sits after the flags.
	for _, frames := range []int{8, 27} {
		hunk := frames * frameSize
		src := cdHunkShapes(frames)["mixed"]
		c, _ := NewCompressor(format.CodecCDZlib, uint32(hunk))
		comp, err := c.Compress(nil, src)
		if err != nil {
			t.Fatal(err)
		}
		ecc, lb := cdHeaderSizes(frames, uint32(hunk))
		var base int
		if lb == 3 {
			base = int(comp[ecc])<<16 | int(comp[ecc+1])<<8 | int(comp[ecc+2])
		} else {
			base = int(binary.BigEndian.Uint16(comp[ecc:]))
		}
		d, _ := NewDecompressor(format.CodecCDZlib, uint32(hunk))
		out := make([]byte, hunk)
		if err := d.Decompress(out, comp); err != nil || !bytes.Equal(out, src) {
			t.Fatalf("frames=%d: round trip: %v", frames, err)
		}
		// The base part must itself be a deflate stream of the audio area.
		dd := newDeflateDecompressor()
		buf := make([]byte, frames*cdrom.SectorSize)
		if err := dd.Decompress(buf, comp[ecc+lb:ecc+lb+base]); err != nil {
			t.Fatalf("frames=%d: base part: %v", frames, err)
		}
		sub := make([]byte, frames*96)
		if err := dd.Decompress(sub, comp[ecc+lb+base:]); err != nil {
			t.Fatalf("frames=%d: subcode part: %v", frames, err)
		}
		// Subcode of frame 0 is byte(i*7+0) for i in 2352..2447.
		if sub[0] != byte(cdrom.SectorSize*7%256) {
			t.Errorf("frames=%d: subcode[0] = %d", frames, sub[0])
		}
	}
}

func TestCDFLACLayout(t *testing.T) {
	// Audio area is FLAC of big-endian samples followed by raw-deflated subcode.
	src := make([]byte, cdHunk)
	for f := range 8 {
		copy(src[f*frameSize:], audioHunk(cdrom.SectorSize, uint64(f)))
	}
	var want []int16
	for f := range 8 {
		sector := src[f*frameSize : f*frameSize+cdrom.SectorSize]
		for i := 0; i < len(sector); i += 2 {
			want = append(want, int16(binary.BigEndian.Uint16(sector[i:])))
		}
	}
	c, _ := NewCompressor(format.CodecCDFLAC, cdHunk)
	comp, err := c.Compress(nil, src)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]int16, len(want))
	used, err := flac.Decode(comp, samples, 2352)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(samples, want) {
		t.Fatal("FLAC part is not the big-endian sample stream")
	}
	sub := make([]byte, 8*96)
	if err := newDeflateDecompressor().Decompress(sub, comp[used:]); err != nil {
		t.Fatalf("subcode after FLAC: %v", err)
	}
}
