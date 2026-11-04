package chdman

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func TestExtractDVDRoundTrip(t *testing.T) {
	oracle.Require(t)
	for _, codecs := range []string{"", "zlib", "none"} {
		for _, workers := range []int{1, 0} {
			name := codecs
			if name == "" {
				name = "default"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				iso := testimg.ISO(t, dir, 300, testimg.Mixed)
				want, err := os.ReadFile(iso)
				if err != nil {
					t.Fatal(err)
				}
				var opts []string
				if codecs != "" {
					opts = []string{codecs}
				}

				theirs := filepath.Join(dir, "chdman.chd")
				oracle.CreateDVD(t, iso, theirs, codecs, 0)
				ours := filepath.Join(dir, "ours.chd")
				if _, err := CreateDVD(context.Background(), iso, ours, CreateOptions{Codecs: opts}); err != nil {
					t.Fatal(err)
				}
				for _, chd := range []string{theirs, ours} {
					out := filepath.Join(dir, "out.iso")
					var last uint64
					_, err := ExtractDVD(context.Background(), chd, out, ExtractOptions{
						Workers: workers, Force: true,
						Progress: func(done, total uint64) {
							if done < last || total != uint64(len(want)) {
								t.Errorf("progress %d/%d after %d", done, total, last)
							}
							last = done
						},
					})
					if err != nil {
						t.Fatalf("%s: %v", filepath.Base(chd), err)
					}
					if got, _ := os.ReadFile(out); !bytes.Equal(got, want) {
						t.Errorf("%s: extracted ISO differs", filepath.Base(chd))
					}
					if last != uint64(len(want)) {
						t.Errorf("%s: final progress %d", filepath.Base(chd), last)
					}
				}
			})
		}
	}
}

func TestExtractDVDHunkLargerThanData(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 3, testimg.Mixed) // one 4096-byte hunk, half used
	want, _ := os.ReadFile(iso)
	chd := filepath.Join(dir, "a.chd")
	if _, err := CreateDVD(context.Background(), iso, chd, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.iso")
	if _, err := ExtractDVD(context.Background(), chd, out, ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, want) {
		t.Error("extracted ISO differs")
	}
}

func TestExtractDVDRejectsCD(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{{Mode: "MODE1/2352", Frames: 20, Shape: testimg.Mixed}}})
	chd := filepath.Join(dir, "cd.chd")
	oracle.CreateCD(t, cue, chd, "", 0)
	out := filepath.Join(dir, "out.iso")
	_, err := ExtractDVD(context.Background(), chd, out, ExtractOptions{})
	if !errors.Is(err, ErrUnsupportedDiscType) {
		t.Fatalf("err = %v, want ErrUnsupportedDiscType", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output exists")
	}
}

func TestExtractDVDRefusesExistingAndInput(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 4, testimg.Mixed)
	chd := filepath.Join(dir, "a.chd")
	if _, err := CreateDVD(context.Background(), iso, chd, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractDVD(context.Background(), chd, iso, ExtractOptions{}); !errors.Is(err, os.ErrExist) {
		t.Errorf("existing output: err = %v", err)
	}
	if _, err := ExtractDVD(context.Background(), chd, chd, ExtractOptions{Force: true}); err == nil {
		t.Error("output equal to input: expected an error")
	}
	if _, err := Open(chd); err != nil {
		t.Errorf("input damaged: %v", err)
	}
}

func TestExtractDVDCancelLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 8192, testimg.Mixed)
	chd := filepath.Join(dir, "a.chd")
	if _, err := CreateDVD(context.Background(), iso, chd, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.iso")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := ExtractDVD(ctx, chd, out, ExtractOptions{Progress: func(_, _ uint64) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"a.chd", "image.iso"}) {
		t.Errorf("directory holds %v", got)
	}
}
