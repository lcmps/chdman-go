package chdman

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// chdmanHashes has chdman build a CD CHD and returns the hashes of its
// header. It never reads the map: chdman writes a garbled map for some files
// of one to three hunks, and these fixtures are small.
func chdmanHashes(t *testing.T, cue, codecs string, hunkBytes int) (raw, sum string) {
	t.Helper()
	chd := filepath.Join(t.TempDir(), "want.chd")
	oracle.CreateCD(t, cue, chd, codecs, hunkBytes)
	b, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.RawSHA1[:]), hex.EncodeToString(h.SHA1[:])
}

// assertCreateCDMatches runs CreateCD and compares its hashes, reported and
// stored, with chdman's, then has chdman verify our file.
func assertCreateCDMatches(t *testing.T, cue string, opts CreateOptions, codecs string, hunkBytes int) CreateResult {
	t.Helper()
	wantRaw, wantSum := chdmanHashes(t, cue, codecs, hunkBytes)
	out := filepath.Join(t.TempDir(), "got.chd")
	res, err := CreateCD(context.Background(), cue, out, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.RawSHA1 != wantRaw {
		t.Errorf("rawsha1 %s, chdman %s", res.RawSHA1, wantRaw)
	}
	if res.SHA1 != wantSum {
		t.Errorf("sha1 %s, chdman %s", res.SHA1, wantSum)
	}
	if info := chdInfo(t, out); info.RawSHA1 != res.RawSHA1 || info.SHA1 != res.SHA1 {
		t.Errorf("stored hashes %s/%s, result %s/%s", info.RawSHA1, info.SHA1, res.RawSHA1, res.SHA1)
	}
	if err := oracle.Verify(t, out); err != nil {
		t.Errorf("chdman rejects our file: %v", err)
	}
	return res
}

func TestCreateCDHashParity(t *testing.T) {
	oracle.Require(t)
	for _, l := range testimg.Layouts() {
		t.Run(l.Name, func(t *testing.T) {
			cue := testimg.Cue(t, t.TempDir(), l.Disc)
			assertCreateCDMatches(t, cue, CreateOptions{}, "", 0)
		})
	}
}

func TestCreateCDEachCodec(t *testing.T) {
	oracle.Require(t)
	for _, c := range []string{"cdzl", "cdzs", "cdlz", "cdfl", "lzma", "cdlz,cdzl", "none"} {
		t.Run(c, func(t *testing.T) {
			dir := t.TempDir()
			cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed}}})
			out := filepath.Join(t.TempDir(), "got.chd")
			opts := CreateOptions{Codecs: strings.Split(c, ",")}
			if _, err := CreateCD(context.Background(), cue, out, opts); err != nil {
				t.Fatal(err)
			}
			if err := oracle.Verify(t, out); err != nil {
				t.Errorf("chdman rejects our file: %v", err)
			}
			if c != "none" {
				if got := chdInfo(t, out).Codecs[0]; got != opts.Codecs[0] {
					t.Errorf("first codec %s, want %s", got, opts.Codecs[0])
				}
				return
			}
			// An uncompressed CHD has no hashes: compare the data itself.
			extracted := filepath.Join(t.TempDir(), "x.cue")
			oracle.ExtractCD(t, out, extracted, false)
			want, err := os.ReadFile(filepath.Join(dir, "disc.bin"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(filepath.Dir(extracted), "x.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Error("chdman extracts different data than the bin we read")
			}
		})
	}
}

func TestCreateCDNonDefaultHunk(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Layouts()[4].Disc)
	t.Run("9792", func(t *testing.T) {
		assertCreateCDMatches(t, cue, CreateOptions{HunkBytes: 9792}, "", 9792)
	})
	t.Run("not a frame multiple", func(t *testing.T) {
		out := filepath.Join(dir, "bad.chd")
		if _, err := CreateCD(context.Background(), cue, out, CreateOptions{HunkBytes: 9800}); err == nil {
			t.Fatal("expected an error")
		}
		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("output exists: %v", err)
		}
	})
}

func TestCreateCDWarnings(t *testing.T) {
	oracle.Require(t)
	mode1 := testimg.Track{Mode: "MODE1/2352", Frames: 30, Shape: testimg.Mixed}
	audio := testimg.Track{Mode: "AUDIO", Frames: 20, Shape: testimg.Mixed}

	t.Run("trailing bytes", func(t *testing.T) {
		dir := t.TempDir()
		bin := testimg.Bin(t, dir, "disc.bin", mode1)
		f, err := os.OpenFile(bin, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(bytes.Repeat([]byte{0xAA}, 100))
		f.Close()
		cue := testimg.WriteCue(t, dir, "d.cue", "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		res := assertCreateCDMatches(t, cue, CreateOptions{}, "", 0)
		if len(res.Warnings) != 1 || res.Warnings[0].Kind != WarnTrailingBytesDropped || res.Warnings[0].Track != 1 {
			t.Errorf("warnings %+v", res.Warnings)
		}
	})
	t.Run("lead-out", func(t *testing.T) {
		dir := t.TempDir()
		a := testimg.Bin(t, dir, "a.tmp", mode1)
		b := testimg.Bin(t, dir, "b.tmp", audio)
		var all []byte
		for _, p := range []string{a, b} {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, data...)
			os.Remove(p)
		}
		if err := os.WriteFile(filepath.Join(dir, "disc.bin"), all, 0o644); err != nil {
			t.Fatal(err)
		}
		cue := testimg.WriteCue(t, dir, "s.cue", `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT 00:00:20
REM SESSION 02
  TRACK 02 AUDIO
    INDEX 01 00:00:30
`)
		res := assertCreateCDMatches(t, cue, CreateOptions{}, "", 0)
		if len(res.Warnings) != 1 || res.Warnings[0].Kind != WarnFramesOverwritten {
			t.Errorf("warnings %+v", res.Warnings)
		}
	})
}

func TestCreateCDWarnsAboutDroppedInput(t *testing.T) {
	oracle.Require(t)
	t.Run("unused file", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "a.bin", testimg.Track{Mode: "MODE1/2352", Frames: 60, Shape: testimg.Mixed})
		testimg.Bin(t, dir, "b.bin", testimg.Track{Mode: "AUDIO", Frames: 50, Shape: testimg.Mixed})
		cue := testimg.WriteCue(t, dir, "d.cue", "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\nFILE \"b.bin\" BINARY\n")
		res := assertCreateCDMatches(t, cue, CreateOptions{}, "", 0)
		if len(res.Warnings) != 1 || res.Warnings[0].Kind != WarnFileUnused || !strings.Contains(res.Warnings[0].Message, "b.bin") {
			t.Errorf("warnings %+v", res.Warnings)
		}
	})
	t.Run("last track trimmed to its lead-out", func(t *testing.T) {
		dir := t.TempDir()
		testimg.Bin(t, dir, "disc.img", testimg.Track{Mode: "MODE1/2352", Frames: 2000, Shape: testimg.Mixed})
		cue := testimg.WriteCue(t, dir, "s.cue", `FILE "disc.img" BINARY
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
		res := assertCreateCDMatches(t, cue, CreateOptions{}, "", 0)
		if len(res.Warnings) != 1 || res.Warnings[0].Kind != WarnFramesDropped || res.Warnings[0].Track != 2 ||
			!strings.Contains(res.Warnings[0].Message, "1325 frames") {
			t.Errorf("warnings %+v", res.Warnings)
		}
	})
}

func TestCreateCDZeroByteBin(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disc.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cue := testimg.WriteCue(t, dir, "d.cue", "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
	if err := oracle.Run(t, "createcd", "-f", "-i", cue, "-o", filepath.Join(t.TempDir(), "w.chd")); err == nil {
		t.Fatal("chdman accepts an empty bin")
	}
	out := filepath.Join(dir, "out.chd")
	_, err := CreateCD(context.Background(), cue, out, CreateOptions{})
	if !errors.Is(err, chderr.InvalidCue) {
		t.Fatalf("error %v, want InvalidCue", err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"d.cue", "disc.bin"}) {
		t.Errorf("directory holds %v", got)
	}
}

func TestCreateCDShortBin(t *testing.T) {
	dir := t.TempDir()
	testimg.Bin(t, dir, "a.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed})
	testimg.Bin(t, dir, "b.bin", testimg.Track{Mode: "AUDIO", Frames: 20, Shape: testimg.Mixed})
	// Track 1 is 30 frames by the cue, but a.bin holds 20. chdman fails only
	// when it reads the missing frames; we fail before writing anything.
	cue := testimg.WriteCue(t, dir, "short.cue", `FILE "a.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    INDEX 01 00:00:30
FILE "b.bin" BINARY
  TRACK 03 AUDIO
    INDEX 01 00:00:00
`)
	out := filepath.Join(dir, "out.chd")
	_, err := CreateCD(context.Background(), cue, out, CreateOptions{})
	if !errors.Is(err, chderr.InvalidCue) {
		t.Fatalf("error %v, want InvalidCue", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("output exists: %v", err)
	}
}

func TestCreateCDRefusesToOverwriteAnInput(t *testing.T) {
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Disc{Split: true, Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed}, {Mode: "AUDIO", Frames: 20, Shape: testimg.Mixed}}})
	for _, in := range []string{cue, filepath.Join(dir, "track02.bin")} {
		before, err := os.ReadFile(in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CreateCD(context.Background(), cue, in, CreateOptions{Force: true}); err == nil {
			t.Errorf("expected an error writing over %s", in)
		}
		if after, _ := os.ReadFile(in); !bytes.Equal(before, after) {
			t.Errorf("%s changed", in)
		}
	}
}

func TestCreateCDCancelClosesBins(t *testing.T) {
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Disc{Split: true, Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 4000, Shape: testimg.Mixed}, {Mode: "AUDIO", Frames: 4000, Shape: testimg.Mixed}}})
	openFDs := func() int {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("no /proc/self/fd")
		}
		return len(ents)
	}
	before := openFDs()
	ctx, cancel := context.WithCancel(context.Background())
	out := filepath.Join(dir, "out.chd")
	_, err := CreateCD(ctx, cue, out, CreateOptions{Workers: 2, Progress: func(done, total uint64) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("output exists: %v", err)
	}
	if after := openFDs(); after != before {
		t.Errorf("%d file descriptors open, %d before", after, before)
	}
}

func TestCreateCDBinsStayBounded(t *testing.T) {
	// One bin per track, 99 tracks: at most one is open at any time.
	dir := t.TempDir()
	var tracks []testimg.Track
	for range 99 {
		tracks = append(tracks, testimg.Track{Mode: "AUDIO", Frames: 3, Shape: testimg.Random})
	}
	cue := testimg.Cue(t, dir, testimg.Disc{Split: true, Tracks: tracks})
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	base, peak := len(ents), 0
	out := filepath.Join(dir, "o.chd")
	_, err = CreateCD(context.Background(), cue, out, CreateOptions{Workers: 1, HunkBytes: 2448, Progress: func(done, total uint64) {
		if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
			peak = max(peak, len(ents))
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The temporary output, one bin and the directory listing itself.
	if peak-base > 4 {
		t.Errorf("up to %d descriptors open above the %d before", peak-base, base)
	}
	if got := chdInfo(t, out).HunkCount; got != 99*4/1 {
		t.Errorf("%d hunks", got)
	}
}

// negativePregapCue has INDEX 00 in one FILE and INDEX 01 at zero in the
// next, so chdman stores a pregap of -40 frames. The field is an unsigned
// frame count printed with %d, so it reads back as a negative number.
func negativePregapCue(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testimg.Bin(t, dir, "a.bin", testimg.Track{Mode: "MODE1/2352", Frames: 60, Shape: testimg.Mixed})
	testimg.Bin(t, dir, "b.bin", testimg.Track{Mode: "AUDIO", Frames: 100, Shape: testimg.Mixed})
	return testimg.WriteCue(t, dir, "d.cue", `FILE "a.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "b.bin" BINARY
  TRACK 02 AUDIO
    INDEX 00 00:00:40
    INDEX 01 00:00:00
`)
}

func TestNegativePregapMatchesChdman(t *testing.T) {
	oracle.Require(t)
	cue := negativePregapCue(t)
	ctx := context.Background()

	// Creating: both hashes, and the metadata text that the sha1 covers.
	res := assertCreateCDMatches(t, cue, CreateOptions{}, "", 0)
	ours := filepath.Join(t.TempDir(), "ours.chd")
	if _, err := CreateCD(ctx, cue, ours, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	theirs := filepath.Join(t.TempDir(), "theirs.chd")
	oracle.CreateCD(t, cue, theirs, "", 0)
	for _, chd := range []string{ours, theirs} {
		r, err := Open(chd)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, m := range r.Info().Metadata {
			found = found || strings.Contains(string(m.Data), "PREGAP:-40 ")
		}
		r.Close()
		if !found {
			t.Errorf("%s: no PREGAP:-40 in the metadata", filepath.Base(chd))
		}
	}
	if res.SHA1 == "" {
		t.Fatal("no sha1")
	}

	// Extracting: both CHDs must extract, to what chdman extracts.
	for _, chd := range []string{ours, theirs} {
		gotDir, wantDir := t.TempDir(), t.TempDir()
		if _, err := ExtractCD(ctx, chd, filepath.Join(gotDir, "out.cue"), ExtractCDOptions{}); err != nil {
			t.Fatalf("%s: %v", filepath.Base(chd), err)
		}
		oracle.ExtractCD(t, chd, filepath.Join(wantDir, "out.cue"), false)
		assertSameOutput(t, gotDir, wantDir)
	}

	// Reading: the sectors of both CHDs agree with each other.
	ro, err := Open(ours)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rt, err := Open(theirs)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	a, err := ro.Tracks()
	if err != nil {
		t.Fatal(err)
	}
	b, err := rt.Tracks()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a, b) {
		t.Errorf("tracks %+v, chdman's %+v", a, b)
	}
	// The negative pregap must read as negative, not as a huge unsigned count.
	if len(a) != 2 || a[1].Pregap != -40 {
		t.Errorf("track 2 pregap = %+v, want -40", a)
	}
}

// A bin of 4 GiB or more makes chdman compute the offset of the track after
// it in 32 bits. The hashes must follow it into the wrong bytes.
func TestCreateCDPreviousTrackSizeWraps(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 5 GiB")
	}
	if raceEnabled {
		t.Skip("takes minutes under the race detector, and the plain pass runs it")
	}
	oracle.Require(t)
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(5 << 30); err != nil {
		t.Skipf("cannot make a sparse file: %v", err)
	}
	f.Close()
	cue := testimg.WriteCue(t, dir, "d.cue", `FILE "a.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    INDEX 01 444:26:50
`)
	res := assertCreateCDMatches(t, cue, CreateOptions{Codecs: []string{"cdzl"}}, "cdzl", 0)
	var warned bool
	for _, w := range res.Warnings {
		warned = warned || (w.Kind == WarnOffsetTruncated && w.Track == 2)
	}
	if !warned {
		t.Errorf("no %s warning: %+v", WarnOffsetTruncated, res.Warnings)
	}
}

// A lead-out that does not fit the tracks around it makes chdman's unsigned
// arithmetic wrap. It converts the cue anyway, and the hashes depend on how.
func TestCreateCDMultisessionGapsThatDoNotFit(t *testing.T) {
	oracle.Require(t)
	for _, tc := range []struct {
		name            string
		frames          int
		leadOut, track2 string
	}{
		{"gap-larger-than-first-session", 2000, "00:01:00", "00:10:00"},
		{"gap-larger-than-next-track", 1200, "00:05:00", "00:15:00"},
		{"lead-out-past-next-track", 2000, "00:10:00", "00:06:00"},
	} {
		for _, ext := range []string{"bin", "img"} {
			t.Run(tc.name+"/"+ext, func(t *testing.T) {
				dir := t.TempDir()
				testimg.Bin(t, dir, "disc."+ext, testimg.Track{Mode: "MODE1/2352", Frames: tc.frames, Shape: testimg.Mixed})
				cue := testimg.WriteCue(t, dir, "s.cue", fmt.Sprintf(`FILE "disc.%s" BINARY
REM SESSION 01
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  REM LEAD-OUT %s
REM SESSION 02
  TRACK 02 MODE1/2352
    INDEX 00 %[3]s
    INDEX 01 %[3]s
`, ext, tc.leadOut, tc.track2))
				res := assertCreateCDMatches(t, cue, CreateOptions{}, "", 0)
				overwritten := slices.ContainsFunc(res.Warnings, func(w Warning) bool { return w.Kind == WarnFramesOverwritten })
				if overwritten != (ext == "bin") {
					t.Errorf("frames-overwritten warning = %v for a .%s bin: %+v", overwritten, ext, res.Warnings)
				}
			})
		}
	}
}

// Every file that the cue names is an input, even one that no track reads:
// chdman-go does not replace it.
func TestCreateCDRefusesToOverwriteAnUnreadBin(t *testing.T) {
	dir := t.TempDir()
	testimg.Bin(t, dir, "a.bin", testimg.Track{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed})
	b := testimg.Bin(t, dir, "b.bin", testimg.Track{Mode: "AUDIO", Frames: 20, Shape: testimg.Mixed})
	cue := testimg.WriteCue(t, dir, "d.cue", "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\nFILE \"b.bin\" BINARY\n")
	orig, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateCD(context.Background(), cue, b, CreateOptions{Force: true}); err == nil {
		t.Fatal("replaced a file that the cue names")
	}
	if got, _ := os.ReadFile(b); !bytes.Equal(got, orig) {
		t.Error("the bin was modified")
	}
	// A missing file that is named is not an input and not an error.
	missing := testimg.WriteCue(t, dir, "m.cue", "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\nFILE \"gone.bin\" BINARY\n")
	if _, err := CreateCD(context.Background(), missing, filepath.Join(dir, "m.chd"), CreateOptions{}); err != nil {
		t.Errorf("missing unread bin: %v", err)
	}
}
