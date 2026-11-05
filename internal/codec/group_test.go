package codec

import (
	"math"
	"testing"

	"github.com/lcmps/chdman-go/internal/format"
)

// countingCompressor records how often a group calls a codec slot.
type countingCompressor struct {
	output []byte
	calls  int
}

func (c *countingCompressor) Compress(dst, _ []byte) ([]byte, error) {
	c.calls++
	return append(dst[:0], c.output...), nil
}

const groupTestFrames = 8

// dataFrame fills one 2448-byte frame like a data sector: the 12-byte sync
// pattern, then compressible content.
func dataFrame(frame []byte) {
	sync := []byte{0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0}
	copy(frame, sync)
	for i := len(sync); i < 2352; i++ {
		frame[i] = byte(i % 7)
	}
}

// audioFrame fills one frame with big-endian 16-bit stereo sine samples, as a
// CHD stores CD audio. position is the first sample's index in the hunk.
func audioFrame(frame []byte, position int) {
	for i := 0; i+3 < 2352; i += 4 {
		v := int16(12000 * math.Sin(float64(position+i/4)/30))
		frame[i], frame[i+1] = byte(uint16(v)>>8), byte(v)
		frame[i+2], frame[i+3] = byte(uint16(v)>>8), byte(v)
	}
}

// groupTestHunk builds an 8-frame hunk: audioFrames frames of audio at the
// end, data sectors before them.
func groupTestHunk(audioFrames int) []byte {
	hunk := make([]byte, groupTestFrames*2448)
	for f := range groupTestFrames {
		frame := hunk[f*2448 : (f+1)*2448]
		if f >= groupTestFrames-audioFrames {
			audioFrame(frame, f*588)
		} else {
			dataFrame(frame)
		}
	}
	return hunk
}

func TestGroupFLACOnlyRunsWhereItCanHelp(t *testing.T) {
	cdDefault := [4]format.CodecTag{format.CodecCDLZMA, format.CodecCDZlib, format.CodecCDFLAC}
	cases := []struct {
		name        string
		tags        [4]format.CodecTag
		flacSlot    int
		hunk        []byte
		wantFLACRun int
	}{
		{"all data sectors skip FLAC", cdDefault, 2, groupTestHunk(0), 0},
		{"all audio runs FLAC", cdDefault, 2, groupTestHunk(8), 1},
		{"one audio frame among data runs FLAC", cdDefault, 2, groupTestHunk(1), 1},
		{"FLAC as the only codec still runs on data", [4]format.CodecTag{format.CodecCDFLAC}, 0, groupTestHunk(0), 1},
		{"FLAC in the first slot is skipped on data too",
			[4]format.CodecTag{format.CodecCDFLAC, format.CodecCDZlib}, 0, groupTestHunk(0), 0},
		{"generic FLAC is never skipped",
			[4]format.CodecTag{format.CodecZlib, format.CodecFLAC}, 1, groupTestHunk(0), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewGroup(tc.tags, groupTestFrames*2448)
			if err != nil {
				t.Fatal(err)
			}
			spy := &countingCompressor{output: []byte{1, 2, 3}}
			g.compressors[tc.flacSlot] = spy
			if _, _, err := g.Best(tc.hunk); err != nil {
				t.Fatal(err)
			}
			if spy.calls != tc.wantFLACRun {
				t.Fatalf("FLAC ran %d times, want %d", spy.calls, tc.wantFLACRun)
			}
		})
	}
}

// A slot that stops running must not disturb which slot number a result gets,
// or the map would point hunks at the wrong codec.
func TestGroupKeepsSlotNumbersWhenFLACIsSkipped(t *testing.T) {
	tags := [4]format.CodecTag{format.CodecCDFLAC, format.CodecCDZlib}
	g, err := NewGroup(tags, groupTestFrames*2448)
	if err != nil {
		t.Fatal(err)
	}
	mapType, data, err := g.Best(groupTestHunk(0))
	if err != nil {
		t.Fatal(err)
	}
	if mapType != 1 {
		t.Fatalf("map type %d, want slot 1 (cdzl)", mapType)
	}
	dec, err := NewDecompressor(format.CodecCDZlib, groupTestFrames*2448)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, groupTestFrames*2448)
	if err := dec.Decompress(got, data); err != nil {
		t.Fatalf("slot 1's data does not decode as cdzl: %v", err)
	}
}
