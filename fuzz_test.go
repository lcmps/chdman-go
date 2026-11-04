package chdman

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// exercise opens data, reads all of it and verifies it. It must never panic.
func exercise(data []byte) (openErr, useErr error) {
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err, nil
	}
	defer r.Close()
	n := r.Info().LogicalBytes
	buf := make([]byte, min(n, 1<<20))
	for off := uint64(0); off < n && useErr == nil; off += uint64(len(buf)) {
		_, useErr = r.ReadAt(buf, int64(off))
	}
	if _, err := r.Verify(context.Background(), VerifyOptions{Workers: 2}); useErr == nil {
		useErr = err
	}
	return nil, useErr
}

// seedFiles returns small chdman-made CHDs, one per codec.
func seedFiles(t testing.TB) [][]byte {
	var out [][]byte
	for _, c := range []string{"zlib", "zstd", "lzma", "huff", "flac", "none", ""} {
		chd, _ := makeDVD(t, testimg.Mixed, 24, c, 4096)
		b, err := os.ReadFile(chd)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func FuzzOpenAndRead(f *testing.F) {
	for _, b := range seedFiles(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		exercise(data)
	})
}

func TestTruncatedFiles(t *testing.T) {
	for i, b := range seedFiles(t) {
		for n := 0; n < len(b); n += 64 {
			openErr, useErr := exercise(b[:n])
			if openErr == nil && useErr == nil {
				t.Fatalf("seed %d truncated to %d of %d bytes: no error", i, n, len(b))
			}
		}
	}
}

func TestHostileLengths(t *testing.T) {
	t.Run("rle map", hostileRLEMap)
	huge := uint64(1) << 40
	for name, patch := range map[string]func(b []byte, h format.Header){
		"logical bytes": func(b []byte, h format.Header) { binary.BigEndian.PutUint64(b[32:], huge) },
		"map length": func(b []byte, h format.Header) {
			if h.Compressed() {
				binary.BigEndian.PutUint32(b[h.MapOffset:], 0xffffff00)
			}
		},
		"both": func(b []byte, h format.Header) {
			binary.BigEndian.PutUint64(b[32:], huge)
			if h.Compressed() {
				binary.BigEndian.PutUint32(b[h.MapOffset:], 0xffffff00)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			for i, orig := range seedFiles(t) {
				b := bytes.Clone(orig)
				h, err := format.ParseHeader(b)
				if err != nil {
					t.Fatal(err)
				}
				patch(b, h)
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				openErr, useErr := exercise(b)
				runtime.ReadMemStats(&after)
				if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
					t.Errorf("seed %d: allocated %d MiB", i, alloc>>20)
				}
				if name != "map length" || h.Compressed() {
					if err := firstErr(openErr, useErr); !isCorrupt(err) {
						t.Errorf("seed %d: error %v, want ErrCorruptData", i, err)
					}
				}
			}
		})
	}
}

// A few kilobytes of RLE map can describe millions of hunks; opening and
// reading such a file must not commit memory in proportion to the hunks.
func hostileRLEMap(t *testing.T) {
	const hunks = 4_000_000
	entries := make([]format.MapEntry, hunks)
	entries[0] = format.MapEntry{Type: format.CompType0, CompLen: 16}
	for i := 1; i < hunks; i++ {
		entries[i] = format.MapEntry{Type: format.CompSelf}
	}
	h := format.Header{
		Codecs:       [4]format.CodecTag{format.CodecZlib},
		LogicalBytes: hunks * 2048, HunkBytes: 2048, UnitBytes: 2048,
		MapOffset: format.HeaderSize,
	}
	m, err := format.EncodeCompressedMap(entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := uint64(format.HeaderSize + len(m))
	entries[0].Offset = first
	if m, err = format.EncodeCompressedMap(entries, first); err != nil {
		t.Fatal(err)
	}
	entries = nil
	hdr := h.Marshal()
	file := append(append(hdr[:], m...), make([]byte, 16)...) // hunk 0 is not valid zlib
	if len(file) > 64<<10 {
		t.Fatalf("file is %d bytes", len(file))
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	openErr, useErr := exercise(file)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("allocated %d MiB for a %d byte file", alloc>>20, len(file))
	}
	if err := firstErr(openErr, useErr); !isCorrupt(err) {
		t.Errorf("error %v, want ErrCorruptData", err)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func isCorrupt(err error) bool { return errors.Is(err, ErrCorruptData) }
