package format

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
)

// mapFile builds a compressed CHD holding only a header, the map for entries
// and stored bytes for the entries that need them. logical overrides the
// header's LogicalBytes when non-zero.
func mapFile(t *testing.T, entries []MapEntry, stored []byte, logical uint64) ([]byte, Header) {
	t.Helper()
	m, err := EncodeCompressedMap(entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := uint64(HeaderSize + len(m))
	for i := range entries { // stored offsets are relative to the first hunk
		if entries[i].Type != CompSelf {
			entries[i].Offset += first
		}
	}
	if m, err = EncodeCompressedMap(entries, first); err != nil {
		t.Fatal(err)
	}
	h := Header{
		Codecs:       [4]CodecTag{CodecZlib},
		LogicalBytes: uint64(len(entries)) * 2048, HunkBytes: 2048, UnitBytes: 2048,
		MapOffset: HeaderSize,
	}
	if logical != 0 {
		h.LogicalBytes = logical
	}
	hdr := h.Marshal()
	return append(append(hdr[:], m...), stored...), h
}

// A large disc of identical hunks legitimately has a tiny map, so the hunk
// count cannot be bounded tightly by the map size.
func TestReadMapLargeMostlyZero(t *testing.T) {
	const hunks = 2_500_000
	entries := make([]MapEntry, hunks)
	entries[0] = MapEntry{Type: CompType0, CompLen: 16, Offset: 0}
	for i := 1; i < hunks; i++ {
		entries[i] = MapEntry{Type: CompSelf} // all refer to hunk 0
	}
	b, h := mapFile(t, entries, make([]byte, 16), 0)
	if len(b) > 64<<10 {
		t.Fatalf("test file is %d bytes; the map should be tiny", len(b))
	}
	got, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != hunks || got.Entry(hunks-1).Type != CompSelf || got.Entry(hunks-1).Offset != 0 {
		t.Fatalf("decoded %d entries, last %+v", got.Len(), got.Entry(got.Len()-1))
	}
}

// A header may claim far more hunks than the map holds. The decoder must
// stop when the map's bits run out instead of sizing everything up front.
func TestReadMapClaimedCountDoesNotAllocateUpFront(t *testing.T) {
	const real = 200_000
	entries := make([]MapEntry, real)
	var stored []byte
	for i := range entries {
		if i%2 == 0 {
			entries[i] = MapEntry{Type: CompType0, CompLen: 1, CRC16: uint16(i)}
			stored = append(stored, 0)
		} else {
			entries[i] = MapEntry{Type: CompSelf}
		}
	}
	// Offsets of stored hunks are consecutive from the first one.
	off := uint64(0)
	for i := range entries {
		if entries[i].Type == CompType0 {
			entries[i].Offset = off
			off++
		}
	}
	b, h := mapFile(t, entries, stored, 20*real*2048) // claims 4M hunks
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
	runtime.ReadMemStats(&after)
	if !errors.Is(err, chderr.CorruptData) {
		t.Fatalf("err = %v, want CorruptData", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Errorf("allocated %d MiB for a %d byte file", grew>>20, len(b))
	}
}

// A few kilobytes of RLE map may describe millions of hunks. Decoding it must
// cost memory in proportion to the map, not to the hunk count it describes.
func TestReadMapRLEDoesNotAmplify(t *testing.T) {
	for name, step := range map[string]uint64{"self0": 0, "self1": 1} {
		t.Run(name, func(t *testing.T) {
			const hunks = 4_000_000
			entries := make([]MapEntry, hunks)
			entries[0] = MapEntry{Type: CompType0, CompLen: 16}
			for i := 1; i < hunks; i++ {
				entries[i] = MapEntry{Type: CompSelf, Offset: step * uint64(i-1)}
			}
			b, h := mapFile(t, entries, make([]byte, 16), 0)
			entries = nil
			if len(b) > 64<<10 {
				t.Fatalf("test file is %d bytes", len(b))
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			m, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
				t.Errorf("allocated %d MiB for a %d byte map", grew>>20, len(b))
			}
			if m.Len() != hunks {
				t.Fatalf("decoded %d hunks", m.Len())
			}
			if e := m.Entry(hunks - 1); e.Type != CompSelf || e.Offset != 0 {
				t.Errorf("last entry %+v, want a reference to hunk 0", e)
			}
		})
	}
}

// Self references must point at earlier hunks.
func TestReadMapRejectsForwardSelfReference(t *testing.T) {
	for name, entries := range map[string][]MapEntry{
		"itself":  {{Type: CompType0, CompLen: 16}, {Type: CompSelf, Offset: 1}},
		"forward": {{Type: CompType0, CompLen: 16}, {Type: CompSelf, Offset: 2}, {Type: CompSelf, Offset: 0}},
	} {
		t.Run(name, func(t *testing.T) {
			b, h := mapFile(t, entries, make([]byte, 16), 0)
			if _, err := ReadMap(bytes.NewReader(b), h, int64(len(b))); !errors.Is(err, chderr.CorruptData) {
				t.Fatalf("err = %v, want CorruptData", err)
			}
		})
	}
}

// A reference to a hunk that is itself a reference resolves to the hunk that
// holds the data, so reading never walks a chain.
func TestReadMapFlattensChains(t *testing.T) {
	const hunks = 50_000
	entries := make([]MapEntry, hunks)
	entries[0] = MapEntry{Type: CompType0, CompLen: 16}
	for i := 1; i < hunks; i++ {
		entries[i] = MapEntry{Type: CompSelf, Offset: uint64(i - 1)} // a chain through every hunk
	}
	// Sprinkle explicit references to other references.
	for i := 100; i < hunks; i += 997 {
		entries[i] = MapEntry{Type: CompSelf, Offset: uint64(i / 2)}
	}
	b, h := mapFile(t, entries, make([]byte, 16), 0)
	m, err := ReadMap(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(1); i < hunks; i++ {
		if e := m.Entry(i); e.Type != CompSelf || e.Offset != 0 {
			t.Fatalf("entry %d = %+v, want a reference to hunk 0", i, e)
		}
	}
}

// eofReader returns io.EOF together with every read's data, which io.ReaderAt
// permits.
type eofReader struct{ b []byte }

func (r eofReader) ReadAt(p []byte, off int64) (int, error) {
	return copy(p, r.b[min(off, int64(len(r.b))):]), io.EOF
}

func TestReadMapAcceptsEOFWithData(t *testing.T) {
	entries := []MapEntry{{Type: CompType0, CompLen: 16}, {Type: CompSelf}}
	b, h := mapFile(t, entries, make([]byte, 16), 0)
	if _, err := ReadMap(eofReader{b}, h, int64(len(b))); err != nil {
		t.Fatal(err)
	}
}
