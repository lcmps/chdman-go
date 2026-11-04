package cdrom_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func TestMetadataMatchesChdman(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 30, Pregap: 150, Postgap: 5, Shape: testimg.Mixed},
	}})
	chd := filepath.Join(dir, "cd.chd")
	oracle.CreateCD(t, cue, chd, "", 0)

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
	toc, err := cdrom.ParseMetadata(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(toc.Tracks) != 2 {
		t.Fatalf("parsed %d tracks, want 2", len(toc.Tracks))
	}
	got := cdrom.Metadata(toc)
	if len(got) != len(entries) {
		t.Fatalf("Metadata gave %d entries, chdman wrote %d", len(got), len(entries))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], entries[i]) {
			t.Errorf("entry %d = %+v (%q), chdman wrote %+v (%q)", i, got[i], got[i].Data, entries[i], entries[i].Data)
		}
	}
}
