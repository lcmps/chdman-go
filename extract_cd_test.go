package chdman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/chdwriter"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// cdLayouts is every shared layout plus a disc of eleven tracks, which makes
// chdman number split bins with two digits.
func cdLayouts() []testimg.Layout {
	var tracks []testimg.Track
	for range 11 {
		tracks = append(tracks, testimg.Track{Mode: "AUDIO", Frames: 12, Shape: testimg.Mixed})
	}
	return append(testimg.Layouts(),
		testimg.Layout{Name: "eleven-split", Disc: testimg.Disc{Tracks: tracks, Split: true}},
		testimg.Layout{Name: "eleven-single", Disc: testimg.Disc{Tracks: tracks}},
	)
}

// makeCDChd builds the layout's disc and has chdman-go compress it. It returns
// the directory holding the original bins and the CHD.
func makeCDChd(t *testing.T, d testimg.Disc, codecs ...string) (srcDir, chd string) {
	t.Helper()
	srcDir = t.TempDir()
	cue := testimg.Cue(t, srcDir, d)
	chd = filepath.Join(t.TempDir(), "disc.chd")
	if _, err := CreateCD(context.Background(), cue, chd, CreateOptions{Codecs: codecs}); err != nil {
		t.Fatal(err)
	}
	return srcDir, chd
}

func readDirFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range dirNames(t, dir) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = b
	}
	return files
}

// assertSameOutput compares what chdman-go and chdman wrote for one CHD, by
// file name and by byte.
func assertSameOutput(t *testing.T, got, want string) {
	t.Helper()
	g, w := readDirFiles(t, got), readDirFiles(t, want)
	for name, wb := range w {
		gb, ok := g[name]
		switch {
		case !ok:
			t.Errorf("missing output %s; have %v", name, dirNames(t, got))
		case !bytes.Equal(gb, wb):
			if strings.HasSuffix(name, ".cue") {
				t.Errorf("%s differs:\n got %q\nwant %q", name, gb, wb)
			} else {
				t.Errorf("%s differs (%d bytes, want %d)", name, len(gb), len(wb))
			}
		}
	}
	for name := range g {
		if _, ok := w[name]; !ok {
			t.Errorf("unexpected output %s", name)
		}
	}
}

func TestExtractCDCueMatchesChdman(t *testing.T) {
	oracle.Require(t)
	for _, l := range cdLayouts() {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split=%v", l.Name, split), func(t *testing.T) {
				_, chd := makeCDChd(t, l.Disc)
				ours, theirs := t.TempDir(), t.TempDir()
				res, err := ExtractCD(context.Background(), chd, filepath.Join(ours, "out.cue"), ExtractCDOptions{SplitBin: split})
				if err != nil {
					t.Fatal(err)
				}
				if len(res.Warnings) != 0 {
					t.Errorf("warnings %+v", res.Warnings)
				}
				oracle.ExtractCD(t, chd, filepath.Join(theirs, "out.cue"), split)
				assertSameOutput(t, ours, theirs)
			})
		}
	}
}

func TestExtractCDNamedBins(t *testing.T) {
	oracle.Require(t)
	disc := testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 30, Shape: testimg.Mixed},
	}}
	_, chd := makeCDChd(t, disc)
	tests := []struct {
		name  string
		bin   string // relative to the output directory
		split bool
	}{
		{"single with extension", "image.img", false},
		{"single without extension", "image", false},
		{"split with track variable", "t%t.img", true},
		{"split with padded variable", "x-%03t.dat", true},
		{"split twice", "%t-%t.bin", true},
		{"single ignores the variable", "t%t.bin", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ours, theirs := t.TempDir(), t.TempDir()
			opts := ExtractCDOptions{SplitBin: tt.split, OutputBin: filepath.Join(ours, tt.bin)}
			if _, err := ExtractCD(context.Background(), chd, filepath.Join(ours, "out.cue"), opts); err != nil {
				t.Fatal(err)
			}
			args := []string{"extractcd", "-f", "-i", chd, "-o", filepath.Join(theirs, "out.cue"), "-ob", filepath.Join(theirs, tt.bin)}
			if tt.split {
				args = append(args, "-sb")
			}
			if err := oracle.Run(t, args...); err != nil {
				t.Fatal(err)
			}
			assertSameOutput(t, ours, theirs)
		})
	}
}

func TestExtractCDReadsChdmanAllCodecs(t *testing.T) {
	oracle.Require(t)
	disc := testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 30, Shape: testimg.Mixed},
	}}
	for _, codecs := range []string{"cdzl", "cdzs", "cdlz", "cdfl", "cdlz,cdzl,cdfl", "none"} {
		t.Run(codecs, func(t *testing.T) {
			cue := testimg.Cue(t, t.TempDir(), disc)
			chd := filepath.Join(t.TempDir(), "disc.chd")
			oracle.CreateCD(t, cue, chd, codecs, 0)
			ours, theirs := t.TempDir(), t.TempDir()
			if _, err := ExtractCD(context.Background(), chd, filepath.Join(ours, "out.cue"), ExtractCDOptions{}); err != nil {
				t.Fatal(err)
			}
			oracle.ExtractCD(t, chd, filepath.Join(theirs, "out.cue"), false)
			assertSameOutput(t, ours, theirs)
		})
	}
}

func TestRoundTripCD(t *testing.T) {
	for _, l := range cdLayouts() {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split=%v", l.Name, split), func(t *testing.T) {
				_, chd := makeCDChd(t, l.Disc)
				want := chdInfo(t, chd)
				dir := t.TempDir()
				cue := filepath.Join(dir, "out.cue")
				if _, err := ExtractCD(context.Background(), chd, cue, ExtractCDOptions{SplitBin: split}); err != nil {
					t.Fatal(err)
				}
				again := filepath.Join(t.TempDir(), "again.chd")
				res, err := CreateCD(context.Background(), cue, again, CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if res.RawSHA1 != want.RawSHA1 || res.SHA1 != want.SHA1 {
					t.Errorf("hashes %s/%s, original %s/%s", res.RawSHA1, res.SHA1, want.RawSHA1, want.SHA1)
				}
				if len(res.Warnings) != 0 {
					t.Errorf("warnings %+v", res.Warnings)
				}
			})
		}
	}
}

func TestAudioSwappedBack(t *testing.T) {
	for _, l := range cdLayouts() {
		t.Run(l.Name, func(t *testing.T) {
			src, chd := makeCDChd(t, l.Disc)
			out := t.TempDir()
			if _, err := ExtractCD(context.Background(), chd, filepath.Join(out, "out.cue"),
				ExtractCDOptions{SplitBin: l.Disc.Split}); err != nil {
				t.Fatal(err)
			}
			if !l.Disc.Split {
				want, _ := os.ReadFile(filepath.Join(src, "disc.bin"))
				got, err := os.ReadFile(filepath.Join(out, "out.bin"))
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("out.bin differs from disc.bin (%d vs %d bytes, err %v)", len(got), len(want), err)
				}
				return
			}
			width := 1
			if len(l.Disc.Tracks) >= 10 {
				width = 2
			}
			for n := range l.Disc.Tracks {
				want, _ := os.ReadFile(filepath.Join(src, fmt.Sprintf("track%02d.bin", n+1)))
				got, err := os.ReadFile(filepath.Join(out, fmt.Sprintf("out (Track %0*d).bin", width, n+1)))
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("track %d differs (%d vs %d bytes, err %v)", n+1, len(got), len(want), err)
				}
			}
		})
	}
}

// subcodeCHD has chdman build a CHD with subcode from a .toc file, and returns
// it with the frames' sector halves and the bin's path.
func subcodeCHD(t *testing.T) (chd string, sectors []byte) {
	t.Helper()
	oracle.Require(t)
	const frames = 40
	dir := t.TempDir()
	var bin []byte
	for i := range frames {
		sector := bytes.Repeat([]byte{byte(i + 1)}, 2352)
		sub := bytes.Repeat([]byte{byte(0xA0 + i)}, 96)
		bin = append(bin, sector...)
		bin = append(bin, sub...)
		sectors = append(sectors, sector...)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	toc := testimg.WriteCue(t, dir, "disc.toc", "CD_ROM\n\n\n// Track 1\nTRACK MODE1_RAW RW_RAW\nNO COPY\n"+
		"DATAFILE \"sub.bin\" 00:00:40 // length in bytes: 97920\n\n\n")
	chd = filepath.Join(t.TempDir(), "sub.chd")
	oracle.CreateCD(t, toc, chd, "", 0)
	return chd, sectors
}

func TestExtractSubcodeWarning(t *testing.T) {
	chd, sectors := subcodeCHD(t)
	ours, theirs := t.TempDir(), t.TempDir()
	res, err := ExtractCD(context.Background(), chd, filepath.Join(ours, "out.cue"), ExtractCDOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != WarnSubcodeOmitted || res.Warnings[0].Track != 1 {
		t.Fatalf("warnings %+v", res.Warnings)
	}
	if got, _ := os.ReadFile(filepath.Join(ours, "out.bin")); !bytes.Equal(got, sectors) {
		t.Errorf("bin holds %d bytes, want the %d sector bytes without subcode", len(got), len(sectors))
	}
	oracle.ExtractCD(t, chd, filepath.Join(theirs, "out.cue"), false)
	assertSameOutput(t, ours, theirs)
}

func TestExtractCDRejects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, cdCHD := makeCDChd(t, testimg.Layouts()[1].Disc)

	iso := testimg.ISO(t, dir, 8, testimg.Mixed)
	dvd := filepath.Join(dir, "dvd.chd")
	if _, err := CreateDVD(ctx, iso, dvd, CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	gd := filepath.Join(dir, "gd.chd")
	f, err := os.Create(gd)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = chdwriter.Write(ctx, f, zeroSource{}, chdwriter.Options{
		HunkBytes: 19584, UnitBytes: 2448, LogicalBytes: 19584,
		Metadata: []format.MetadataEntry{{Tag: format.TagCHGT, Flags: format.FlagChecksum,
			Data: []byte("TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:8 PAD:0 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0\x00")}},
		Workers: 1,
	})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}

	for _, in := range []string{dvd, gd} {
		out := filepath.Join(t.TempDir(), "o.cue")
		if _, err := ExtractCD(ctx, in, out, ExtractCDOptions{}); !errors.Is(err, chderr.UnsupportedDiscType) {
			t.Errorf("%s: err = %v, want ErrUnsupportedDiscType", filepath.Base(in), err)
		}
	}

	outDir := t.TempDir()
	bad := []struct {
		name string
		out  string
		opts ExtractCDOptions
	}{
		{"not a cue", "o.toc", ExtractCDOptions{}},
		{"no extension", "o", ExtractCDOptions{}},
		{"quote in bin name", "o.cue", ExtractCDOptions{OutputBin: filepath.Join(outDir, `a"b.bin`)}},
		{"quote in bin extension", "o.cue", ExtractCDOptions{OutputBin: filepath.Join(outDir, `a.b"n`)}},
		{"split without track variable", "o.cue", ExtractCDOptions{SplitBin: true, OutputBin: filepath.Join(outDir, "x.bin")}},
		{"bin is the cue", "o.cue", ExtractCDOptions{OutputBin: filepath.Join(outDir, "o.cue")}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ExtractCD(ctx, cdCHD, filepath.Join(outDir, tt.out), tt.opts); err == nil {
				t.Error("expected an error")
			}
			if got := dirNames(t, outDir); len(got) != 0 {
				t.Errorf("left behind %v", got)
			}
		})
	}
	if _, err := ExtractCD(ctx, filepath.Join(dir, "missing.chd"), filepath.Join(outDir, "o.cue"), ExtractCDOptions{}); err == nil {
		t.Error("missing input: expected an error")
	}
}

type zeroSource struct{}

func (zeroSource) ReadHunk(_ uint32, dst []byte) error { clear(dst); return nil }

func TestExtractCDRefusesExistingAndInputs(t *testing.T) {
	ctx := context.Background()
	_, chd := makeCDChd(t, testimg.Layouts()[4].Disc)
	t.Run("existing bin without Force leaves no cue", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "out.bin")
		if err := os.WriteFile(bin, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ExtractCD(ctx, chd, filepath.Join(dir, "out.cue"), ExtractCDOptions{})
		if !errors.Is(err, os.ErrExist) {
			t.Errorf("err = %v", err)
		}
		if got := dirNames(t, dir); !slices.Equal(got, []string{"out.bin"}) {
			t.Errorf("directory holds %v", got)
		}
		if b, _ := os.ReadFile(bin); string(b) != "keep" {
			t.Error("existing bin was touched")
		}
	})
	t.Run("existing cue without Force", func(t *testing.T) {
		dir := t.TempDir()
		cue := filepath.Join(dir, "out.cue")
		os.WriteFile(cue, []byte("keep"), 0o644)
		if _, err := ExtractCD(ctx, chd, cue, ExtractCDOptions{}); !errors.Is(err, os.ErrExist) {
			t.Errorf("err = %v", err)
		}
		if got := dirNames(t, dir); !slices.Equal(got, []string{"out.cue"}) {
			t.Errorf("directory holds %v", got)
		}
	})
	t.Run("Force replaces all outputs", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"out.cue", "out (Track 1).bin", "out (Track 2).bin"} {
			os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o644)
		}
		opts := ExtractCDOptions{SplitBin: true, ExtractOptions: ExtractOptions{Force: true}}
		if _, err := ExtractCD(ctx, chd, filepath.Join(dir, "out.cue"), opts); err != nil {
			t.Fatal(err)
		}
		for n, b := range readDirFiles(t, dir) {
			if bytes.Equal(b, []byte("old")) {
				t.Errorf("%s was not replaced", n)
			}
		}
		if len(dirNames(t, dir)) != 3 {
			t.Errorf("directory holds %v", dirNames(t, dir))
		}
	})
	t.Run("output is the input", func(t *testing.T) {
		dir := t.TempDir()
		in := filepath.Join(dir, "disc.cue")
		b, _ := os.ReadFile(chd)
		os.WriteFile(in, b, 0o644)
		opts := ExtractCDOptions{ExtractOptions: ExtractOptions{Force: true}}
		if _, err := ExtractCD(ctx, in, in, opts); err == nil {
			t.Error("expected an error")
		}
		opts.OutputBin = in
		if _, err := ExtractCD(ctx, in, filepath.Join(dir, "x.cue"), opts); err == nil {
			t.Error("bin equal to the input: expected an error")
		}
		if got, _ := os.ReadFile(in); !bytes.Equal(got, b) {
			t.Error("input damaged")
		}
	})
}

func TestExtractCDCancelLeavesNothing(t *testing.T) {
	_, chd := makeCDChd(t, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 4000, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 100, Shape: testimg.Mixed},
	}, Split: true})
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := ExtractCDOptions{SplitBin: true, ExtractOptions: ExtractOptions{Progress: func(_, _ uint64) { cancel() }}}
	_, err := ExtractCD(ctx, chd, filepath.Join(dir, "out.cue"), opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := dirNames(t, dir); len(got) != 0 {
		t.Errorf("directory holds %v", got)
	}
}

func TestExtractCDWorkerCountDoesNotChangeOutput(t *testing.T) {
	_, chd := makeCDChd(t, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 3000, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 1001, Shape: testimg.Mixed},
	}})
	var first map[string][]byte
	for _, w := range []int{1, 3, 0} {
		dir := t.TempDir()
		var last uint64
		opts := ExtractCDOptions{ExtractOptions: ExtractOptions{Workers: w, Progress: func(done, total uint64) {
			if done < last || done > total {
				t.Errorf("progress %d/%d after %d", done, total, last)
			}
			last = done
		}}}
		if _, err := ExtractCD(context.Background(), chd, filepath.Join(dir, "out.cue"), opts); err != nil {
			t.Fatal(err)
		}
		if want := uint64(4001 * 2352); last != want {
			t.Errorf("final progress %d, want %d", last, want)
		}
		got := readDirFiles(t, dir)
		if first == nil {
			first = got
		} else if !sameFiles(first, got) {
			t.Errorf("workers=%d: output differs", w)
		}
	}
}

func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

func TestExtractCDUppercaseExtension(t *testing.T) {
	oracle.Require(t)
	_, chd := makeCDChd(t, testimg.Layouts()[4].Disc)
	ours, theirs := t.TempDir(), t.TempDir()
	if _, err := ExtractCD(context.Background(), chd, filepath.Join(ours, "A.CUE"), ExtractCDOptions{SplitBin: true}); err != nil {
		t.Fatal(err)
	}
	oracle.ExtractCD(t, chd, filepath.Join(theirs, "A.CUE"), true)
	assertSameOutput(t, ours, theirs)
}
