package testimg_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func TestISOSizeAndDeterminism(t *testing.T) {
	for _, s := range []testimg.Shape{testimg.Zeros, testimg.Random, testimg.Repeated, testimg.Mixed} {
		a := readFile(t, testimg.ISO(t, t.TempDir(), 8, s))
		b := readFile(t, testimg.ISO(t, t.TempDir(), 8, s))
		if len(a) != 8*2048 {
			t.Errorf("shape %d: size = %d, want %d", s, len(a), 8*2048)
		}
		if string(a) != string(b) {
			t.Errorf("shape %d: output is not deterministic", s)
		}
	}
}

func TestCueDataSectorsAreValid(t *testing.T) {
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 8, Shape: testimg.Mixed},
		{Mode: "MODE2/2352", Frames: 4, Shape: testimg.Repeated},
		{Mode: "AUDIO", Frames: 4, Shape: testimg.Random},
	}})
	bin := readFile(t, filepath.Join(dir, "disc.bin"))
	if len(bin) != 16*cdrom.SectorSize {
		t.Fatalf("bin size = %d, want %d", len(bin), 16*cdrom.SectorSize)
	}
	if _, err := os.Stat(cue); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		sec := bin[i*cdrom.SectorSize : (i+1)*cdrom.SectorSize]
		if sec[0] != 0 || sec[11] != 0 || sec[1] != 0xff || sec[10] != 0xff {
			t.Errorf("sector %d: bad sync", i)
		}
		if !cdrom.ECCVerify(sec) {
			t.Errorf("sector %d: ECC invalid", i)
		}
	}
}

func TestCueSplitWritesOneBinPerTrack(t *testing.T) {
	dir := t.TempDir()
	testimg.Cue(t, dir, testimg.Disc{Split: true, Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 3, Shape: testimg.Zeros},
		{Mode: "AUDIO", Frames: 5, Shape: testimg.Zeros},
	}})
	for name, frames := range map[string]int{"track01.bin": 3, "track02.bin": 5} {
		if got := len(readFile(t, filepath.Join(dir, name))); got != frames*cdrom.SectorSize {
			t.Errorf("%s size = %d, want %d", name, got, frames*cdrom.SectorSize)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
