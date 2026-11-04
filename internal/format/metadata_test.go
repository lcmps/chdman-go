package format

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func readMeta(t *testing.T, b []byte) (Header, []MetadataEntry) {
	t.Helper()
	h, err := ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ReadMetadata(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return h, meta
}

func TestOverallSHA1MatchesChdman(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	dvd := filepath.Join(dir, "dvd.chd")
	oracle.CreateDVD(t, testimg.ISO(t, dir, 64, testimg.Mixed), dvd, "", 0)

	cueDir := t.TempDir()
	cue := testimg.Cue(t, cueDir, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 30, Pregap: 150, Shape: testimg.Mixed},
	}})
	cd := filepath.Join(dir, "cd.chd")
	oracle.CreateCD(t, cue, cd, "", 0)

	for name, path := range map[string]string{"dvd": dvd, "cd": cd} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			h, meta := readMeta(t, b)
			if len(meta) == 0 {
				t.Fatal("no metadata")
			}
			if got := OverallSHA1(h.RawSHA1, meta); got != h.SHA1 {
				t.Errorf("OverallSHA1 = %x, want %x", got, h.SHA1)
			}
		})
	}
}

func TestDVDMetadataShape(t *testing.T) {
	b, _ := chdmanFile(t, "", 64)
	_, meta := readMeta(t, b)
	if len(meta) != 1 {
		t.Fatalf("got %d metadata entries, want 1", len(meta))
	}
	m := meta[0]
	if m.Tag != TagDVD || !bytes.Equal(m.Data, []byte{0}) || m.Flags&FlagChecksum == 0 {
		t.Errorf("metadata = %+v", m)
	}
}

func TestMarshalMetadataRoundTrip(t *testing.T) {
	a := MetadataEntry{Tag: TagCHT2, Flags: FlagChecksum, Data: []byte("TRACK:1 TYPE:MODE1_RAW")}
	c := MetadataEntry{Tag: TagDVD, Flags: FlagChecksum, Data: []byte{0}}
	first := uint64(HeaderSize)
	ab := MarshalMetadata(a, first+uint64(16+len(a.Data)))
	cb := MarshalMetadata(c, 0)
	file := append(make([]byte, HeaderSize), append(ab, cb...)...)
	got, err := ReadMetadata(bytes.NewReader(file), Header{MetaOffset: first}, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Tag != a.Tag || !bytes.Equal(got[0].Data, a.Data) || got[1].Tag != c.Tag || got[1].Flags != FlagChecksum {
		t.Fatalf("got %+v", got)
	}
	if ab[4] != FlagChecksum || ab[7] != byte(len(a.Data)) {
		t.Errorf("header layout wrong: %x", ab[:16])
	}
}

func TestReadMetadataRejects(t *testing.T) {
	e := MetadataEntry{Tag: TagDVD, Flags: FlagChecksum, Data: []byte{0}}
	prefix := make([]byte, HeaderSize)

	loop := append(bytes.Clone(prefix), MarshalMetadata(e, HeaderSize)...) // points at itself
	if _, err := ReadMetadata(bytes.NewReader(loop), Header{MetaOffset: HeaderSize}, int64(len(loop))); !errors.Is(err, chderr.CorruptData) {
		t.Errorf("loop: err = %v, want CorruptData", err)
	}
	past := append(bytes.Clone(prefix), MarshalMetadata(e, 1<<40)...)
	if _, err := ReadMetadata(bytes.NewReader(past), Header{MetaOffset: HeaderSize}, int64(len(past))); !errors.Is(err, chderr.CorruptData) {
		t.Errorf("next past end: err = %v, want CorruptData", err)
	}
	trunc := append(bytes.Clone(prefix), MarshalMetadata(MetadataEntry{Tag: TagDVD, Data: make([]byte, 100)}, 0)[:50]...)
	if _, err := ReadMetadata(bytes.NewReader(trunc), Header{MetaOffset: HeaderSize}, int64(len(trunc))); !errors.Is(err, chderr.CorruptData) {
		t.Errorf("truncated data: err = %v, want CorruptData", err)
	}
}

func TestReadMetadataRejectsOverlappingEntries(t *testing.T) {
	// Every entry claims 16 MiB of data but starts only 16 bytes after the
	// previous one, so the claims overlap and sum to far more than the file.
	const entries = 60
	file := make([]byte, HeaderSize+1<<24+entries*16)
	for i := range entries {
		off := HeaderSize + 16*i
		binary.BigEndian.PutUint32(file[off:], TagDVD)
		binary.BigEndian.PutUint32(file[off+4:], 0xffffff)
		binary.BigEndian.PutUint64(file[off+8:], uint64(off+16))
	}
	binary.BigEndian.PutUint64(file[HeaderSize+16*(entries-1)+8:], 0)

	// Count the bytes read instead of measuring the heap, which the race
	// detector inflates. The summed claims exceed the file, so rejecting
	// them must not take more reads than the file holds.
	cr := &countingReaderAt{r: bytes.NewReader(file)}
	_, err := ReadMetadata(cr, Header{MetaOffset: HeaderSize}, int64(len(file)))
	if !errors.Is(err, chderr.CorruptData) {
		t.Fatalf("err = %v, want CorruptData", err)
	}
	if cr.n > int64(len(file)) {
		t.Errorf("read %d bytes of a %d byte file before rejecting", cr.n, len(file))
	}
}

type countingReaderAt struct {
	r io.ReaderAt
	n int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

func TestOverallSHA1NoMetadata(t *testing.T) {
	raw := [20]byte{1, 2, 3}
	want := sha1.Sum(raw[:])
	if got := OverallSHA1(raw, nil); got != want {
		t.Errorf("got %x, want %x", got, want)
	}
	// Entries without the checksum flag are ignored.
	if got := OverallSHA1(raw, []MetadataEntry{{Tag: TagCHT2, Data: []byte("x")}}); got != want {
		t.Errorf("unflagged entry changed the hash")
	}
}
