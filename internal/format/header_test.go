package format_test

import (
	. "github.com/lcmps/chdman-go/internal/format"

	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func chdmanHeader(t *testing.T) []byte {
	t.Helper()
	oracle.Require(t)
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 64, testimg.Zeros)
	chd := filepath.Join(dir, "out.chd")
	oracle.CreateDVD(t, iso, chd, "", 0)
	b, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	return b[:HeaderSize]
}

func TestParseHeaderFromChdman(t *testing.T) {
	h, err := ParseHeader(chdmanHeader(t))
	if err != nil {
		t.Fatal(err)
	}
	if h.HunkBytes != 4096 || h.UnitBytes != 2048 {
		t.Errorf("hunk/unit = %d/%d, want 4096/2048", h.HunkBytes, h.UnitBytes)
	}
	want := [4]CodecTag{CodecLZMA, CodecZlib, CodecHuffman, CodecFLAC}
	if h.Codecs != want {
		t.Errorf("codecs = %v, want %v", h.Codecs, want)
	}
	if !h.Compressed() || h.HunkCount() != 64*2048/4096 {
		t.Errorf("compressed=%v hunks=%d", h.Compressed(), h.HunkCount())
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	raw := chdmanHeader(t)
	h, err := ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Marshal(); !bytes.Equal(got[:], raw) {
		t.Fatalf("Marshal mismatch\n got %x\nwant %x", got, raw)
	}
}

func TestParseHeaderRejects(t *testing.T) {
	good := Header{
		Codecs: [4]CodecTag{CodecZlib}, LogicalBytes: 8192,
		HunkBytes: 4096, UnitBytes: 2048,
	}
	mutate := func(f func(b []byte)) []byte {
		m := good.Marshal()
		f(m[:])
		return m[:]
	}
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"bad magic", mutate(func(b []byte) { b[0] = 'X' }), chderr.CorruptData},
		{"short", []byte("MComprHD"), chderr.CorruptData},
		{"version 4", mutate(func(b []byte) { binary.BigEndian.PutUint32(b[12:], 4) }), chderr.UnsupportedVersion},
		{"parent", mutate(func(b []byte) { b[110] = 1 }), chderr.ParentRequired},
		{"zero hunk", mutate(func(b []byte) { binary.BigEndian.PutUint32(b[56:], 0) }), chderr.CorruptData},
		{"hunk count overflows uint32", mutate(func(b []byte) {
			binary.BigEndian.PutUint64(b[32:], 1<<40)
			binary.BigEndian.PutUint32(b[56:], 1)
		}), chderr.CorruptData},
		{"logical size wraps", mutate(func(b []byte) {
			binary.BigEndian.PutUint64(b[32:], math.MaxUint64)
		}), chderr.CorruptData},
		{"huge hunk", mutate(func(b []byte) { binary.BigEndian.PutUint32(b[56:], 0xFFFFF000) }), chderr.CorruptData},
		{"bad length", mutate(func(b []byte) { binary.BigEndian.PutUint32(b[8:], 108) }), chderr.CorruptData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseHeader(tt.in); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	gm := good.Marshal()
	if _, err := ParseHeader(gm[:]); err != nil {
		t.Fatalf("good header rejected: %v", err)
	}
}

func TestParseHeaderRejectsIndivisibleHunk(t *testing.T) {
	h := Header{Codecs: [4]CodecTag{CodecZlib}, LogicalBytes: 8192, HunkBytes: 4096, UnitBytes: 2048}
	raw := h.Marshal()
	if _, err := ParseHeader(raw[:]); err != nil {
		t.Fatal(err)
	}
	for _, hunk := range []uint32{1, 3000, 4097} {
		h.HunkBytes = hunk
		raw := h.Marshal()
		if _, err := ParseHeader(raw[:]); !errors.Is(err, chderr.CorruptData) {
			t.Errorf("hunk %d with unit 2048: err = %v, want CorruptData", hunk, err)
		}
	}
}
