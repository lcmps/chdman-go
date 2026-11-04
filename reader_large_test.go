package chdman

import (
	"bytes"
	"io"
	"testing"

	"github.com/lcmps/chdman-go/internal/format"
)

// zeroCHD builds an uncompressed CHD of logical bytes that are all zero. Its
// map has no stored data, so the file stays small however big the disc is.
func zeroCHD(t testing.TB, logical uint64, hunk uint32) []byte {
	t.Helper()
	h := format.Header{LogicalBytes: logical, HunkBytes: hunk, UnitBytes: 2048, MapOffset: format.HeaderSize}
	hdr := h.Marshal()
	return append(hdr[:], make([]byte, 4*h.HunkCount())...)
}

// ReadAt must not truncate 64-bit sizes to int: on a 32-bit build, an image
// over 2 GiB made the slice bound negative. Run with GOARCH=386 to see it.
func TestReadAtOver2GiB(t *testing.T) {
	const hunk = 64 << 10
	logical := uint64(3<<30) + 5*hunk
	file := zeroCHD(t, logical, hunk)
	r, err := NewReader(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	buf := bytes.Repeat([]byte{0xAA}, 3*hunk)
	for _, off := range []int64{0, 2<<30 - 1, 2 << 30, 3 << 30, int64(logical) - hunk - 7} {
		n, err := r.ReadAt(buf[:hunk+1], off)
		if err != nil || n != hunk+1 {
			t.Fatalf("ReadAt(off %d) = %d, %v", off, n, err)
		}
		if !bytes.Equal(buf[:hunk+1], make([]byte, hunk+1)) {
			t.Fatalf("ReadAt(off %d) is not zero", off)
		}
	}
	// A read spanning the end gets the remainder and io.EOF.
	n, err := r.ReadAt(buf, int64(logical)-hunk-7)
	if err != io.EOF || n != hunk+7 {
		t.Fatalf("read across the end = %d, %v; want %d, EOF", n, err, hunk+7)
	}
	if n, err = r.ReadAt(buf, int64(logical)); err != io.EOF || n != 0 {
		t.Fatalf("read at the end = %d, %v", n, err)
	}
}
