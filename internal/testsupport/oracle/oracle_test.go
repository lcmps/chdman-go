package oracle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func TestOracleVersion(t *testing.T) {
	oracle.Require(t) // fails or skips unless chdman reports 0.289
	if got := oracle.Version(t); got != "0.289" {
		t.Fatalf("chdman version = %q, want 0.289", got)
	}
}

func TestOracleAcceptsGeneratedImages(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()

	iso := testimg.ISO(t, dir, 16, testimg.Mixed)
	isoCHD := filepath.Join(dir, "iso.chd")
	oracle.CreateDVD(t, iso, isoCHD, "", 0)
	if err := oracle.Verify(t, isoCHD); err != nil {
		t.Errorf("verify DVD: %v", err)
	}
	isoOut := filepath.Join(dir, "out.iso")
	oracle.ExtractDVD(t, isoCHD, isoOut)
	assertSameFile(t, iso, isoOut)

	cue := testimg.Cue(t, dir, testimg.Disc{
		Tracks: []testimg.Track{
			{Mode: "MODE1/2352", Frames: 16, Shape: testimg.Mixed},
			{Mode: "AUDIO", Frames: 16, Pregap: 150, Shape: testimg.Mixed},
		},
		Split: true,
	})
	cdCHD := filepath.Join(dir, "cd.chd")
	oracle.CreateCD(t, cue, cdCHD, "", 0)
	if err := oracle.Verify(t, cdCHD); err != nil {
		t.Errorf("verify CD: %v", err)
	}
	outDir := t.TempDir()
	outCue := filepath.Join(outDir, "out.cue")
	oracle.ExtractCD(t, cdCHD, outCue, true)
	if _, err := os.Stat(filepath.Join(outDir, "out (Track 1).bin")); err != nil {
		t.Errorf("split extraction: %v", err)
	}
	assertSameFile(t, filepath.Join(dir, "track01.bin"), filepath.Join(outDir, "out (Track 1).bin"))
}

func TestVerifyReportsCorruption(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 64, testimg.Random)
	chd := filepath.Join(dir, "a.chd")
	oracle.CreateDVD(t, iso, chd, "", 0) // uncompressed CHDs are not verified
	data, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(chd, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := oracle.Verify(t, chd); err == nil {
		t.Error("verify of a corrupted CHD returned nil")
	}
}

func assertSameFile(t *testing.T, a, b string) {
	t.Helper()
	da, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(da) != string(db) {
		t.Errorf("%s and %s differ", a, b)
	}
}
