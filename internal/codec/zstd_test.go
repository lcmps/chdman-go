package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/lcmps/chdman-go/internal/chderr"
)

func TestZstdBoundsAllocationByDestination(t *testing.T) {
	// A frame header declaring an 8 GiB content size, followed by nothing.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0xc0, 0x00}
	frame = binary.LittleEndian.AppendUint64(frame, 8<<30)
	frame = append(frame, 0x01, 0x00, 0x00)

	d, err := newZstdDecompressor()
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err = d.Decompress(make([]byte, 4096), frame)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, chderr.CorruptData) {
		t.Fatalf("err = %v, want CorruptData", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 256<<20 {
		t.Fatalf("decoder allocated %d MiB for a 4 KiB destination", grew>>20)
	}
}

func TestZstdDecodesWithAndWithoutContentSize(t *testing.T) {
	want := bytes.Repeat([]byte("chd zstd "), 500)
	for name, opts := range map[string][]zstd.EOption{
		"with size":    nil,
		"without size": {zstd.WithZeroFrames(false)},
	} {
		t.Run(name, func(t *testing.T) {
			var frame []byte
			if name == "with size" {
				enc, _ := zstd.NewWriter(nil, opts...)
				frame = enc.EncodeAll(want, nil)
			} else {
				var buf bytes.Buffer
				enc, _ := zstd.NewWriter(&buf)
				enc.Write(want)
				enc.Close()
				frame = buf.Bytes()
			}
			d, err := newZstdDecompressor()
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if err := d.Decompress(got, frame); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("round trip mismatch")
			}
		})
	}
}
