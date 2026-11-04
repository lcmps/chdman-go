package chdwriter

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

const (
	testHunk = 4096
	isoUnit  = 2048
)

type memSource struct{ data []byte }

func (s memSource) ReadHunk(index uint32, dst []byte) error {
	n := copy(dst, s.data[min(int(index)*len(dst), len(s.data)):])
	clear(dst[n:])
	return nil
}

func dvdMeta() []format.MetadataEntry {
	return []format.MetadataEntry{{Tag: format.TagDVD, Flags: format.FlagChecksum, Data: []byte{0}}}
}

func shapeData(t *testing.T, sectors int, s testimg.Shape) []byte {
	t.Helper()
	b, err := os.ReadFile(testimg.ISO(t, t.TempDir(), sectors, s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// audioData is a slowly varying 16-bit stereo wave that FLAC beats the other
// codecs on.
func audioData(n int) []byte {
	b := make([]byte, n)
	for i := 0; i+3 < n; i += 4 {
		v := int16(8000 * math.Sin(float64(i/4)/40))
		binary.BigEndian.PutUint16(b[i:], uint16(v))
		binary.BigEndian.PutUint16(b[i+2:], uint16(v/2))
	}
	return b
}

func opts(codecs ...format.CodecTag) Options {
	var o Options
	copy(o.Codecs[:], codecs)
	o.HunkBytes, o.UnitBytes = testHunk, isoUnit
	o.Metadata = dvdMeta()
	return o
}

func writeCHD(t *testing.T, data []byte, o Options) (path string, raw, sum [20]byte) {
	t.Helper()
	o.LogicalBytes = uint64(len(data))
	path = filepath.Join(t.TempDir(), "out.chd")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, sum, err = Write(context.Background(), f, memSource{data}, o)
	if err != nil {
		t.Fatal(err)
	}
	return path, raw, sum
}

func roundTrip(t *testing.T, data []byte, o Options) string {
	t.Helper()
	oracle.Require(t)
	path, raw, sum := writeCHD(t, data, o)
	if want := sha1.Sum(data); raw != want {
		t.Errorf("raw SHA-1 %x, want %x", raw, want)
	}
	if want := format.OverallSHA1(raw, o.Metadata); sum != want {
		t.Errorf("SHA-1 %x, want %x", sum, want)
	}
	if err := oracle.Verify(t, path); err != nil {
		t.Fatal(err)
	}
	iso := filepath.Join(t.TempDir(), "out.iso")
	oracle.ExtractDVD(t, path, iso)
	got, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("chdman extractdvd returned different bytes")
	}
	return path
}

func readBack(t *testing.T, path string) (format.Header, []format.MapEntry, []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	m, err := format.ReadMap(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]format.MapEntry, m.Len())
	for i := range entries {
		entries[i] = m.Entry(uint32(i))
	}
	return h, entries, b
}

var shapes = map[string]testimg.Shape{
	"zeros": testimg.Zeros, "random": testimg.Random,
	"repeated": testimg.Repeated, "mixed": testimg.Mixed,
}

func TestWriteDefaultCodecsVerifiedByChdman(t *testing.T) {
	sets := map[string][]format.CodecTag{
		"lzma":    {format.CodecLZMA},
		"default": {format.CodecLZMA, format.CodecZlib, format.CodecHuffman, format.CodecFLAC},
	}
	for setName, tags := range sets {
		for name, shape := range shapes {
			t.Run(setName+"/"+name, func(t *testing.T) {
				// An odd sector count leaves a zero-padded final hunk.
				roundTrip(t, shapeData(t, 301, shape), opts(tags...))
			})
		}
	}
}

func TestWriteEachCodec(t *testing.T) {
	codecs := []format.CodecTag{format.CodecZlib, format.CodecZstd, format.CodecHuffman, format.CodecFLAC}
	for _, c := range codecs {
		t.Run(c.String()+"/mixed", func(t *testing.T) {
			roundTrip(t, shapeData(t, 130, testimg.Mixed), opts(c))
		})
	}
	t.Run("flac/audio", func(t *testing.T) {
		path := roundTrip(t, audioData(40*testHunk), opts(format.CodecFLAC))
		_, m, _ := readBack(t, path)
		for i, e := range m {
			if e.Type != 0 {
				t.Fatalf("hunk %d stored as type %d, want flac in slot 0", i, e.Type)
			}
		}
	})
}

func TestWriteSelfRefs(t *testing.T) {
	o := opts(format.CodecZlib)
	small, _, _ := writeCHD(t, shapeData(t, testimg.RepeatBlock, testimg.Repeated), o)
	path := roundTrip(t, shapeData(t, 4*testimg.RepeatBlock, testimg.Repeated), o)
	_, m, _ := readBack(t, path)
	selfs := 0
	for i, e := range m {
		if e.Type == format.CompSelf {
			selfs++
			if e.Offset >= uint64(i) {
				t.Errorf("hunk %d refers forward to %d", i, e.Offset)
			}
		}
	}
	if selfs == 0 {
		t.Fatal("no CompSelf entries")
	}
	a, _ := os.Stat(small)
	b, _ := os.Stat(path)
	if b.Size() >= 2*a.Size() {
		t.Errorf("4x repeated data is %d bytes, 1x is %d: no saving", b.Size(), a.Size())
	}
}

func TestWriteNone(t *testing.T) {
	oracle.Require(t)
	data := shapeData(t, 100, testimg.Mixed)
	// Make sure there are hunks of both kinds, then a short final hunk.
	clear(data[4*testHunk : 5*testHunk])
	data[len(data)-1] = 1
	o := opts()
	path, raw, sum := writeCHD(t, data, o)
	if raw != ([20]byte{}) || sum != ([20]byte{}) {
		t.Errorf("returned hashes %x %x, want zero", raw, sum)
	}
	h, m, file := readBack(t, path)
	if h.RawSHA1 != ([20]byte{}) || h.SHA1 != ([20]byte{}) {
		t.Error("header hashes are not zero")
	}
	hunks := h.HunkCount()
	if h.MapOffset != format.HeaderSize {
		t.Errorf("map offset %d, want %d", h.MapOffset, format.HeaderSize)
	}
	if want := uint64(format.HeaderSize + 4*hunks); h.MetaOffset != want {
		t.Errorf("metadata offset %d, want %d", h.MetaOffset, want)
	}
	zeroHunks := 0
	for i := range hunks {
		entry := binary.BigEndian.Uint32(file[format.HeaderSize+4*i:])
		hunk := data[int(i)*testHunk : min(int(i+1)*testHunk, len(data))]
		if m[i].Type != format.CompNone {
			t.Fatalf("hunk %d type %d", i, m[i].Type)
		}
		if bytes.Count(hunk, []byte{0}) == len(hunk) {
			zeroHunks++
			if entry != 0 {
				t.Errorf("zero hunk %d has entry %d", i, entry)
			}
			continue
		}
		if entry == 0 {
			t.Fatalf("hunk %d has no entry", i)
		}
		stored := file[int(entry)*testHunk:]
		if !bytes.Equal(stored[:len(hunk)], hunk) {
			t.Errorf("hunk %d data differs at entry %d", i, entry)
		}
	}
	if zeroHunks == 0 {
		t.Fatal("test data has no zero hunk")
	}
	iso := filepath.Join(t.TempDir(), "out.iso")
	oracle.ExtractDVD(t, path, iso)
	got, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("chdman extractdvd returned different bytes")
	}
}

func TestWorkerCountSameHashes(t *testing.T) {
	data := shapeData(t, 400, testimg.Mixed)
	for _, tags := range [][]format.CodecTag{
		{format.CodecLZMA, format.CodecZlib, format.CodecHuffman, format.CodecFLAC}, nil,
	} {
		var files [][]byte
		var raws [][20]byte
		var sums [][20]byte
		for _, w := range []int{1, 8} {
			o := opts(tags...)
			o.Workers = w
			path, raw, sum := writeCHD(t, data, o)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files, raws, sums = append(files, b), append(raws, raw), append(sums, sum)
		}
		if raws[0] != raws[1] || sums[0] != sums[1] {
			t.Errorf("%v: hashes differ between worker counts", tags)
		}
		if !bytes.Equal(files[0], files[1]) {
			t.Errorf("%v: files differ between worker counts", tags)
		}
	}
}

type failingSource struct{ memSource }

var errRead = errors.New("read failed")

func (s failingSource) ReadHunk(i uint32, dst []byte) error {
	if i == 5 {
		return errRead
	}
	return s.memSource.ReadHunk(i, dst)
}

func TestWriteSourceErrorAndCancel(t *testing.T) {
	data := shapeData(t, 200, testimg.Random)
	o := opts(format.CodecZlib)
	o.LogicalBytes = uint64(len(data))
	f, err := os.Create(filepath.Join(t.TempDir(), "x.chd"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, _, err := Write(context.Background(), f, failingSource{memSource{data}}, o); !errors.Is(err, errRead) {
		t.Errorf("source error: got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Write(ctx, f, memSource{data}, o); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: got %v", err)
	}
}

func TestWriteRejectsBadOptions(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "x.chd"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bad := map[string]func(*Options){
		"zero hunk":     func(o *Options) { o.HunkBytes = 0 },
		"huge hunk":     func(o *Options) { o.HunkBytes = format.MaxHunkBytes + 4096 },
		"unit mismatch": func(o *Options) { o.UnitBytes = 3000 },
		"gap in codecs": func(o *Options) { o.Codecs = [4]format.CodecTag{0, format.CodecZlib} },
	}
	for name, mutate := range bad {
		o := opts(format.CodecZlib)
		o.LogicalBytes = testHunk
		mutate(&o)
		if _, _, err := Write(context.Background(), f, memSource{make([]byte, testHunk)}, o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
