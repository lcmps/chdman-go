package chdman

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// makeDVD builds an ISO of the given shape and has chdman compress it.
func makeDVD(t testing.TB, shape testimg.Shape, sectors int, codecs string, hunk int) (chd string, iso []byte) {
	t.Helper()
	oracle.Require(t)
	dir := t.TempDir()
	isoPath := testimg.ISO(t, dir, sectors, shape)
	chd = filepath.Join(dir, "out.chd")
	oracle.CreateDVD(t, isoPath, chd, codecs, hunk)
	iso, err := os.ReadFile(isoPath)
	if err != nil {
		t.Fatal(err)
	}
	return chd, iso
}

func makeCD(t testing.TB, codecs string) string {
	t.Helper()
	oracle.Require(t)
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 60, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 40, Pregap: 150, Shape: testimg.Mixed},
	}})
	chd := filepath.Join(dir, "out.chd")
	oracle.CreateCD(t, cue, chd, codecs, 0)
	return chd
}

func openT(t testing.TB, path string) *Reader {
	t.Helper()
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func readAll(t testing.TB, r *Reader) []byte {
	t.Helper()
	buf := make([]byte, r.Info().LogicalBytes)
	if n, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %d bytes, %v", n, err)
	}
	return buf
}

func TestReadAtMatchesISO(t *testing.T) {
	for _, codecs := range []string{"zlib", "zstd", "lzma", "huff", "flac", "none", ""} {
		t.Run("codec="+codecs, func(t *testing.T) {
			chd, iso := makeDVD(t, testimg.Mixed, 300, codecs, 8192)
			r := openT(t, chd)
			if got := r.Info().LogicalBytes; got != uint64(len(iso)) {
				t.Fatalf("LogicalBytes = %d, want %d", got, len(iso))
			}
			if got := readAll(t, r); !bytes.Equal(got, iso) {
				t.Fatal("whole-file read differs from the ISO")
			}
			rng := rand.New(rand.NewPCG(7, 8))
			for range 200 {
				off := rng.IntN(len(iso))
				n := 1 + rng.IntN(20000) // often spans hunks
				buf := make([]byte, n)
				got, err := r.ReadAt(buf, int64(off))
				want := iso[off:min(off+n, len(iso))]
				if got != len(want) || !bytes.Equal(buf[:got], want) {
					t.Fatalf("ReadAt(%d, %d) returned %d bytes, differing from the ISO", n, off, got)
				}
				if (got < n) != (err == io.EOF) {
					t.Fatalf("ReadAt(%d, %d) = %d, %v", n, off, got, err)
				}
			}
			if n, err := r.ReadAt(make([]byte, 1), int64(len(iso))); n != 0 || err != io.EOF {
				t.Errorf("ReadAt at the end = %d, %v; want 0, EOF", n, err)
			}
			if _, err := r.ReadAt(make([]byte, 1), -1); err == nil {
				t.Error("ReadAt at a negative offset succeeded")
			}
		})
	}
}

func TestReadAtCD(t *testing.T) {
	for _, codecs := range []string{"cdzl", "cdzs", "cdlz", "cdfl"} {
		t.Run(codecs, func(t *testing.T) {
			r := openT(t, makeCD(t, codecs))
			sum := sha1.Sum(readAll(t, r))
			if got := hex.EncodeToString(sum[:]); got != r.Info().RawSHA1 {
				t.Fatalf("content SHA-1 %s, rawsha1 %s", got, r.Info().RawSHA1)
			}
			if r.Info().Disc != DiscCD {
				t.Errorf("Disc = %v, want DiscCD", r.Info().Disc)
			}
		})
	}
}

func TestSelfReferences(t *testing.T) {
	chd, iso := makeDVD(t, testimg.Zeros, 200, "zlib", 4096)
	r := openT(t, chd)
	self := 0
	for _, e := range r.entries {
		if e.Type == format.CompSelf {
			self++
		}
	}
	if self == 0 {
		t.Fatal("the map has no CompSelf entries")
	}
	if !bytes.Equal(readAll(t, r), iso) {
		t.Fatal("content differs from the ISO")
	}
}

// storedHunk returns the file offset of a hunk that holds incompressible data.
func storedHunk(t testing.TB, r *Reader) format.MapEntry {
	t.Helper()
	for _, e := range r.entries {
		if e.Type == format.CompNone && e.CompLen > 0 {
			return e
		}
	}
	t.Fatal("no stored hunk in the map")
	return format.MapEntry{}
}

func flipByte(t testing.TB, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatal(err)
	}
}

func TestHunkCRCChecked(t *testing.T) {
	chd, _ := makeDVD(t, testimg.Random, 40, "zlib", 4096)
	e := storedHunk(t, openT(t, chd))
	flipByte(t, chd, int64(e.Offset)+10)
	r := openT(t, chd)
	_, err := r.ReadAt(make([]byte, 4096*len(r.entries)), 0)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("ReadAt = %v, want ErrCorruptData", err)
	}
}

func TestConcurrentReads(t *testing.T) {
	chd, iso := makeDVD(t, testimg.Mixed, 400, "zstd", 8192)
	r := openT(t, chd)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1))
			for range 100 {
				off := rng.IntN(len(iso))
				buf := make([]byte, 1+rng.IntN(30000))
				n, _ := r.ReadAt(buf, int64(off))
				if want := iso[off:min(off+len(buf), len(iso))]; !bytes.Equal(buf[:n], want) {
					t.Errorf("goroutine %d: bad read at %d", g, off)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// selfRefFile builds a minimal compressed CHD of two 4096-byte hunks whose
// map is the given entries (hunk data follows the map when stored is set).
func selfRefFile(t testing.TB, entries []format.MapEntry, stored []byte) []byte {
	t.Helper()
	h := format.Header{
		Codecs:       [4]format.CodecTag{format.CodecZlib},
		LogicalBytes: 8192, HunkBytes: 4096, UnitBytes: 2048,
		MapOffset: format.HeaderSize,
	}
	m, err := format.EncodeCompressedMap(entries, format.HeaderSize+16+0) // fixed up below
	if err != nil {
		t.Fatal(err)
	}
	first := uint64(format.HeaderSize + len(m))
	for i := range entries {
		if entries[i].Type == format.CompNone {
			entries[i].Offset = first
		}
	}
	m, err = format.EncodeCompressedMap(entries, first)
	if err != nil {
		t.Fatal(err)
	}
	hdr := h.Marshal()
	return append(append(hdr[:], m...), stored...)
}

func TestSelfReferenceOrder(t *testing.T) {
	zeros := make([]byte, 4096)
	crc := format.CRC16(zeros)
	none := format.MapEntry{Type: format.CompNone, CompLen: 4096, CRC16: crc}
	for name, entries := range map[string][]format.MapEntry{
		"cycle":   {{Type: format.CompSelf, Offset: 1}, {Type: format.CompSelf, Offset: 0}},
		"itself":  {none, {Type: format.CompSelf, Offset: 1}},
		"forward": {{Type: format.CompSelf, Offset: 1}, none},
	} {
		t.Run(name, func(t *testing.T) {
			data := selfRefFile(t, entries, zeros)
			_, err := NewReader(bytes.NewReader(data), int64(len(data)))
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("NewReader = %v, want ErrCorruptData", err)
			}
		})
	}
	t.Run("backward", func(t *testing.T) {
		data := selfRefFile(t, []format.MapEntry{none, {Type: format.CompSelf, Offset: 0}}, zeros)
		r, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, r); !bytes.Equal(got, make([]byte, 8192)) {
			t.Fatal("content is not 8192 zero bytes")
		}
	})
}
