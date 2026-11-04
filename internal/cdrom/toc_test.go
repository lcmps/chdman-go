package cdrom

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
)

func cht2(s string) format.MetadataEntry {
	return format.MetadataEntry{Tag: format.TagCHT2, Flags: format.FlagChecksum, Data: []byte(s + "\x00")}
}

func TestMetadataFormatExact(t *testing.T) {
	toc := TOC{Tracks: []Track{{
		Type: MODE2_RAW, Sub: None, DataSize: 2352, Frames: 300,
		Pregap: 150, PregapType: MODE2_RAW, PregapSub: None, PregapDataSize: 2352,
	}}}
	got := Metadata(toc)
	const want = "TRACK:1 TYPE:MODE2_RAW SUBTYPE:NONE FRAMES:300 PREGAP:150 PGTYPE:VMODE2_RAW PGSUB:NONE POSTGAP:0\x00"
	if len(got) != 1 || got[0].Tag != format.TagCHT2 || got[0].Flags != format.FlagChecksum || string(got[0].Data) != want {
		t.Fatalf("Metadata = %+v, want one CHT2 entry %q", got, want)
	}
}

func TestMetadataWithoutPregapDataHasNoVPrefix(t *testing.T) {
	toc := TOC{Tracks: []Track{{Type: AUDIO, Sub: None, DataSize: 2352, Frames: 10, PregapType: MODE1, PregapSub: None}}}
	const want = "TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:10 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0\x00"
	if got := Metadata(toc); string(got[0].Data) != want {
		t.Fatalf("got %q, want %q", got[0].Data, want)
	}
}

func TestExtraFrames(t *testing.T) {
	for frames, want := range map[string]uint32{"301": 3, "300": 0, "1": 3, "302": 2} {
		toc, err := ParseMetadata([]format.MetadataEntry{
			cht2("TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:" + frames + " PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := toc.Tracks[0].ExtraFrames; got != want {
			t.Errorf("Frames=%s: ExtraFrames = %d, want %d", frames, got, want)
		}
	}
}

func TestParseMetadataIgnoresCHSE(t *testing.T) {
	tracks := []format.MetadataEntry{
		cht2("TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:40 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
		cht2("TRACK:2 TYPE:AUDIO SUBTYPE:NONE FRAMES:30 PREGAP:150 PGTYPE:VAUDIO PGSUB:NONE POSTGAP:0"),
	}
	chse := format.MetadataEntry{Tag: format.TagCHSE, Flags: format.FlagChecksum, Data: []byte("SESSION:1\x00")}
	want, err := ParseMetadata(tracks)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMetadata([]format.MetadataEntry{chse, tracks[0], chse, tracks[1]})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with CHSE = %+v, without = %+v", got, want)
	}
}

func TestParseMetadataFrameOffsets(t *testing.T) {
	toc, err := ParseMetadata([]format.MetadataEntry{
		cht2("TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:41 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
		cht2("TRACK:2 TYPE:AUDIO SUBTYPE:NONE FRAMES:30 PREGAP:150 PGTYPE:VAUDIO PGSUB:NONE POSTGAP:5"),
		cht2("TRACK:3 TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:2 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if toc.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", toc.Sessions)
	}
	type ofs struct{ chd, log, phys uint32 }
	want := []ofs{
		{0, 0, 0},
		{44, 41 + 150, 41},             // track 1 padded by 3; V pregap is in the data
		{44 + 32, 41 + 30 + 5 + 2, 71}, // no pregap data: pregap added to logofs; postgap of track 2 counts
	}
	for i, w := range want {
		tr := toc.Tracks[i]
		if got := (ofs{tr.ChdFrameOfs, tr.LogFrameOfs, tr.PhysFrameOfs}); got != w {
			t.Errorf("track %d offsets = %+v, want %+v", i+1, got, w)
		}
	}
}

func TestParseMetadataRejects(t *testing.T) {
	gd := format.MetadataEntry{Tag: format.TagCHGD, Data: []byte("TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:1 PAD:0 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0\x00")}
	if _, err := ParseMetadata([]format.MetadataEntry{gd}); !errors.Is(err, chderr.UnsupportedDiscType) {
		t.Errorf("CHGD: err = %v, want UnsupportedDiscType", err)
	}
	gt := gd
	gt.Tag = format.TagCHGT
	if _, err := ParseMetadata([]format.MetadataEntry{gt}); !errors.Is(err, chderr.UnsupportedDiscType) {
		t.Errorf("CHGT: err = %v, want UnsupportedDiscType", err)
	}
	for _, bad := range []string{
		"garbage",
		"TRACK:0 TYPE:AUDIO SUBTYPE:NONE FRAMES:1 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0",
		"TRACK:2 TYPE:AUDIO SUBTYPE:NONE FRAMES:1 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0",
		"TRACK:1 TYPE:CDG SUBTYPE:NONE FRAMES:1 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0",
	} {
		if _, err := ParseMetadata([]format.MetadataEntry{cht2(bad)}); !errors.Is(err, chderr.CorruptData) {
			t.Errorf("%q: err = %v, want CorruptData", bad, err)
		}
	}
	// Each number from 1 to the count must appear exactly once. A repeat
	// leaves another number missing, and would give a track no data size.
	for name, entries := range map[string][]format.MetadataEntry{
		"duplicate": {
			cht2("TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
			cht2("TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
		},
		"missing": {
			cht2("TRACK:2 TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
			cht2("TRACK:2 TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
			cht2("TRACK:3 TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
		},
	} {
		if _, err := ParseMetadata(entries); !errors.Is(err, chderr.CorruptData) {
			t.Errorf("%s track numbers: err = %v, want CorruptData", name, err)
		}
	}
	if _, err := ParseMetadata(nil); err == nil {
		t.Error("no tracks: want an error")
	}
}

func TestParseMetadataCHTR(t *testing.T) {
	toc, err := ParseMetadata([]format.MetadataEntry{
		{Tag: format.TagCHTR, Data: []byte("TRACK:1 TYPE:MODE2_FORM1 SUBTYPE:RW FRAMES:5\x00")},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := toc.Tracks[0]
	if tr.Type != MODE2_FORM1 || tr.DataSize != 2048 || tr.Sub != Normal || tr.SubSize != 96 || tr.Frames != 5 || tr.ExtraFrames != 3 {
		t.Errorf("track = %+v", tr)
	}
}

func TestTypeFromCue(t *testing.T) {
	tests := []struct {
		in        string
		typ       TrackType
		data, sub uint32
		ok        bool
	}{
		{"MODE1/2048", MODE1, 2048, 0, true},
		{"MODE1/2352", MODE1_RAW, 2352, 0, true},
		{"MODE2/2336", MODE2, 2336, 0, true},
		{"MODE2/2352", MODE2_RAW, 2352, 0, true},
		{"AUDIO", AUDIO, 2352, 0, true},
		{"CDG", 0, 0, 0, false},
	}
	for _, tc := range tests {
		typ, data, sub, ok := TypeFromCue(tc.in)
		if typ != tc.typ || data != tc.data || sub != tc.sub || ok != tc.ok {
			t.Errorf("TypeFromCue(%q) = (%v,%d,%d,%v), want (%v,%d,%d,%v)", tc.in, typ, data, sub, ok, tc.typ, tc.data, tc.sub, tc.ok)
		}
	}
}

// A pregap that chdman computed as INDEX 01 minus a later INDEX 00 is a
// negative number stored in an unsigned field, and is written with %d.
func TestNegativePregap(t *testing.T) {
	entries := []format.MetadataEntry{
		cht2("TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:60 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"),
		cht2("TRACK:2 TYPE:AUDIO SUBTYPE:NONE FRAMES:100 PREGAP:-40 PGTYPE:VAUDIO PGSUB:NONE POSTGAP:0"),
	}
	toc, err := ParseMetadata(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got := toc.Tracks[1].Pregap; got != 1<<32-40 {
		t.Errorf("Pregap = %d, want %d", got, uint32(1<<32-40))
	}
	for i, e := range Metadata(toc) {
		if !bytes.Equal(e.Data, entries[i].Data) {
			t.Errorf("entry %d = %q, want %q", i+1, e.Data, entries[i].Data)
		}
	}
	// The pregap is in the data, so INDEX 01 of track 2 is where track 1
	// ends on the logical line, at 60.
	for _, tc := range []struct {
		lba   uint32
		track int
		frame uint64
	}{{59, 0, 59}, {60, 1, 60}, {61, 1, 61}} {
		loc, err := toc.Locate(tc.lba, false)
		if err != nil || loc.Track != tc.track || loc.Zero || loc.Frame != tc.frame {
			t.Errorf("Locate(%d) = %+v, %v; want track %d frame %d", tc.lba, loc, err, tc.track, tc.frame)
		}
	}
}
