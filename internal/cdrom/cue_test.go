package cdrom_test

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// chdmanCreate has chdman build a CD CHD from cue and returns what its header
// and metadata say. It never reads the map: chdman writes a garbled map for
// some files of one to three hunks, and these inputs are small.
func chdmanCreate(t *testing.T, cue string) (format.Header, []format.MetadataEntry, error) {
	t.Helper()
	chd := filepath.Join(t.TempDir(), "cd.chd")
	if err := oracle.Run(t, "createcd", "-f", "-i", cue, "-o", chd); err != nil {
		return format.Header{}, nil, err
	}
	b, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := format.ReadMetadata(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return h, entries, nil
}

// sourcesRawSHA1 hashes the logical bytes of a CD CHD as the sources describe
// them: each frame holds the track's data bytes (byte-swapped when asked),
// zero subcode, then the zero pad and the padding to a multiple of four.
func sourcesRawSHA1(t *testing.T, toc cdrom.TOC, srcs []cdrom.TrackSource) [20]byte {
	t.Helper()
	h := sha1.New()
	files := map[string][]byte{}
	for i, tr := range toc.Tracks {
		src := srcs[i]
		if src.Frames+src.Pad != tr.Frames {
			t.Errorf("track %d: source frames %d + pad %d != toc frames %d", i+1, src.Frames, src.Pad, tr.Frames)
		}
		data, ok := files[src.Path]
		if !ok {
			var err error
			if data, err = os.ReadFile(src.Path); err != nil {
				t.Fatal(err)
			}
			files[src.Path] = data
		}
		bytesPerFrame := uint64(tr.DataSize + tr.SubSize)
		for f := range src.Frames {
			at := src.Offset + uint64(f)*bytesPerFrame
			if at+bytesPerFrame > uint64(len(data)) {
				t.Fatalf("track %d frame %d reads past the end of %s", i+1, f, src.Path)
			}
			frame := make([]byte, cdrom.FrameSize)
			copy(frame, data[at:at+bytesPerFrame])
			if src.Swap {
				for j := 0; j < cdrom.SectorSize; j += 2 {
					frame[j], frame[j+1] = frame[j+1], frame[j]
				}
			}
			h.Write(frame)
		}
		h.Write(make([]byte, (src.Pad+tr.ExtraFrames)*cdrom.FrameSize))
	}
	return [20]byte(h.Sum(nil))
}

// assertCueMatchesChdman has chdman build a CHD from cue, then checks that
// ParseCue agrees with it: the metadata entries, the logical size and the
// raw SHA1 of the data the sources describe. When chdman rejects the cue,
// ParseCue must too.
func assertCueMatchesChdman(t *testing.T, cue string) (cdrom.TOC, []cdrom.TrackSource, []cdrom.Warning) {
	t.Helper()
	oracle.Require(t)
	hdr, entries, chdErr := chdmanCreate(t, cue)
	c, err := cdrom.ParseCue(cue)
	toc, srcs, warns := c.TOC, c.Sources, c.Warnings
	if chdErr != nil {
		if err == nil {
			t.Fatalf("chdman rejects the cue (%v) but ParseCue accepts it", chdErr)
		}
		if !errors.Is(err, chderr.InvalidCue) && !errors.Is(err, chderr.UnsupportedDiscType) {
			t.Errorf("ParseCue error %v wraps neither InvalidCue nor UnsupportedDiscType", err)
		}
		return cdrom.TOC{}, nil, nil
	}
	if err != nil {
		t.Fatalf("chdman accepts the cue but ParseCue fails: %v", err)
	}
	if len(srcs) != len(toc.Tracks) {
		t.Fatalf("%d sources for %d tracks", len(srcs), len(toc.Tracks))
	}
	got := cdrom.Metadata(toc)
	if len(got) != len(entries) {
		t.Fatalf("Metadata gave %d entries, chdman wrote %d", len(got), len(entries))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], entries[i]) {
			t.Errorf("entry %d = %q, chdman wrote %q", i, got[i].Data, entries[i].Data)
		}
	}
	var logical uint64
	for _, tr := range toc.Tracks {
		logical += uint64(tr.Frames+tr.ExtraFrames) * cdrom.FrameSize
	}
	if logical != hdr.LogicalBytes {
		t.Errorf("logical bytes = %d, chdman wrote %d", logical, hdr.LogicalBytes)
	}
	if sum := sourcesRawSHA1(t, toc, srcs); sum != hdr.RawSHA1 {
		t.Errorf("raw SHA1 of the sources = %x, chdman wrote %x", sum, hdr.RawSHA1)
	}
	return toc, srcs, warns
}

// concatBin writes the bins of tracks, back to back, to dir/name.
func concatBin(t *testing.T, dir, name string, tracks ...testimg.Track) string {
	t.Helper()
	var all []byte
	for i, tr := range tracks {
		p := testimg.Bin(t, dir, fmt.Sprintf("part%d.tmp", i), tr)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, all, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCueLayouts(t *testing.T) {
	oracle.Require(t)
	mixed := testimg.Mixed
	data := func(mode string, frames int) testimg.Track {
		return testimg.Track{Mode: mode, Frames: frames, Shape: mixed}
	}
	audio := func(frames int) testimg.Track { return testimg.Track{Mode: "AUDIO", Frames: frames, Shape: mixed} }

	for _, l := range testimg.Layouts() {
		t.Run(l.Name, func(t *testing.T) {
			assertCueMatchesChdman(t, testimg.Cue(t, t.TempDir(), l.Disc))
		})
	}

	// Hand-written layouts: MOTOROLA files and REM LEAD-OUT multisession.
	t.Run("motorola", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "a.bin", data("MODE1/2352", 20))
		testimg.Bin(t, dir, "b.bin", data("MODE1/2352", 20))
		testimg.Bin(t, dir, "c.bin", audio(20))
		cue := testimg.WriteCue(t, dir, "m.cue", `FILE "a.bin" MOTOROLA
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "b.bin" BINARY
  TRACK 02 MODE1/2352
    INDEX 01 00:00:00
FILE "c.bin" MOTOROLA
  TRACK 03 AUDIO
    INDEX 01 00:00:00
`)
		assertCueMatchesChdman(t, cue)
	})
	for _, ext := range []string{"bin", "img", "IMG"} {
		t.Run("multisession-lead-out-single-"+ext, func(t *testing.T) {
			dir := t.TempDir()
			// The first session is 20 frames, followed by 10 frames of filler.
			concatBin(t, dir, "disc."+ext, data("MODE1/2352", 30), audio(20))
			cue := testimg.WriteCue(t, dir, "s.cue", fmt.Sprintf(`FILE "disc.%s" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT 00:00:20
REM SESSION 02
  TRACK 02 AUDIO
    INDEX 01 00:00:30
`, ext))
			assertCueMatchesChdman(t, cue)
		})
		t.Run("multisession-lead-out-multi-"+ext, func(t *testing.T) {
			dir := t.TempDir()
			testimg.Bin(t, dir, "a."+ext, data("MODE1/2352", 20))
			testimg.Bin(t, dir, "b."+ext, audio(20))
			testimg.Bin(t, dir, "c."+ext, data("MODE1/2352", 12))
			cue := testimg.WriteCue(t, dir, "s.cue", fmt.Sprintf(`FILE "a.%[1]s" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "b.%[1]s" BINARY
  TRACK 02 AUDIO
    INDEX 01 00:00:00
  REM LEAD-OUT 00:00:10
REM SESSION 02
  REM LEAD-IN 00:00:20
FILE "c.%[1]s" BINARY
  TRACK 03 MODE1/2352
    INDEX 01 00:00:00
`, ext))
			assertCueMatchesChdman(t, cue)
		})
	}
}

func TestCueWildFormats(t *testing.T) {
	oracle.Require(t)
	disc := func(t *testing.T) (dir string) {
		dir = t.TempDir()
		testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed})
		return dir
	}
	const plain = "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"

	t.Run("crlf", func(t *testing.T) {
		cue := testimg.WriteCue(t, disc(t), "d.cue", strings.ReplaceAll(plain, "\n", "\r\n"))
		assertCueMatchesChdman(t, cue)
	})
	t.Run("utf8-bom", func(t *testing.T) {
		// chdman's tokenizer keeps the BOM in the first token, so it never
		// sees the FILE command. Whatever it does with the cue, we do.
		cue := testimg.WriteCue(t, disc(t), "d.cue", "\xef\xbb\xbf"+plain)
		assertCueMatchesChdman(t, cue)
	})
	t.Run("quoted-name-with-spaces", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "my disc image.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed})
		cue := testimg.WriteCue(t, dir, "d.cue", strings.Replace(plain, "disc.bin", "my disc image.bin", 1))
		assertCueMatchesChdman(t, cue)
	})
	t.Run("single-quoted-name", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "it is.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed})
		cue := testimg.WriteCue(t, dir, "d.cue", "FILE 'it is.bin' BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		assertCueMatchesChdman(t, cue)
	})
	t.Run("non-ascii-name", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "ディスク é.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed})
		cue := testimg.WriteCue(t, dir, "d.cue", strings.Replace(plain, "disc.bin", "ディスク é.bin", 1))
		assertCueMatchesChdman(t, cue)
	})
	t.Run("cue-in-another-directory", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed})
		cue := testimg.WriteCue(t, dir, "cues/d.cue", strings.Replace(plain, "disc.bin", "../disc.bin", 1))
		assertCueMatchesChdman(t, cue)
	})
	t.Run("comments-and-catalog", func(t *testing.T) {
		cue := testimg.WriteCue(t, disc(t), "d.cue", `REM GENRE Test
REM COMMENT "ExactAudioCopy v1.0"
CATALOG 0123456789012
PERFORMER "Someone"
TITLE "A title"
FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    FLAGS DCP PRE
    ISRC ABCDE1234567
    TITLE "Track"
    INDEX 01 00:00:00
`)
		assertCueMatchesChdman(t, cue)
	})
	t.Run("long-line-split-at-510", func(t *testing.T) {
		// fgets(buf, 511) splits a long line, and the rest of it is read
		// as a line of its own.
		head := "REM " + strings.Repeat("x", 510-4)
		cue := testimg.WriteCue(t, disc(t), "d.cue", head+"FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		assertCueMatchesChdman(t, cue)
	})
	t.Run("no-trailing-newline", func(t *testing.T) {
		cue := testimg.WriteCue(t, disc(t), "d.cue", strings.TrimSuffix(plain, "\n"))
		assertCueMatchesChdman(t, cue)
	})
	t.Run("bare-frame-count", func(t *testing.T) {
		dir := t.TempDir()
		concatBin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20}, testimg.Track{Mode: "AUDIO", Frames: 10})
		cue := testimg.WriteCue(t, dir, "d.cue", `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 0
  TRACK 02 AUDIO
    PREGAP 150
    INDEX 01 20
`)
		assertCueMatchesChdman(t, cue)
	})
}

// rejection is a cue that ParseCue must refuse.
type rejection struct {
	name string
	cue  func(t *testing.T) string
	want error
	// chdmanFails is whether chdman rejects it too.
	chdmanFails bool
}

func rejections() []rejection {
	oneTrack := func(t *testing.T, dir, text string) string {
		testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 8})
		return testimg.WriteCue(t, dir, "d.cue", text)
	}
	return []rejection{
		{"gdi-style-multi-cue", func(t *testing.T) string {
			dir := t.TempDir()
			testimg.Bin(t, dir, "t1.bin", testimg.Track{Mode: "MODE1/2352", Frames: 8})
			testimg.Bin(t, dir, "t2.bin", testimg.Track{Mode: "MODE1/2352", Frames: 8})
			return testimg.WriteCue(t, dir, "g.cue", `REM SINGLE-DENSITY AREA
FILE "t1.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
REM HIGH-DENSITY AREA
FILE "t2.bin" BINARY
  TRACK 02 MODE1/2352
    INDEX 01 00:00:00
`)
		}, chderr.UnsupportedDiscType, false},
		{"wave", func(t *testing.T) string {
			return oneTrack(t, t.TempDir(), "FILE \"disc.bin\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n")
		}, chderr.InvalidCue, true},
		{"cdg", func(t *testing.T) string {
			return oneTrack(t, t.TempDir(), "FILE \"disc.bin\" BINARY\n  TRACK 01 CDG\n    INDEX 01 00:00:00\n")
		}, chderr.InvalidCue, true},
		{"unknown-file-type", func(t *testing.T) string {
			return oneTrack(t, t.TempDir(), "FILE \"disc.bin\" MP3\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		}, chderr.InvalidCue, true},
		{"missing-index-01", func(t *testing.T) string {
			return oneTrack(t, t.TempDir(), "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n")
		}, chderr.InvalidCue, true},
		{"missing-bin", func(t *testing.T) string {
			return testimg.WriteCue(t, t.TempDir(), "d.cue", "FILE \"gone.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		}, chderr.InvalidCue, true},
		{"zero-byte-bin", func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "disc.bin"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return testimg.WriteCue(t, dir, "d.cue", "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		}, chderr.InvalidCue, true},
		{"no-tracks", func(t *testing.T) string {
			return testimg.WriteCue(t, t.TempDir(), "d.cue", "REM nothing here\n")
		}, chderr.InvalidCue, false}, // chdman loops forever on a cue without tracks
		{"lowercase-keywords", func(t *testing.T) string {
			// The tokenizer's comparisons are case sensitive, so no command is
			// recognised: no tracks, which hangs chdman.
			return oneTrack(t, t.TempDir(), "file \"disc.bin\" binary\n  track 01 mode1/2352\n    index 01 00:00:00\n")
		}, chderr.InvalidCue, false},
		{"track-number-zero", func(t *testing.T) string {
			return oneTrack(t, t.TempDir(), "FILE \"disc.bin\" BINARY\n  TRACK 00 MODE1/2352\n    INDEX 01 00:00:00\n")
		}, chderr.InvalidCue, false},
		{"track-number-over-99", func(t *testing.T) string {
			return oneTrack(t, t.TempDir(), "FILE \"disc.bin\" BINARY\n  TRACK 100 MODE1/2352\n    INDEX 01 00:00:00\n")
		}, chderr.InvalidCue, false},
		{"index-over-99", func(t *testing.T) string {
			return oneTrack(t, t.TempDir(), "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 100 00:00:00\n")
		}, chderr.InvalidCue, true},
	}
}

func TestCueRejections(t *testing.T) {
	for _, r := range rejections() {
		t.Run(r.name, func(t *testing.T) {
			_, err := cdrom.ParseCue(r.cue(t))
			if err == nil {
				t.Fatal("ParseCue accepted the cue")
			}
			if !errors.Is(err, r.want) {
				t.Errorf("error %v does not wrap %v", err, r.want)
			}
		})
	}
	t.Run("names-wave-and-cdg", func(t *testing.T) {
		for _, name := range []string{"wave", "cdg"} {
			for _, r := range rejections() {
				if r.name != name {
					continue
				}
				_, err := cdrom.ParseCue(r.cue(t))
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), name) {
					t.Errorf("%s: error %v does not name it", name, err)
				}
			}
		}
	})
	t.Run("missing-cue", func(t *testing.T) {
		_, err := cdrom.ParseCue(filepath.Join(t.TempDir(), "none.cue"))
		if !errors.Is(err, chderr.InvalidCue) || !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error %v, want InvalidCue and ErrNotExist", err)
		}
	})
}

// TestCueRejectionsMatchChdman checks that chdman refuses the same cues.
func TestCueRejectionsMatchChdman(t *testing.T) {
	oracle.Require(t)
	for _, r := range rejections() {
		if !r.chdmanFails {
			continue
		}
		t.Run(r.name, func(t *testing.T) {
			if _, _, err := chdmanCreate(t, r.cue(t)); err == nil {
				t.Error("chdman accepted the cue")
			}
		})
	}
}

func TestCueWarnings(t *testing.T) {
	mixed := testimg.Mixed
	appendBytes := func(t *testing.T, path string, n int) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(bytes.Repeat([]byte{0xa5}, n)); err != nil {
			t.Fatal(err)
		}
	}
	oracleCheck := func(t *testing.T, cue string) []cdrom.Warning {
		t.Helper()
		_, _, warns := assertCueMatchesChdman(t, cue)
		return warns
	}

	t.Run("clean", func(t *testing.T) {
		c, err := cdrom.ParseCue(testimg.Cue(t, t.TempDir(), testimg.Disc{Tracks: []testimg.Track{
			{Mode: "MODE1/2352", Frames: 8, Shape: mixed}, {Mode: "AUDIO", Frames: 6, Shape: mixed}}}))
		if err != nil || len(c.Warnings) != 0 {
			t.Fatalf("err = %v, warnings = %v", err, c.Warnings)
		}
	})
	t.Run("trailing-bytes-single-track", func(t *testing.T) {
		dir := t.TempDir()
		cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{{Mode: "MODE1/2352", Frames: 20, Shape: mixed}}})
		appendBytes(t, filepath.Join(dir, "disc.bin"), 3)
		warns := oracleCheck(t, cue)
		if len(warns) != 1 || warns[0].Kind != "trailing-bytes-dropped" || warns[0].Track != 1 || !strings.Contains(warns[0].Message, "3") {
			t.Fatalf("warnings = %+v", warns)
		}
	})
	t.Run("trailing-bytes-last-track-of-shared-bin", func(t *testing.T) {
		dir := t.TempDir()
		cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{
			{Mode: "MODE1/2352", Frames: 20, Shape: mixed}, {Mode: "AUDIO", Frames: 10, Shape: mixed}}})
		appendBytes(t, filepath.Join(dir, "disc.bin"), 3)
		warns := oracleCheck(t, cue)
		if len(warns) != 1 || warns[0].Kind != "trailing-bytes-dropped" || warns[0].Track != 2 {
			t.Fatalf("warnings = %+v", warns)
		}
	})
	t.Run("trailing-bytes-split-bin", func(t *testing.T) {
		dir := t.TempDir()
		cue := testimg.Cue(t, dir, testimg.Disc{Split: true, Tracks: []testimg.Track{
			{Mode: "MODE1/2352", Frames: 20, Shape: mixed}, {Mode: "AUDIO", Frames: 10, Shape: mixed}}})
		appendBytes(t, filepath.Join(dir, "track01.bin"), 100)
		warns := oracleCheck(t, cue)
		if len(warns) != 1 || warns[0].Track != 1 || warns[0].Kind != "trailing-bytes-dropped" {
			t.Fatalf("warnings = %+v", warns)
		}
	})
	t.Run("frames-overwritten", func(t *testing.T) {
		dir := t.TempDir()
		concatBin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 30, Shape: mixed}, testimg.Track{Mode: "AUDIO", Frames: 20, Shape: mixed})
		cue := testimg.WriteCue(t, dir, "s.cue", `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT 00:00:20
REM SESSION 02
  TRACK 02 AUDIO
    INDEX 01 00:00:30
`)
		warns := oracleCheck(t, cue)
		if len(warns) != 1 || warns[0].Kind != "frames-overwritten" || warns[0].Track != 1 {
			t.Fatalf("warnings = %+v", warns)
		}
	})
	// A track's file is fixed when its TRACK command comes, so a bin that a
	// later FILE names is never read.
	unused := func(t *testing.T, cue string, want ...string) {
		t.Helper()
		var got []string
		for _, w := range oracleCheck(t, cue) {
			if w.Kind == cdrom.WarnFileUnused {
				got = append(got, filepath.Base(strings.Fields(w.Message)[0]))
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("unused files warned about = %v, want %v", got, want)
		}
	}
	twoBins := func(t *testing.T) string {
		dir := t.TempDir()
		testimg.Bin(t, dir, "a.bin", testimg.Track{Mode: "MODE1/2352", Frames: 60, Shape: mixed})
		testimg.Bin(t, dir, "b.bin", testimg.Track{Mode: "AUDIO", Frames: 50, Shape: mixed})
		return dir
	}
	t.Run("file-after-the-last-track", func(t *testing.T) {
		dir := twoBins(t)
		unused(t, testimg.WriteCue(t, dir, "d.cue", "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\nFILE \"b.bin\" BINARY\n"), "b.bin")
	})
	t.Run("file-after-file", func(t *testing.T) {
		dir := twoBins(t)
		unused(t, testimg.WriteCue(t, dir, "d.cue", "FILE \"b.bin\" BINARY\nFILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"), "b.bin")
	})
	t.Run("file-between-track-and-index", func(t *testing.T) {
		dir := twoBins(t)
		unused(t, testimg.WriteCue(t, dir, "d.cue", "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\nFILE \"b.bin\" BINARY\n    INDEX 01 00:00:00\n"), "b.bin")
	})
	t.Run("two-unused-files", func(t *testing.T) {
		dir := twoBins(t)
		testimg.Bin(t, dir, "c.bin", testimg.Track{Mode: "AUDIO", Frames: 20, Shape: mixed})
		unused(t, testimg.WriteCue(t, dir, "d.cue", "FILE \"c.bin\" BINARY\nFILE \"b.bin\" BINARY\nFILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"), "c.bin", "b.bin")
	})
	t.Run("unused-file-without-data-is-clean", func(t *testing.T) {
		dir := twoBins(t)
		if err := os.WriteFile(filepath.Join(dir, "empty.bin"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		unused(t, testimg.WriteCue(t, dir, "d.cue", "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\nFILE \"empty.bin\" BINARY\nFILE \"gone.bin\" BINARY\n"))
	})
	t.Run("used-files-are-clean", func(t *testing.T) {
		dir := twoBins(t)
		// A file named twice, and a file named before the track that reads it.
		unused(t, testimg.WriteCue(t, dir, "d.cue", `FILE "a.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "a.bin" BINARY
  TRACK 02 AUDIO
    INDEX 01 00:00:30
FILE "b.bin" BINARY
  TRACK 03 AUDIO
    INDEX 01 00:00:00
`))
	})

	// The last track of a single-bin multisession cue ends at its own
	// lead-out, and the rest of the bin is dropped.
	for _, ext := range []string{"bin", "img"} {
		t.Run("last-track-trimmed-"+ext, func(t *testing.T) {
			dir := t.TempDir()
			testimg.Bin(t, dir, "disc."+ext, testimg.Track{Mode: "MODE1/2352", Frames: 2000, Shape: mixed})
			cue := testimg.WriteCue(t, dir, "s.cue", fmt.Sprintf(`FILE "disc.%s" BINARY
REM SESSION 01
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT 00:05:00
REM SESSION 02
  TRACK 02 MODE1/2352
    INDEX 00 00:06:00
    INDEX 01 00:06:00
  REM LEAD-OUT 00:10:00
`, ext))
			_, _, warns := assertCueMatchesChdman(t, cue)
			var dropped []cdrom.Warning
			for _, w := range warns {
				if w.Kind == cdrom.WarnFramesDropped {
					dropped = append(dropped, w)
				}
			}
			// Track 1 keeps 375 frames, so track 2 starts at frame 375 of the
			// bin and ends 300 frames later, at its lead-out. What follows up
			// to frame 2000 is never read.
			if len(dropped) != 1 || dropped[0].Track != 2 || !strings.Contains(dropped[0].Message, " 1325 frames") {
				t.Errorf("want one warning for track 2 about 1325 frames, got %+v", dropped)
			}
		})
	}
	t.Run("lead-out-with-img-is-clean", func(t *testing.T) {
		dir := t.TempDir()
		concatBin(t, dir, "disc.img", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: mixed}, testimg.Track{Mode: "AUDIO", Frames: 20, Shape: mixed})
		cue := testimg.WriteCue(t, dir, "s.cue", `FILE "disc.img" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT 00:00:20
REM SESSION 02
  TRACK 02 AUDIO
    INDEX 01 00:00:30
`)
		if warns := oracleCheck(t, cue); len(warns) != 0 {
			t.Fatalf("warnings = %+v", warns)
		}
	})
}

func TestAudioSwap(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d"} {
		testimg.Bin(t, dir, n+".bin", testimg.Track{Mode: "AUDIO", Frames: 8})
	}
	cue := testimg.WriteCue(t, dir, "s.cue", `FILE "a.bin" MOTOROLA
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "b.bin" BINARY
  TRACK 02 MODE1/2352
    INDEX 01 00:00:00
FILE "c.bin" BINARY
  TRACK 03 AUDIO
    INDEX 01 00:00:00
FILE "d.bin" MOTOROLA
  TRACK 04 AUDIO
    INDEX 01 00:00:00
`)
	c, err := cdrom.ParseCue(cue)
	srcs := c.Sources
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, false, true, true}
	for i, s := range srcs {
		if s.Swap != want[i] {
			t.Errorf("track %d: Swap = %v, want %v", i+1, s.Swap, want[i])
		}
	}
}

func TestParseCueSources(t *testing.T) {
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 20}, {Mode: "AUDIO", Frames: 10}}})
	c, err := cdrom.ParseCue(cue)
	toc, srcs := c.TOC, c.Sources
	if err != nil {
		t.Fatal(err)
	}
	if len(toc.Tracks) != 2 || toc.Sessions != 1 || toc.Flags != 0 {
		t.Fatalf("toc = %+v", toc)
	}
	bin := filepath.Join(dir, "disc.bin")
	if srcs[0].Path != bin || srcs[1].Path != bin {
		t.Errorf("paths = %q, %q, want %q", srcs[0].Path, srcs[1].Path, bin)
	}
	if srcs[0].Offset != 0 || srcs[1].Offset != 20*2352 || srcs[0].Frames != 20 || srcs[1].Frames != 10 {
		t.Errorf("sources = %+v", srcs)
	}
	if toc.Tracks[1].ExtraFrames != 2 || toc.Tracks[1].ChdFrameOfs != 20 {
		t.Errorf("track 2 = %+v", toc.Tracks[1])
	}
}

// chdman indexes its track arrays with -1 for a command that comes before the
// first TRACK, which happens to do no harm: the cue converts as if the command
// were not there. TRACK 00 selects the same slot.
func TestCueBeforeFirstTrack(t *testing.T) {
	oracle.Require(t)
	const track = "  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"
	for name, text := range map[string]string{
		"index":         "FILE \"disc.bin\" BINARY\n    INDEX 01 00:00:00\n" + track,
		"index-0":       "FILE \"disc.bin\" BINARY\n    INDEX 00 00:00:00\n" + track,
		"pregap":        "FILE \"disc.bin\" BINARY\n    PREGAP 00:02:00\n" + track,
		"postgap":       "FILE \"disc.bin\" BINARY\n    POSTGAP 00:02:00\n" + track,
		"lead-out":      "REM LEAD-OUT 00:02:00\nFILE \"disc.bin\" BINARY\n" + track,
		"track-00":      "FILE \"disc.bin\" BINARY\n  TRACK 00 MODE1/2352\n    INDEX 01 00:00:00\n" + track,
		"track-00-pre":  "FILE \"disc.bin\" BINARY\n  TRACK 00 MODE1/2352\n    PREGAP 00:02:00\n    INDEX 01 00:00:10\n" + track,
		"track-0-alias": "FILE \"disc.bin\" BINARY\n  TRACK x MODE1/2352\n    INDEX 01 00:00:00\n" + track,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 100, Shape: testimg.Mixed})
			toc, _, _ := assertCueMatchesChdman(t, testimg.WriteCue(t, dir, "d.cue", text))
			if len(toc.Tracks) != 1 || toc.Tracks[0].Pregap != 0 {
				t.Errorf("tracks = %+v", toc.Tracks)
			}
		})
	}
}

// sparseBin creates a file of size bytes that takes no disk space.
func sparseBin(t *testing.T, dir, name string, size int64) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Skipf("cannot make a sparse file: %v", err)
	}
}

// bigTrackCue has a first track of 2,000,000 frames, 4,704,000,000 bytes, in
// a bin of 5 GiB.
func bigTrackCue(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sparseBin(t, dir, "a.bin", 5<<30)
	return testimg.WriteCue(t, dir, "d.cue", `FILE "a.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    INDEX 01 444:26:50
`)
}

// chdman multiplies the frames of the previous track by its frame size in 32
// bits, so track 2 starts 4,704,000,000 mod 2^32 bytes into the bin.
func TestCuePreviousTrackSizeWraps(t *testing.T) {
	cue := bigTrackCue(t)
	c, err := cdrom.ParseCue(cue)
	toc, srcs, warns := c.TOC, c.Sources, c.Warnings
	if err != nil {
		t.Fatal(err)
	}
	const wrapped = 4704000000 - 1<<32
	if got := srcs[1].Offset; got != wrapped {
		t.Errorf("track 2 offset = %d, want %d", got, wrapped)
	}
	if want := uint32((5<<30 - wrapped) / 2352); toc.Tracks[1].Frames != want {
		t.Errorf("track 2 frames = %d, want %d", toc.Tracks[1].Frames, want)
	}
	var found bool
	for _, w := range warns {
		if w.Kind == cdrom.WarnOffsetTruncated && w.Track == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s warning for track 2: %+v", cdrom.WarnOffsetTruncated, warns)
	}
}

// singleBinMultisession is a two-session single-bin cue with a lead-out time
// on track 1, the shape that IsoBuster and ImgBurn write. The numbers are
// chosen by the callers to break the assumptions that a gap fits its tracks:
// chdman converts all of these, with its unsigned arithmetic wrapping.
func singleBinMultisession(t *testing.T, ext string, frames int, leadOut, track2 string) string {
	t.Helper()
	dir := t.TempDir()
	testimg.Bin(t, dir, "disc."+ext, testimg.Track{Mode: "MODE1/2352", Frames: frames, Shape: testimg.Mixed})
	return testimg.WriteCue(t, dir, "s.cue", fmt.Sprintf(`FILE "disc.%s" BINARY
REM SESSION 01
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT %s
REM SESSION 02
  TRACK 02 MODE1/2352
    INDEX 00 %[3]s
    INDEX 01 %[3]s
`, ext, leadOut, track2))
}

func TestCueMultisessionGapsThatDoNotFit(t *testing.T) {
	oracle.Require(t)
	for _, tc := range []struct {
		name            string
		frames          int
		leadOut, track2 string
	}{
		// A first session of 75 frames with a gap of 675.
		{"gap-larger-than-first-session", 2000, "00:01:00", "00:10:00"},
		// A gap larger than what follows it.
		{"gap-larger-than-next-track", 1200, "00:05:00", "00:15:00"},
		// A lead-out time after the start of the next track.
		{"lead-out-past-next-track", 2000, "00:10:00", "00:06:00"},
		{"lead-out-at-next-track", 2000, "00:06:00", "00:06:00"},
	} {
		for _, ext := range []string{"bin", "img"} {
			t.Run(tc.name+"/"+ext, func(t *testing.T) {
				assertCueMatchesChdman(t, singleBinMultisession(t, ext, tc.frames, tc.leadOut, tc.track2))
			})
		}
	}
}

// chdman picks the parser from the extension. The other formats are not
// supported here, and a big image must not be read whole looking for a cue.
func TestParseCueOtherFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"disc.iso", chderr.InvalidCue}, {"disc.ISO", chderr.InvalidCue},
		{"disc.cdr", chderr.InvalidCue}, {"disc.toast", chderr.InvalidCue},
		{"disc.nrg", chderr.InvalidCue}, {"disc.toc", chderr.InvalidCue},
		{"disc.gdi", chderr.UnsupportedDiscType}, {"disc.GDI", chderr.UnsupportedDiscType},
		{"dir.d/disc.iso", chderr.InvalidCue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			// A file that is a perfectly good cue if it is read as one.
			good := "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"
			testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 8})
			if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := cdrom.ParseCue(path)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == chderr.InvalidCue && !strings.Contains(err.Error(), ".cue") {
				t.Errorf("error %q does not name the supported input", err)
			}
		})
	}
	t.Run("no extension is a cue", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 8})
		path := testimg.WriteCue(t, dir, "sheet", "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		if _, err := cdrom.ParseCue(path); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a big file is not read as a cue", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "big")
		if err := os.WriteFile(path, make([]byte, 5<<20), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := cdrom.ParseCue(path)
		if !errors.Is(err, chderr.InvalidCue) || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("err = %v, want InvalidCue, too large", err)
		}
	})
}
