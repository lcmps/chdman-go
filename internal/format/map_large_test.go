package format

import (
	"bytes"
	"errors"
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
	if len(got) != hunks || got[hunks-1].Type != CompSelf || got[hunks-1].Offset != 0 {
		t.Fatalf("decoded %d entries, last %+v", len(got), got[len(got)-1])
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
