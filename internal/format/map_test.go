package format_test

import (
	. "github.com/lcmps/chdman-go/internal/format"

	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// chdmanFile creates a DVD CHD of a Mixed ISO with chdman and returns its
// bytes and parsed header. Empty codecs means the chdman default.
func chdmanFile(t *testing.T, codecs string, sectors int) ([]byte, Header) {
	return chdmanFileShape(t, codecs, sectors, testimg.Mixed)
}

func chdmanFileShape(t *testing.T, codecs string, sectors int, shape testimg.Shape) ([]byte, Header) {
	t.Helper()
	oracle.Require(t)
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, sectors, shape)
	chd := filepath.Join(dir, "out.chd")
	oracle.CreateDVD(t, iso, chd, codecs, 0)
	b, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	h, err := ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	return b, h
}

func TestReadMapFromChdman(t *testing.T) {
	b, h := chdmanFile(t, "", 512)
	m, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	entries := entriesOf(m)
	if m.Len() != h.HunkCount() {
		t.Fatalf("got %d entries, want %d", len(entries), h.HunkCount())
	}
	for i, e := range entries {
		switch e.Type {
		case CompType0, CompType1, CompType2, CompType3, CompNone:
			if e.Offset+uint64(e.CompLen) > uint64(len(b)) {
				t.Errorf("entry %d spans past file end: %+v", i, e)
			}
		case CompSelf:
			if e.Offset >= uint64(len(entries)) {
				t.Errorf("entry %d self reference out of range: %+v", i, e)
			}
		default:
			t.Errorf("entry %d has type %d", i, e.Type)
		}
	}
}

// withMap returns the file with enc appended as its map, and a matching header.
func withMap(b []byte, h Header, enc []byte) ([]byte, Header) {
	out := append(bytes.Clone(b), enc...)
	h.MapOffset = uint64(len(b))
	return out, h
}

func TestEncodeMapRoundTrip(t *testing.T) {
	for name, shape := range map[string]testimg.Shape{"mixed": testimg.Mixed, "zeros": testimg.Zeros} {
		t.Run(name, func(t *testing.T) {
			b, h := chdmanFileShape(t, "", 512, shape)
			m, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
			if err != nil {
				t.Fatal(err)
			}
			entries := entriesOf(m)
			if shape == testimg.Zeros && !slices.ContainsFunc(entries, func(e MapEntry) bool { return e.Type == CompSelf }) {
				t.Fatal("expected self references in an all-zero image")
			}
			firstOffset := binary.BigEndian.Uint64(b[h.MapOffset+4:]) >> 16
			enc, err := EncodeCompressedMap(entries, firstOffset)
			if err != nil {
				t.Fatal(err)
			}
			mapBytes := binary.BigEndian.Uint32(b[h.MapOffset:])
			if orig := b[h.MapOffset : h.MapOffset+16+uint64(mapBytes)]; !bytes.Equal(enc, orig) {
				t.Errorf("encoded map differs from chdman's\n got %x\nwant %x", enc, orig)
			}
			file, h2 := withMap(b, h, enc)
			got, err := ReadMap(bytes.NewReader(file), h2, int64(len(file)))
			if err != nil {
				t.Fatal(err)
			}
			assertEntries(t, entriesOf(got), entries)
		})
	}
}

// entriesOf returns every entry of m.
func entriesOf(m *Map) []MapEntry {
	out := make([]MapEntry, m.Len())
	for i := range out {
		out[i] = m.Entry(uint32(i))
	}
	return out
}

func assertEntries(t *testing.T, got, want []MapEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestEncodeMapSyntheticRuns covers every pseudo-type and the RLE forms.
func TestEncodeMapSyntheticRuns(t *testing.T) {
	const hunkBytes = 4096
	var entries []MapEntry
	off := uint64(1000)
	add := func(e MapEntry) {
		if e.Type <= CompNone {
			off += uint64(e.CompLen)
		}
		entries = append(entries, e)
	}
	for i := range 300 { // long run of one type: RLE large, split
		add(MapEntry{Type: CompType0, CompLen: uint32(100 + i%7), Offset: off, CRC16: uint16(i)})
	}
	for range 10 { // medium run: RLE small
		add(MapEntry{Type: CompNone, CompLen: hunkBytes, Offset: off, CRC16: 7})
	}
	add(MapEntry{Type: CompSelf, Offset: 5})
	add(MapEntry{Type: CompSelf, Offset: 5}) // SELF_0
	add(MapEntry{Type: CompSelf, Offset: 6}) // SELF_1
	add(MapEntry{Type: CompSelf, Offset: 7}) // SELF_1
	add(MapEntry{Type: CompSelf, Offset: 7})
	add(MapEntry{Type: CompSelf, Offset: 2})
	add(MapEntry{Type: CompType1, CompLen: 50, Offset: off, CRC16: 9})
	add(MapEntry{Type: CompType1, CompLen: 51, Offset: off, CRC16: 10})
	add(MapEntry{Type: CompType2, CompLen: 52, Offset: off, CRC16: 11})
	h := Header{Codecs: [4]CodecTag{CodecZlib, CodecLZMA, CodecHuffman, CodecFLAC}, HunkBytes: hunkBytes, UnitBytes: 2048, LogicalBytes: uint64(len(entries)) * hunkBytes}
	enc, err := EncodeCompressedMap(entries, 1000)
	if err != nil {
		t.Fatal(err)
	}
	file := make([]byte, off+uint64(len(enc)))
	copy(file[off:], enc)
	h.MapOffset = off
	got, err := ReadMap(bytes.NewReader(file), h, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entriesOf(got), entries)
}

func TestReadMapRejectsBadCRC(t *testing.T) {
	b, h := chdmanFile(t, "", 512)
	bad := bytes.Clone(b)
	bad[h.MapOffset+10] ^= 0xff // the stored map CRC lives at header bytes 10-11
	if _, err := ReadMap(bytes.NewReader(bad), h, int64(len(bad))); !errors.Is(err, chderr.CorruptData) {
		t.Fatalf("flipped map CRC: err = %v, want CorruptData", err)
	}
	bad = bytes.Clone(b)
	bad[h.MapOffset+16+20] ^= 0x10 // a byte of the compressed map body
	if _, err := ReadMap(bytes.NewReader(bad), h, int64(len(bad))); !errors.Is(err, chderr.CorruptData) {
		t.Fatalf("flipped body byte: err = %v, want CorruptData", err)
	}
}

func TestReadMapRejectsOversizedMap(t *testing.T) {
	b, h := chdmanFile(t, "", 64)
	bad := bytes.Clone(b)
	binary.BigEndian.PutUint32(bad[h.MapOffset:], 0xfffffff0)
	if _, err := ReadMap(bytes.NewReader(bad), h, int64(len(bad))); !errors.Is(err, chderr.CorruptData) {
		t.Fatalf("err = %v, want CorruptData", err)
	}
}

func TestUncompressedMap(t *testing.T) {
	b, h := chdmanFile(t, "none", 64)
	if h.Compressed() {
		t.Fatal("expected an uncompressed CHD")
	}
	m, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	entries := entriesOf(m)
	if m.Len() != h.HunkCount() {
		t.Fatalf("got %d entries, want %d", len(entries), h.HunkCount())
	}
	var zero, data int
	for i, e := range entries {
		if e.CompLen == 0 {
			zero++
			continue
		}
		data++
		if e.Type != CompNone || e.CompLen != h.HunkBytes || e.Offset+uint64(e.CompLen) > uint64(len(b)) {
			t.Errorf("entry %d = %+v", i, e)
		}
	}
	if data == 0 {
		t.Error("no data hunks found")
	}
	t.Logf("%d data hunks, %d zero hunks", data, zero)
}

func TestUncompressedMapZeroHunks(t *testing.T) {
	b, h := chdmanFileShape(t, "none", 64, testimg.Zeros)
	m, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range entriesOf(m) {
		if e != (MapEntry{Type: CompNone}) {
			t.Fatalf("entry %d = %+v, want a zero hunk", i, e)
		}
	}
}
