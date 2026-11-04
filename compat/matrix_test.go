// Package compat holds the chdman compatibility matrix: every cell builds an
// input, has chdman 0.289 and chdman-go compress it, and checks the seven
// success criteria of the design against each other.
//
// Per cell (CD or DVD):
//
//  1. rawsha1 and sha1 of chdman-go equal chdman's, compared separately, and
//     equal the hashes stored in the file;
//  2. chdman verifies chdman-go's file, and chdman's extraction of it equals
//     chdman's extraction of chdman's own file (and, where the cell loses
//     nothing, the original input);
//  3. chdman-go verifies chdman's file (not verifiable for "none"), extracts it
//     like chdman does, and random reads match the original bytes;
//  4. flipping a byte in the data, the map or the metadata of a file that
//     stores hashes makes open or verify fail, for both tools' files;
//  5. create, extract, create keeps both hashes, or for the shapes where
//     chdman does not keep them either, equals chdman's own round trip;
//  6. inputs that make chdman lose data produce the matching warning;
//  7. everything runs with CGO_ENABLED=0 (release-check).
//
// Reductions, since the full cross product would take hours:
//
//   - every CD layout is crossed with the default codecs only;
//   - every codec is crossed with two representative layouts (a data+audio
//     disc and a MODE2 disc); "none" with six layouts that cover every sector
//     size and the pregap shapes;
//   - the data shapes (zeros, random, repeated, valid ECC, invalid ECC) are
//     crossed with four layouts, one per sector mode family, at the default
//     codecs; the shape of a whole track is one value, so mixed sector shapes
//     are covered by the layout cells, which use testimg.Mixed;
//   - hunk sizes are crossed with one larger disc (CD) or one 2 MiB image
//     (DVD) and the default codecs;
//   - DVD codecs run on one mixed image, DVD shapes on the default codecs;
//   - PS2-sized and PSP-sized images are sparse files of 4.7 GB and 1.8 GB, run
//     only without -short and with a hash-parity, verify and sampled-read
//     subset (no full extraction), because 4.7 GB written twice per cell would
//     dominate the run; the scaled-down images in the default set cover the
//     extraction paths for the same code;
//   - chdman's own files of one to three hunks are only read through their
//     header (known chdman 0.289 map defect), so every cell that reads
//     chdman's file has at least four hunks, enforced by the runner.
//
// CHDMAN_GO_REAL_IMAGES=<dir> adds every .cue and .iso there as a cell. The
// directory must exist and hold at least one of them, or the run fails. Real
// cells run one at a time and check hash parity, the read paths and
// extraction: warnings are reported, not compared with a fixed set, and a
// file of fewer than four hunks is read through its header only. Above
// bigRealBytes of content the extra corruption checks and the second create
// of the round trip are skipped, and every intermediate file is deleted as
// soon as it has been compared. Run them with a generous -timeout and a
// TMPDIR with room for several copies of the largest image; see the README.
package compat

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lcmps/chdman-go"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// minReadableHunks is the least number of hunks for chdman's own file to be
// read back reliably (see the top comment).
const minReadableHunks = 4

const zeroHash = "0000000000000000000000000000000000000000"

// bigRealBytes is the content size above which a real-image cell skips the
// work that only repeats a check: three more full verifies per file for the
// corruption checks, and the second create of the round trip.
const bigRealBytes = 1 << 30

// ---- shared helpers --------------------------------------------------------

func readHeader(t testing.TB, path string) format.Header {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, format.HeaderSize)
	if _, err := io.ReadFull(f, b); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	h, err := format.ParseHeader(b)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return h
}

func hashes(h format.Header) (raw, sum string) {
	return hex.EncodeToString(h.RawSHA1[:]), hex.EncodeToString(h.SHA1[:])
}

func splitCodecs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func rngFor(name string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(name))
	return rand.New(rand.NewPCG(h.Sum64(), 17))
}

func openReader(t testing.TB, path string) *chdman.Reader {
	t.Helper()
	r, err := chdman.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// assertVerify checks that chdman-go verifies the file: OK, or not verifiable
// for an uncompressed file.
func assertVerify(t testing.TB, r *chdman.Reader, none bool) {
	t.Helper()
	res, err := r.Verify(context.Background(), chdman.VerifyOptions{Workers: 2})
	want := chdman.VerifyOK
	if none {
		want = chdman.VerifyNotVerifiable
	}
	if err != nil || res.Status != want {
		t.Fatalf("Verify = %+v, %v; want %v", res, err, want)
	}
}

func logicalSHA1(t testing.TB, r *chdman.Reader) string {
	t.Helper()
	h := sha1.New()
	if _, err := io.Copy(h, io.NewSectionReader(r, 0, int64(r.Info().LogicalBytes))); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// assertSameReads compares the logical content of two readers, whole when it
// is small and at random offsets otherwise.
func assertSameReads(t testing.TB, name string, a, b *chdman.Reader) {
	t.Helper()
	size := int64(a.Info().LogicalBytes)
	if size != int64(b.Info().LogicalBytes) {
		t.Fatalf("logical bytes %d and %d", size, b.Info().LogicalBytes)
	}
	if size <= 64<<20 {
		if x, y := logicalSHA1(t, a), logicalSHA1(t, b); x != y {
			t.Errorf("logical content differs: %s and %s", x, y)
		}
	}
	rng := rngFor(name)
	for range 24 {
		n := 1 + rng.Int64N(min(size, 3*int64(a.Info().HunkBytes)))
		off := rng.Int64N(size - n + 1)
		x, y := make([]byte, n), make([]byte, n)
		if _, err := a.ReadAt(x, off); err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", n, off, err)
		}
		if _, err := b.ReadAt(y, off); err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", n, off, err)
		}
		if !bytes.Equal(x, y) {
			t.Fatalf("reads of %d bytes at %d differ", n, off)
		}
	}
}

// flipAt reads from src with one byte inverted.
type flipAt struct {
	src io.ReaderAt
	off int64
}

func (f flipAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.src.ReadAt(p, off)
	if i := f.off - off; i >= 0 && i < int64(n) {
		p[i] ^= 0xff
	}
	return n, err
}

// metadataRegion returns where the metadata chain ends and the data offset of
// the first non-empty checksummed entry.
func metadataRegion(t testing.TB, f *os.File, h format.Header) (end, checksummed int64) {
	t.Helper()
	checksummed = -1
	end = int64(format.HeaderSize)
	var hdr [16]byte
	for off := h.MetaOffset; off != 0; {
		if _, err := f.ReadAt(hdr[:], int64(off)); err != nil {
			t.Fatal(err)
		}
		lf := binary.BigEndian.Uint32(hdr[4:])
		n := int64(lf & 0xffffff)
		if uint8(lf>>24)&format.FlagChecksum != 0 && n > 0 && checksummed < 0 {
			checksummed = int64(off) + 16
		}
		end = max(end, int64(off)+16+n)
		off = binary.BigEndian.Uint64(hdr[8:])
	}
	return end, checksummed
}

// assertCorruptionCaught flips one byte in the data, the map and the metadata
// of a file that stores hashes. Each must make open or verify fail.
func assertCorruptionCaught(t testing.TB, path string) {
	t.Helper()
	h := readHeader(t, path)
	if h.RawSHA1 == [20]byte{} {
		t.Fatalf("%s stores no hashes; corruption does not apply", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	metaEnd, metaData := metadataRegion(t, f, h)
	if metaData < 0 {
		t.Fatal("no checksummed metadata")
	}
	// The layout of both tools' files: header, metadata, hunks, map.
	if int64(h.MapOffset) <= metaEnd {
		t.Fatalf("map at %d, metadata ends at %d", h.MapOffset, metaEnd)
	}
	cases := map[string]int64{
		"data":     metaEnd + (int64(h.MapOffset)-metaEnd)/2,
		"map":      int64(h.MapOffset) + (st.Size()-int64(h.MapOffset))/2,
		"metadata": metaData,
	}
	for _, name := range slices.Sorted(mapsKeys(cases)) {
		r, err := chdman.NewReader(flipAt{f, cases[name]}, st.Size())
		if err != nil {
			// Refused at open: the error must say the file is damaged.
			if !errors.Is(err, chdman.ErrCorruptData) {
				t.Errorf("%s: open with a flipped %s byte failed with %v, want ErrCorruptData", filepath.Base(path), name, err)
			}
			continue
		}
		res, err := r.Verify(context.Background(), chdman.VerifyOptions{Workers: 2})
		switch {
		case err == nil && res.Status == chdman.VerifyOK:
			t.Errorf("%s: a flipped %s byte at %d still verifies", filepath.Base(path), name, cases[name])
		case err != nil && !errors.Is(err, chdman.ErrCorruptData) && !errors.Is(err, chdman.ErrHashMismatch):
			// A damaged hunk is corrupt data and damaged content a hash
			// mismatch; nothing else may explain the failure.
			t.Errorf("%s: verify with a flipped %s byte failed with %v, want ErrCorruptData or ErrHashMismatch", filepath.Base(path), name, err)
		}
	}
}

func mapsKeys[M ~map[string]V, V any](m M) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// assertSelfReferences fails unless the compressed file's map has hunks that
// refer to an earlier identical hunk, which is what the Repeated shape is
// there to exercise.
func assertSelfReferences(t testing.TB, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	m, err := format.ReadMap(f, readHeader(t, path), st.Size())
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	var self uint32
	for i := range m.Len() {
		if m.Entry(i).Type == format.CompSelf {
			self++
		}
	}
	if self == 0 {
		t.Errorf("%s: none of %d hunks is a self-reference; the Repeated cell does not exercise them", filepath.Base(path), m.Len())
	}
}

// realImages returns the .cue and .iso files in CHDMAN_GO_REAL_IMAGES, or nil
// when it is not set. A directory that does not exist, or that holds neither,
// fails the test: running nothing would look like passing.
func realImages(t *testing.T) (cues, isos []string) {
	t.Helper()
	dir := os.Getenv("CHDMAN_GO_REAL_IMAGES")
	if dir == "" {
		t.Log("CHDMAN_GO_REAL_IMAGES is not set; no extra cells") // not a skip: release runs allow none
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("CHDMAN_GO_REAL_IMAGES: %v", err)
	}
	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".cue":
			cues = append(cues, path)
		case ".iso":
			isos = append(isos, path)
		}
	}
	if len(cues)+len(isos) == 0 {
		t.Fatalf("CHDMAN_GO_REAL_IMAGES=%s holds no .cue or .iso files", dir)
	}
	return cues, isos
}

func kinds(ws []chdman.Warning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, string(w.Kind))
	}
	slices.Sort(out)
	return out
}

func wantKinds(ks ...chdman.WarningKind) []string {
	var out []string
	for _, k := range ks {
		out = append(out, string(k))
	}
	slices.Sort(out)
	return out
}

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- CD cells --------------------------------------------------------------

type cdCell struct {
	name string
	// build writes the input into dir and returns the cue (or toc) path.
	build func(t testing.TB, dir string) string
	// disc, if set, describes the generated input and enables the checks
	// against its bins.
	disc *testimg.Disc
	// codecs is the -c argument; "" means the default.
	codecs string
	hunk   int
	// lossy: the input loses data or has a shape whose hashes chdman itself
	// does not keep through extract and create, so the round trip is
	// compared with chdman's own and the bins with chdman's extraction.
	lossy bool
	// warn is the exact set of warning kinds CreateCD reports.
	warn []chdman.WarningKind
	// headerOnly: chdman's file is too small to read; only its header is used.
	headerOnly bool
	// chdmanOnly: chdman-go cannot read the input format (a .toc), so only
	// chdman's file is used.
	chdmanOnly bool
	// extractWarn is the exact set of warning kinds ExtractCD reports.
	extractWarn []chdman.WarningKind
	// selfRefs: the data repeats, so both files must contain hunks that refer
	// to an earlier identical hunk.
	selfRefs bool
	// real: one of the owner's own images. Warnings are not compared with a
	// fixed set, the cell runs alone and cleans up as it goes.
	real bool
}

func layoutCell(l testimg.Layout) cdCell {
	return cdCell{name: l.Name, disc: &l.Disc, build: func(t testing.TB, dir string) string { return testimg.Cue(t, dir, l.Disc) }}
}

func layoutByName(t testing.TB, name string) testimg.Layout {
	t.Helper()
	for _, l := range allLayouts() {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no layout %q", name)
	return testimg.Layout{}
}

// allLayouts is the shared layouts plus a disc of eleven tracks, which makes
// chdman number split bins with two digits.
func allLayouts() []testimg.Layout {
	var tracks []testimg.Track
	for range 11 {
		tracks = append(tracks, testimg.Track{Mode: "AUDIO", Frames: 12, Shape: testimg.Mixed})
	}
	return append(testimg.Layouts(),
		testimg.Layout{Name: "eleven-split", Disc: testimg.Disc{Tracks: tracks, Split: true}},
		testimg.Layout{Name: "eleven-single", Disc: testimg.Disc{Tracks: tracks}})
}

func withShape(d testimg.Disc, frames func(i int) int, s testimg.Shape) testimg.Disc {
	out := d
	out.Tracks = slices.Clone(d.Tracks)
	for i := range out.Tracks {
		out.Tracks[i].Shape = s
		out.Tracks[i].Frames = frames(i)
	}
	return out
}

// corruptECC flips a byte of the error correction code of every third sector
// of the first track's bin: valid sync and header, invalid ECC.
func corruptECC(t testing.TB, dir string, d testimg.Disc) {
	t.Helper()
	name := "disc.bin"
	if d.Split {
		name = "track01.bin"
	}
	path := filepath.Join(dir, name)
	b := readFile(t, path)
	for i := 0; (i+1)*2352 <= len(b); i += 3 {
		b[i*2352+2100] ^= 0x5a
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cdCells(t testing.TB) (groups []string, cells map[string][]cdCell) {
	cells = map[string][]cdCell{}
	add := func(group string, c ...cdCell) {
		if _, ok := cells[group]; !ok {
			groups = append(groups, group)
		}
		cells[group] = append(cells[group], c...)
	}

	// Every layout at the default codecs and hunk size.
	for _, l := range allLayouts() {
		add("layouts", layoutCell(l))
	}

	// Data shapes on one layout per sector mode family. 200 data frames make
	// the 64-sector Repeated block recur, which forces self-references.
	frames := func(i int) int { return []int{200, 80}[min(i, 1)] }
	for _, ln := range []string{"mode1-2352", "mode2-2352", "mode1-2048", "data-audio-split"} {
		for _, s := range []struct {
			name  string
			shape testimg.Shape
			ecc   bool
		}{
			{"zeros", testimg.Zeros, false}, {"random", testimg.Random, false},
			{"repeated", testimg.Repeated, false}, {"invalid-ecc", testimg.Repeated, true},
		} {
			d := withShape(layoutByName(t, ln).Disc, frames, s.shape)
			c := cdCell{name: ln + "/" + s.name, disc: &d, selfRefs: s.shape == testimg.Repeated, build: func(t testing.TB, dir string) string {
				cue := testimg.Cue(t, dir, d)
				if s.ecc && d.Tracks[0].Mode != "MODE1/2048" {
					corruptECC(t, dir, d)
				}
				return cue
			}}
			if s.ecc && d.Tracks[0].Mode == "MODE1/2048" {
				continue // cooked sectors carry no ECC
			}
			add("shapes", c)
		}
	}

	// Every codec on two layouts: the CD codecs, then the generic ones, which
	// chdman also accepts for CDs, and the default set spelled out.
	for _, codecs := range []string{"cdzl", "cdzs", "cdlz", "cdfl", "cdlz,cdzl,cdfl", "cdzl,cdlz", "lzma", "zlib", "zstd", "huff", "flac"} {
		for _, ln := range []string{"data-audio-single", "mode2-2352"} {
			c := layoutCell(layoutByName(t, ln))
			c.name, c.codecs = codecs+"/"+ln, codecs
			add("codecs", c)
		}
	}
	for _, ln := range []string{"mode1-2048", "mode2-2336", "mode2-2352", "data-audio-split", "pregap", "index00-single"} {
		c := layoutCell(layoutByName(t, ln))
		c.name, c.codecs = "none/"+ln, "none"
		add("codecs", c)
	}

	// Hunk sizes, in frames of 2448 bytes, on a disc that has 480 frames.
	big := testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 200, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 150, IndexZero: 10, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 130, Shape: testimg.Mixed},
	}}
	for _, hs := range []int{2448, 9792, 19584, 39168, 97920} {
		add("hunks", cdCell{name: fmt.Sprintf("%d", hs), hunk: hs, disc: &big,
			build: func(t testing.TB, dir string) string { return testimg.Cue(t, dir, big) }})
	}
	add("hunks", cdCell{name: "9792/cdzl", hunk: 9792, codecs: "cdzl", disc: &big,
		build: func(t testing.TB, dir string) string { return testimg.Cue(t, dir, big) }})

	add("warnings", warningCells()...)
	add("shapes-beyond-testimg", extraShapeCells()...)
	return groups, cells
}

// warningCells are the inputs where chdman loses data: chdman-go must give
// chdman's hashes and say so.
func warningCells() []cdCell {
	mode1 := testimg.Track{Mode: "MODE1/2352", Frames: 60, Shape: testimg.Mixed}
	return []cdCell{
		{name: "trailing-bytes", lossy: true, warn: []chdman.WarningKind{chdman.WarnTrailingBytesDropped},
			build: func(t testing.TB, dir string) string {
				bin := testimg.Bin(t, dir, "disc.bin", mode1)
				f, err := os.OpenFile(bin, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				f.Write(bytes.Repeat([]byte{0xAA}, 100))
				f.Close()
				return testimg.WriteCue(t, dir, "d.cue", "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
			}},
		{name: "lead-out-overwrites-frames", lossy: true, warn: []chdman.WarningKind{chdman.WarnFramesOverwritten},
			build: func(t testing.TB, dir string) string {
				a := testimg.Bin(t, dir, "a.tmp", mode1)
				b := testimg.Bin(t, dir, "b.tmp", testimg.Track{Mode: "AUDIO", Frames: 40, Shape: testimg.Mixed})
				var all []byte
				for _, p := range []string{a, b} {
					all = append(all, readFile(t, p)...)
					os.Remove(p)
				}
				if err := os.WriteFile(filepath.Join(dir, "disc.bin"), all, 0o644); err != nil {
					t.Fatal(err)
				}
				return testimg.WriteCue(t, dir, "s.cue", `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT 00:00:20
REM SESSION 02
  TRACK 02 AUDIO
    INDEX 01 00:00:30
`)
			}},
		{name: "unused-file", lossy: true, warn: []chdman.WarningKind{chdman.WarnFileUnused},
			build: func(t testing.TB, dir string) string {
				testimg.Bin(t, dir, "a.bin", mode1)
				testimg.Bin(t, dir, "b.bin", testimg.Track{Mode: "AUDIO", Frames: 50, Shape: testimg.Mixed})
				return testimg.WriteCue(t, dir, "d.cue", "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\nFILE \"b.bin\" BINARY\n")
			}},
		{name: "last-track-trimmed", lossy: true, warn: []chdman.WarningKind{chdman.WarnFramesDropped},
			build: func(t testing.TB, dir string) string {
				testimg.Bin(t, dir, "disc.img", testimg.Track{Mode: "MODE1/2352", Frames: 2000, Shape: testimg.Mixed})
				return testimg.WriteCue(t, dir, "s.cue", `FILE "disc.img" BINARY
REM SESSION 01
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT 00:05:00
REM SESSION 02
  TRACK 02 MODE1/2352
    INDEX 00 00:06:00
    INDEX 01 00:06:00
  REM LEAD-OUT 00:10:00
`)
			}},
	}
}

// extraShapeCells are inputs that testimg.Cue cannot express: the shapes whose
// create-extract-create hashes chdman itself does not keep.
func extraShapeCells() []cdCell {
	return []cdCell{
		{name: "index00-equals-index01", lossy: true,
			build: func(t testing.TB, dir string) string {
				testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed})
				b := readFile(t, filepath.Join(dir, "disc.bin"))
				b = append(b, readFile(t, testimg.Bin(t, dir, "a.tmp", testimg.Track{Mode: "AUDIO", Frames: 40, Shape: testimg.Mixed}))...)
				os.Remove(filepath.Join(dir, "a.tmp"))
				if err := os.WriteFile(filepath.Join(dir, "disc.bin"), b, 0o644); err != nil {
					t.Fatal(err)
				}
				return testimg.WriteCue(t, dir, "d.cue", `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    INDEX 00 00:00:40
    INDEX 01 00:00:40
`)
			}},
		{name: "multisession-index00-equals-index01", lossy: true,
			build: func(t testing.TB, dir string) string {
				var all []byte
				for i, tr := range []testimg.Track{
					{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed}, {Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed}} {
					name := fmt.Sprintf("t%d.tmp", i)
					all = append(all, readFile(t, testimg.Bin(t, dir, name, tr))...)
					os.Remove(filepath.Join(dir, name))
				}
				if err := os.WriteFile(filepath.Join(dir, "disc.bin"), all, 0o644); err != nil {
					t.Fatal(err)
				}
				return testimg.WriteCue(t, dir, "d.cue", `FILE "disc.bin" BINARY
REM SESSION 01
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
REM SESSION 02
  TRACK 02 MODE1/2352
    INDEX 00 00:00:40
    INDEX 01 00:00:40
`)
			}},
		{name: "subcode-from-toc", lossy: true, chdmanOnly: true,
			extractWarn: []chdman.WarningKind{chdman.WarnSubcodeOmitted},
			build: func(t testing.TB, dir string) string {
				var bin []byte
				for i := range 60 {
					bin = append(bin, bytes.Repeat([]byte{byte(i + 1)}, 2352)...)
					bin = append(bin, bytes.Repeat([]byte{byte(0xA0 + i)}, 96)...)
				}
				if err := os.WriteFile(filepath.Join(dir, "sub.bin"), bin, 0o644); err != nil {
					t.Fatal(err)
				}
				return testimg.WriteCue(t, dir, "disc.toc", "CD_ROM\n\n\n// Track 1\nTRACK MODE1_RAW RW_RAW\nNO COPY\n"+
					"DATAFILE \"sub.bin\" 00:00:60 // length in bytes: 146880\n\n\n")
			}},
		{name: "mode2-form-mix-from-toc", lossy: true, chdmanOnly: true,
			build: func(t testing.TB, dir string) string {
				var bin []byte
				for i := range 60 {
					bin = append(bin, bytes.Repeat([]byte{byte(i + 1)}, 2336)...)
				}
				if err := os.WriteFile(filepath.Join(dir, "m2.bin"), bin, 0o644); err != nil {
					t.Fatal(err)
				}
				return testimg.WriteCue(t, dir, "disc.toc", "CD_ROM_XA\n\n\n// Track 1\nTRACK MODE2_FORM_MIX\nNO COPY\n"+
					"DATAFILE \"m2.bin\" 00:00:60 // length in bytes: 140160\n\n\n")
			}},
	}
}

func TestCD(t *testing.T) {
	oracle.Require(t)
	groups, cells := cdCells(t)
	for _, g := range groups {
		t.Run(g, func(t *testing.T) {
			for _, c := range cells[g] {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					runCD(t, c)
				})
			}
		})
	}
}

// TestCDRealImages runs the cells over the owner's own cues, one at a time.
func TestCDRealImages(t *testing.T) {
	cues, _ := realImages(t)
	if len(cues) == 0 {
		return
	}
	oracle.Require(t)
	for _, cue := range cues {
		t.Run(filepath.Base(cue), func(t *testing.T) {
			runCD(t, cdCell{name: filepath.Base(cue), lossy: true, real: true, build: func(testing.TB, string) string { return cue }})
		})
	}
}

func runCD(t *testing.T, c cdCell) {
	ctx := context.Background()
	none := c.codecs == "none"
	work := t.TempDir()
	srcDir := t.TempDir()
	cue := c.build(t, srcDir)

	want := filepath.Join(work, "want.chd")
	oracle.CreateCD(t, cue, want, c.codecs, c.hunk)
	wantHdr := readHeader(t, want)
	wantRaw, wantSum := hashes(wantHdr)
	readable := wantHdr.HunkCount() >= minReadableHunks
	if !readable && !c.headerOnly && !c.real {
		t.Fatalf("chdman's file has %d hunks; at least %d are needed to read it", wantHdr.HunkCount(), minReadableHunks)
	}
	// A big real image skips the checks that only repeat earlier work.
	big := c.real && wantHdr.LogicalBytes > bigRealBytes
	if big {
		t.Logf("%d bytes: skipping the corruption checks and the round trip's second create", wantHdr.LogicalBytes)
	}
	if none != (wantRaw == zeroHash) {
		t.Fatalf("chdman rawsha1 %s for codecs %q", wantRaw, c.codecs)
	}

	var ours string
	var res chdman.CreateResult
	if !c.chdmanOnly {
		ours = filepath.Join(work, "ours.chd")
		var err error
		res, err = chdman.CreateCD(ctx, cue, ours, chdman.CreateOptions{Codecs: splitCodecs(c.codecs), HunkBytes: uint32(c.hunk), Workers: 2})
		if err != nil {
			t.Fatal(err)
		}
		// 1. Hash parity, each hash on its own.
		if res.RawSHA1 != wantRaw {
			t.Errorf("rawsha1 %s, chdman %s", res.RawSHA1, wantRaw)
		}
		if res.SHA1 != wantSum {
			t.Errorf("sha1 %s, chdman %s", res.SHA1, wantSum)
		}
		if r, s := hashes(readHeader(t, ours)); r != res.RawSHA1 || s != res.SHA1 {
			t.Errorf("stored hashes %s/%s, result %s/%s", r, s, res.RawSHA1, res.SHA1)
		}
		// 6. Warnings. A real image has whatever its cue and bins give.
		if c.real {
			t.Logf("warnings: %+v", res.Warnings)
		} else if got, want := kinds(res.Warnings), wantKinds(c.warn...); !slices.Equal(got, want) {
			t.Errorf("warnings %v, want %v (%+v)", got, want, res.Warnings)
		}
		// 2. chdman reads ours.
		if err := oracle.Verify(t, ours); err != nil {
			t.Errorf("chdman rejects our file: %v", err)
		}
		assertVerify(t, openReader(t, ours), none)
		if !none && !big {
			assertCorruptionCaught(t, ours)
		}
		if c.selfRefs {
			assertSelfReferences(t, ours)
		}
	}

	oursPath := ours
	if c.chdmanOnly {
		oursPath = want
	}
	// Extraction of our file by both tools, single and split bins.
	for _, split := range []bool{false, true} {
		theirs := extractChdman(t, oursPath, split)
		if !c.chdmanOnly {
			assertSameDirAndDrop(t, c.extractOurs(t, oursPath, split), theirs, "chdman-go extraction of our file")
		}
		if readable {
			assertSameDirAndDrop(t, extractChdman(t, want, split), theirs, "chdman extraction of chdman's file and ours")
			assertSameDirAndDrop(t, c.extractOurs(t, want, split), theirs, "chdman-go extraction of chdman's file")
		}
		if !split && !c.lossy && c.disc != nil {
			// Nothing is lost: the bins are the input, byte for byte.
			if got, want := concatBins(t, theirs), concatBins(t, srcDir); !bytes.Equal(got, want) {
				t.Errorf("extracted bins are %d bytes and differ from the input's %d", len(got), len(want))
			}
		}
		os.RemoveAll(theirs)
	}

	// 3. chdman-go reads chdman's file.
	if readable {
		r := openReader(t, want)
		assertVerify(t, r, none)
		if !none && !big {
			assertCorruptionCaught(t, want)
		}
		if c.selfRefs {
			assertSelfReferences(t, want)
		}
		if !c.chdmanOnly {
			ro := openReader(t, ours)
			assertSameReads(t, c.name, ro, r)
			assertSameTracks(t, ro, r)
		}
		if c.disc != nil {
			assertSectors(t, r, *c.disc, srcDir)
		}
	}

	// 5. Round trip. The cue chdman's extraction wrote is the one that
	// chdman's own round trip starts from.
	rtSource := oursPath
	if readable {
		rtSource = want
	}
	if big {
		return
	}
	rtDir := c.extractOurs(t, rtSource, c.disc != nil && c.disc.Split)
	defer os.RemoveAll(rtDir)
	cuePath := filepath.Join(rtDir, "out.cue")
	again := filepath.Join(work, "again.chd")
	rt, err := chdman.CreateCD(ctx, cuePath, again, chdman.CreateOptions{Codecs: splitCodecs(c.codecs), HunkBytes: uint32(c.hunk), Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	wantRT, wantRTSum := wantRaw, wantSum
	if c.lossy {
		// chdman's own create-extract-create, which does not keep the hashes
		// of these shapes either.
		theirDir := extractChdman(t, rtSource, false)
		defer os.RemoveAll(theirDir)
		chdRT := filepath.Join(work, "chdman-again.chd")
		oracle.CreateCD(t, filepath.Join(theirDir, "out.cue"), chdRT, c.codecs, c.hunk)
		wantRT, wantRTSum = hashes(readHeader(t, chdRT))
	}
	if rt.RawSHA1 != wantRT {
		t.Errorf("round trip rawsha1 %s, want %s", rt.RawSHA1, wantRT)
	}
	if rt.SHA1 != wantRTSum {
		t.Errorf("round trip sha1 %s, want %s", rt.SHA1, wantRTSum)
	}
}

// extractOurs extracts with chdman-go into a new directory. A real image has
// whatever warnings its cue gives; any other cell has exactly extractWarn.
func (c cdCell) extractOurs(t testing.TB, chd string, split bool) string {
	t.Helper()
	dir := t.TempDir()
	res, err := chdman.ExtractCD(context.Background(), chd, filepath.Join(dir, "out.cue"),
		chdman.ExtractCDOptions{SplitBin: split, ExtractOptions: chdman.ExtractOptions{Workers: 2}})
	if err != nil {
		t.Fatalf("ExtractCD(%s): %v", filepath.Base(chd), err)
	}
	if c.real {
		t.Logf("extraction warnings: %+v", res.Warnings)
	} else if got, want := kinds(res.Warnings), wantKinds(c.extractWarn...); !slices.Equal(got, want) {
		t.Errorf("extraction warnings %v, want %v", got, want)
	}
	return dir
}

// assertSameTracks compares the tracks that two readers report.
func assertSameTracks(t testing.TB, a, b *chdman.Reader) {
	t.Helper()
	ta, err := a.Tracks()
	if err != nil {
		t.Fatal(err)
	}
	tb, err := b.Tracks()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ta, tb) {
		t.Errorf("tracks %+v, other file's %+v", ta, tb)
	}
}

func extractChdman(t testing.TB, chd string, split bool) string {
	t.Helper()
	dir := t.TempDir()
	oracle.ExtractCD(t, chd, filepath.Join(dir, "out.cue"), split)
	return dir
}

// fileDigest identifies a file's content without keeping it in memory, which
// matters for a real image. The text of a .cue file is kept to explain a
// difference.
type fileDigest struct {
	size int64
	sum  string
	text []byte
}

func dirDigests(t testing.TB, dir string) map[string]fileDigest {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]fileDigest{}
	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		d := fileDigest{size: st.Size(), sum: hashFile(t, f)}
		f.Close()
		if strings.HasSuffix(e.Name(), ".cue") {
			d.text = readFile(t, path)
		}
		files[e.Name()] = d
	}
	return files
}

// assertSameDir compares two extraction directories by file name and content.
func assertSameDir(t testing.TB, got, want, what string) {
	t.Helper()
	g, w := dirDigests(t, got), dirDigests(t, want)
	for name, wd := range w {
		gd, ok := g[name]
		switch {
		case !ok:
			t.Errorf("%s: missing %s; have %v", what, name, slices.Sorted(mapsKeys(g)))
		case gd.sum != wd.sum && wd.text != nil:
			t.Errorf("%s: %s differs:\n got %q\nwant %q", what, name, gd.text, wd.text)
		case gd.sum != wd.sum:
			t.Errorf("%s: %s differs (%d bytes, want %d)", what, name, gd.size, wd.size)
		}
	}
	for name := range g {
		if _, ok := w[name]; !ok {
			t.Errorf("%s: unexpected %s", what, name)
		}
	}
}

// assertSameDirAndDrop is assertSameDir that then deletes got, so that the
// copies of a large image do not pile up while a cell runs.
func assertSameDirAndDrop(t testing.TB, got, want, what string) {
	t.Helper()
	assertSameDir(t, got, want, what)
	os.RemoveAll(got)
}

// concatBins joins every bin of dir in name order, which is track order.
func concatBins(t testing.TB, dir string) []byte {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, e := range ents { // ReadDir sorts by name
		if strings.HasSuffix(e.Name(), ".bin") {
			out = append(out, readFile(t, filepath.Join(dir, e.Name()))...)
		}
	}
	return out
}

func sectorSize(mode string) int {
	switch mode {
	case "MODE1/2048":
		return 2048
	case "MODE2/2336":
		return 2336
	}
	return 2352
}

func userOffset(mode string) int {
	switch mode {
	case "MODE1/2048":
		return 0
	case "MODE2/2336":
		return 8
	case "MODE1/2352":
		return 16
	case "MODE2/2352":
		return 24
	}
	return -1
}

// assertSectors compares ReadSector and ReadUserData, at the first, middle
// and last frame of each track, with the bytes of the generated bins. Later
// sessions are left out: their addresses are not exact, as documented.
func assertSectors(t testing.TB, r *chdman.Reader, d testimg.Disc, srcDir string) {
	t.Helper()
	tracks, err := r.Tracks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != len(d.Tracks) {
		t.Fatalf("%d tracks, want %d", len(tracks), len(d.Tracks))
	}
	bins := map[string][]byte{}
	used := map[string]int{}
	for i, tr := range d.Tracks {
		name := "disc.bin"
		if d.Split {
			name = fmt.Sprintf("track%02d.bin", i+1)
		}
		if _, ok := bins[name]; !ok {
			bins[name] = readFile(t, filepath.Join(srcDir, name))
		}
		size, start := sectorSize(tr.Mode), used[name]
		used[name] += tr.Frames * size
		if d.Session2From > 0 && i+1 >= d.Session2From {
			continue
		}
		n := tr.Frames - tr.IndexZero
		for _, j := range []int{0, n / 2, n - 1} {
			at := start + (tr.IndexZero+j)*size
			raw := bins[name][at : at+size]
			if tr.Mode == "AUDIO" { // CHDs hold audio big-endian
				raw = slices.Clone(raw)
				for k := 0; k+1 < len(raw); k += 2 {
					raw[k], raw[k+1] = raw[k+1], raw[k]
				}
			}
			lba := tracks[i].StartLBA + uint32(j)
			buf := make([]byte, 2352)
			if got, err := r.ReadSector(lba, buf, chdman.ReadOptions{}); err != nil || !bytes.Equal(buf[:got], raw) {
				t.Errorf("track %d frame %d: ReadSector(%d) = %d bytes, %v; differs from the bin", i+1, j, lba, got, err)
			}
			if off := userOffset(tr.Mode); off < 0 {
				if _, err := r.ReadUserData(lba, buf, chdman.ReadOptions{}); !errors.Is(err, chdman.ErrNoUserData) {
					t.Errorf("track %d: ReadUserData of audio: %v", i+1, err)
				}
			} else if got, err := r.ReadUserData(lba, buf, chdman.ReadOptions{}); err != nil || !bytes.Equal(buf[:got], raw[off:off+2048]) {
				t.Errorf("track %d frame %d: ReadUserData(%d) = %d bytes, %v; differs from the bin", i+1, j, lba, got, err)
			}
		}
	}
}

// ---- DVD cells -------------------------------------------------------------

type dvdCell struct {
	name   string
	build  func(t testing.TB, dir string) string // returns the iso path
	codecs string
	hunk   int
	// headerOnly: chdman's file is too small to read back.
	headerOnly bool
	// sparse: a huge sparse input; the cell only checks hashes, verification
	// and reads, not full extraction.
	sparse bool
	// selfRefs: the data repeats, so both files must contain self-references.
	selfRefs bool
	// real: one of the owner's own images; it runs alone and cleans up as it
	// goes.
	real bool
}

func isoCell(name string, sectors int, shape testimg.Shape) dvdCell {
	return dvdCell{name: name, build: func(t testing.TB, dir string) string { return testimg.ISO(t, dir, sectors, shape) }}
}

func dvdCells() (groups []string, cells map[string][]dvdCell) {
	cells = map[string][]dvdCell{}
	add := func(group string, c ...dvdCell) {
		if _, ok := cells[group]; !ok {
			groups = append(groups, group)
		}
		cells[group] = append(cells[group], c...)
	}
	// Sizes. The default hunk holds two sectors.
	small := func(name string, sectors int) dvdCell {
		c := isoCell(name, sectors, testimg.Mixed)
		c.headerOnly = sectors < 2*minReadableHunks
		return c
	}
	add("sizes", small("smaller-than-one-hunk", 1), small("exactly-one-hunk", 2), small("partial-last-hunk", 5),
		small("four-hunks", 8), small("odd-sector-count", 1001))
	for name, sectors := range map[string]int{"ps2-scaled": 4096, "psp-scaled": 2300} {
		add("sizes", dvdCell{name: name, build: func(t testing.TB, dir string) string {
			files := []testimg.File{{Name: "SYSTEM.CNF", Data: []byte("BOOT2 = cdrom0:\\SLUS_000.00;1\nVER = 1.00\n")},
				{Name: "PARAM.SFO", Data: bytes.Repeat([]byte("PSP_GAME "), 700)}}
			img := testimg.Filesystem(files...)
			rest := testimg.ISO(t, dir, sectors-len(img)/2048, testimg.Mixed)
			return testimg.WriteISO(t, dir, append(img, readFile(t, rest)...))
		}})
	}
	add("sizes", sparseCell("ps2-sized-4.7GB", 2295104), sparseCell("psp-sized-1.8GB", 900000))

	// Codecs on one mixed image.
	for _, c := range []string{"zlib", "zstd", "lzma", "huff", "flac", "lzma,zlib,huff,flac", "zlib,zstd", "none"} {
		cell := isoCell(c, 600, testimg.Mixed)
		cell.codecs = c
		add("codecs", cell)
	}
	// Data shapes at the default codecs.
	for name, shape := range map[string]testimg.Shape{"zeros": testimg.Zeros, "random": testimg.Random,
		"repeated": testimg.Repeated, "mixed": testimg.Mixed} {
		c := isoCell(name, 600, shape)
		c.selfRefs = shape == testimg.Repeated
		add("shapes", c)
	}
	// Hunk sizes.
	for _, hs := range []int{2048, 4096, 8192, 32768, 262144} {
		c := isoCell(fmt.Sprint(hs), 1024, testimg.Mixed)
		c.hunk = hs
		add("hunks", c)
	}
	c := isoCell("8192/zlib", 1024, testimg.Mixed)
	c.hunk, c.codecs = 8192, "zlib"
	add("hunks", c)
	return groups, cells
}

// sparseCell is an image of sectors 2048-byte sectors made mostly of holes,
// with real data at both ends.
func sparseCell(name string, sectors int64) dvdCell {
	return dvdCell{name: name, sparse: true, codecs: "zlib", hunk: 1 << 20, build: func(t testing.TB, dir string) string {
		path := filepath.Join(dir, "image.iso")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(sectors * 2048); err != nil {
			t.Skipf("cannot make a sparse file: %v", err)
		}
		fs := testimg.Filesystem(testimg.File{Name: "SYSTEM.CNF", Data: []byte("BOOT2 = cdrom0:\\SLUS_000.00;1\n")})
		tail := bytes.Repeat([]byte("tail data "), 400)
		if _, err := f.WriteAt(fs, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(tail, sectors*2048-int64(len(tail))); err != nil {
			t.Fatal(err)
		}
		return path
	}}
}

func TestDVD(t *testing.T) {
	oracle.Require(t)
	groups, cells := dvdCells()
	for _, g := range groups {
		t.Run(g, func(t *testing.T) {
			for _, c := range cells[g] {
				t.Run(c.name, func(t *testing.T) {
					if c.sparse && testing.Short() {
						t.Skip("writes and reads gigabytes")
					}
					if c.sparse && raceEnabled {
						// Hashing and compressing gigabytes under the race detector
						// outlasts the test timeout; the same code paths run at small
						// sizes in every other cell, and at full size without -race.
						t.Skip("gigabyte-sized cells are too slow under the race detector")
					}
					t.Parallel()
					runDVD(t, c)
				})
			}
		})
	}
}

// TestDVDRealImages runs the cells over the owner's own ISOs, one at a time.
func TestDVDRealImages(t *testing.T) {
	_, isos := realImages(t)
	if len(isos) == 0 {
		return
	}
	oracle.Require(t)
	for _, iso := range isos {
		t.Run(filepath.Base(iso), func(t *testing.T) {
			runDVD(t, dvdCell{name: filepath.Base(iso), real: true, build: func(testing.TB, string) string { return iso }})
		})
	}
}

// TestDVDInvalidSizes: an image that is empty or not a multiple of 2048
// bytes is refused by both tools, and neither leaves an output.
func TestDVDInvalidSizes(t *testing.T) {
	oracle.Require(t)
	for _, size := range []int{0, 1, 2047, 2049, 4097} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			iso := filepath.Join(dir, "bad.iso")
			if err := os.WriteFile(iso, bytes.Repeat([]byte{7}, size), 0o644); err != nil {
				t.Fatal(err)
			}
			theirs, ours := filepath.Join(dir, "theirs.chd"), filepath.Join(dir, "ours.chd")
			if err := oracle.Run(t, "createdvd", "-f", "-i", iso, "-o", theirs); err == nil {
				t.Error("chdman accepts the image")
			}
			if _, err := chdman.CreateDVD(context.Background(), iso, ours, chdman.CreateOptions{}); err == nil {
				t.Error("CreateDVD accepts the image")
			}
			if _, err := os.Stat(ours); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("CreateDVD left an output: %v", err)
			}
		})
	}
}

func runDVD(t *testing.T, c dvdCell) {
	ctx := context.Background()
	none := c.codecs == "none"
	work, srcDir := t.TempDir(), t.TempDir()
	iso := c.build(t, srcDir)
	isoInfo, err := os.Stat(iso)
	if err != nil {
		t.Fatal(err)
	}
	isoSize := isoInfo.Size()

	want := filepath.Join(work, "want.chd")
	oracle.CreateDVD(t, iso, want, c.codecs, c.hunk)
	wantHdr := readHeader(t, want)
	wantRaw, wantSum := hashes(wantHdr)
	readable := wantHdr.HunkCount() >= minReadableHunks
	if !readable && !c.headerOnly && !c.real {
		t.Fatalf("chdman's file has %d hunks; at least %d are needed to read it", wantHdr.HunkCount(), minReadableHunks)
	}
	big := c.real && wantHdr.LogicalBytes > bigRealBytes
	if big {
		t.Logf("%d bytes: skipping the corruption checks and the round trip's second create", wantHdr.LogicalBytes)
	}

	ours := filepath.Join(work, "ours.chd")
	res, err := chdman.CreateDVD(ctx, iso, ours, chdman.CreateOptions{Codecs: splitCodecs(c.codecs), HunkBytes: uint32(c.hunk), Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	// 1. Hash parity.
	if res.RawSHA1 != wantRaw {
		t.Errorf("rawsha1 %s, chdman %s", res.RawSHA1, wantRaw)
	}
	if res.SHA1 != wantSum {
		t.Errorf("sha1 %s, chdman %s", res.SHA1, wantSum)
	}
	if r, s := hashes(readHeader(t, ours)); r != res.RawSHA1 || s != res.SHA1 {
		t.Errorf("stored hashes %s/%s, result %s/%s", r, s, res.RawSHA1, res.SHA1)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %+v", res.Warnings)
	}
	// 2. chdman reads ours.
	if err := oracle.Verify(t, ours); err != nil {
		t.Errorf("chdman rejects our file: %v", err)
	}
	ro := openReader(t, ours)
	assertVerify(t, ro, none)
	if !none && !big {
		assertCorruptionCaught(t, ours)
	}
	if c.selfRefs {
		assertSelfReferences(t, ours)
	}
	if !c.sparse {
		out := filepath.Join(work, "chdman.iso")
		oracle.ExtractDVD(t, ours, out)
		assertSameFile(t, out, iso, "chdman's extraction of our file")
		os.Remove(out)
	}
	assertISOReads(t, c.name, ro, iso, isoSize)

	// 3. chdman-go reads chdman's file.
	if readable {
		rw := openReader(t, want)
		assertVerify(t, rw, none)
		if !none && !big {
			assertCorruptionCaught(t, want)
		}
		if c.selfRefs {
			assertSelfReferences(t, want)
		}
		assertISOReads(t, c.name+"/chdman", rw, iso, isoSize)
		if !c.sparse {
			out := filepath.Join(work, "ours-of-theirs.iso")
			if _, err := chdman.ExtractDVD(ctx, want, out, chdman.ExtractOptions{Workers: 2}); err != nil {
				t.Fatal(err)
			}
			assertSameFile(t, out, iso, "chdman-go extraction of chdman's file")
			os.Remove(out)
			assertSameReads(t, c.name, ro, rw)
		}
	}

	// 5. Round trip: DVD extraction loses nothing, so the hashes are kept.
	if c.sparse || big {
		return
	}
	out := filepath.Join(work, "ours.iso")
	if _, err := chdman.ExtractDVD(ctx, ours, out, chdman.ExtractOptions{Workers: 2}); err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, out, iso, "chdman-go extraction of our file")
	defer os.Remove(out)
	rt, err := chdman.CreateDVD(ctx, out, filepath.Join(work, "again.chd"), chdman.CreateOptions{Codecs: splitCodecs(c.codecs), HunkBytes: uint32(c.hunk), Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if rt.RawSHA1 != wantRaw || rt.SHA1 != wantSum {
		t.Errorf("round trip hashes %s/%s, want %s/%s", rt.RawSHA1, rt.SHA1, wantRaw, wantSum)
	}
}

func assertSameFile(t testing.TB, got, want, what string) {
	t.Helper()
	g, err := os.Open(got)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	w, err := os.Open(want)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	gs, ws := hashFile(t, g), hashFile(t, w)
	if gs != ws {
		t.Errorf("%s differs from %s", what, filepath.Base(want))
	}
}

func hashFile(t testing.TB, f *os.File) string {
	t.Helper()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// assertISOReads compares random byte ranges and whole sectors, the first,
// last, and a past-the-end one, with the original image.
func assertISOReads(t testing.TB, name string, r *chdman.Reader, iso string, size int64) {
	t.Helper()
	f, err := os.Open(iso)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := int64(r.Info().LogicalBytes); got != size {
		t.Fatalf("logical bytes %d, image %d", got, size)
	}
	rng := rngFor(name)
	for range 30 {
		n := 1 + rng.Int64N(min(size, 20000))
		off := rng.Int64N(size - n + 1)
		got, want := make([]byte, n), make([]byte, n)
		if _, err := r.ReadAt(got, off); err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", n, off, err)
		}
		if _, err := f.ReadAt(want, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("ReadAt(%d, %d) differs from the image", n, off)
		}
	}
	sectors := size / 2048
	buf, want := make([]byte, 2048), make([]byte, 2048)
	for _, lba := range []int64{0, sectors / 2, sectors - 1, rng.Int64N(sectors)} {
		if _, err := f.ReadAt(want, lba*2048); err != nil {
			t.Fatal(err)
		}
		if n, err := r.ReadSector(uint32(lba), buf, chdman.ReadOptions{}); err != nil || !bytes.Equal(buf[:n], want) {
			t.Errorf("ReadSector(%d) = %d bytes, %v; differs from the image", lba, n, err)
		}
		if n, err := r.ReadUserData(uint32(lba), buf, chdman.ReadOptions{}); err != nil || !bytes.Equal(buf[:n], want) {
			t.Errorf("ReadUserData(%d) = %d bytes, %v; differs from the image", lba, n, err)
		}
	}
	if _, err := r.ReadSector(uint32(sectors), buf, chdman.ReadOptions{}); !errors.Is(err, io.EOF) {
		t.Errorf("ReadSector past the end: %v, want io.EOF", err)
	}
}
