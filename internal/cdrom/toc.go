package cdrom

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
)

// Frame geometry, from cdrom.h.
const (
	// FrameSize is the size of a stored CD frame: sector plus subcode.
	FrameSize = SectorSize + SubcodeSize
	// SubcodeSize is the size of the subcode data of a frame.
	SubcodeSize = 96
	// TrackPadding is the multiple of frames each track is padded to in a CHD.
	TrackPadding = 4
	// FramesPerHunk is the number of frames in a default CD hunk.
	FramesPerHunk = 8

	maxTracks = 99
)

// TrackType is the sector format of a track, numbered as in cdrom.h.
type TrackType uint32

// Track types.
const (
	MODE1 TrackType = iota
	MODE1_RAW
	MODE2
	MODE2_FORM1
	MODE2_FORM2
	MODE2_FORM_MIX
	MODE2_RAW
	AUDIO
)

// SubType is the subcode format of a track.
type SubType uint32

// Subcode formats.
const (
	None SubType = iota
	Normal
	Raw
)

// Track is one track of a CD table of contents. The offsets are in frames.
type Track struct {
	Type                                   TrackType
	Sub                                    SubType
	DataSize, SubSize                      uint32
	Frames, ExtraFrames, PadFrames         uint32
	Pregap, Postgap                        uint32
	PregapType                             TrackType
	PregapSub                              SubType
	PregapDataSize                         uint32
	Session                                uint32
	ChdFrameOfs, LogFrameOfs, PhysFrameOfs uint32
}

// TOC is the table of contents of a CD.
type TOC struct {
	Tracks   []Track
	Sessions int
	Flags    uint32
}

var typeNames = [...]string{"MODE1", "MODE1_RAW", "MODE2", "MODE2_FORM1", "MODE2_FORM2", "MODE2_FORM_MIX", "MODE2_RAW", "AUDIO"}

func (t TrackType) String() string {
	if int(t) < len(typeNames) {
		return typeNames[t]
	}
	return "UNKNOWN"
}

func (s SubType) String() string {
	switch s {
	case Normal:
		return "RW"
	case Raw:
		return "RW_RAW"
	}
	return "NONE"
}

// typeInfo is one row of get_info_from_type_string.
type typeInfo struct {
	typ      TrackType
	dataSize uint32
}

var typeByString = map[string]typeInfo{
	"MODE1": {MODE1, 2048}, "MODE1/2048": {MODE1, 2048},
	"MODE1_RAW": {MODE1_RAW, 2352}, "MODE1/2352": {MODE1_RAW, 2352},
	"MODE2": {MODE2, 2336}, "MODE2/2336": {MODE2, 2336},
	"MODE2_FORM1": {MODE2_FORM1, 2048}, "MODE2/2048": {MODE2_FORM1, 2048},
	"MODE2_FORM2": {MODE2_FORM2, 2324}, "MODE2/2324": {MODE2_FORM2, 2324},
	"MODE2_FORM_MIX": {MODE2_FORM_MIX, 2336},
	"MODE2_RAW":      {MODE2_RAW, 2352}, "MODE2/2352": {MODE2_RAW, 2352}, "CDI/2352": {MODE2_RAW, 2352},
	"AUDIO": {AUDIO, 2352},
}

// TypeFromCue converts a cue or chdman type string to a track type, its data
// size and its subcode size. ok is false for an unknown type such as CDG.
func TypeFromCue(s string) (typ TrackType, dataSize, subSize uint32, ok bool) {
	info, ok := typeByString[s]
	if !ok {
		return MODE1, 0, 0, false
	}
	return info.typ, info.dataSize, 0, true
}

func subFromString(s string) (SubType, uint32) {
	switch s {
	case "RW":
		return Normal, SubcodeSize
	case "RW_RAW":
		return Raw, SubcodeSize
	}
	return None, 0
}

// Metadata returns the CHT2 entries chdman writes for toc, one per track. It
// writes no CHSE entries.
func Metadata(toc TOC) []format.MetadataEntry {
	entries := make([]format.MetadataEntry, len(toc.Tracks))
	for i, t := range toc.Tracks {
		pgType := t.PregapType.String()
		if t.PregapDataSize > 0 {
			pgType = "V" + pgType
		}
		s := fmt.Sprintf("TRACK:%d TYPE:%s SUBTYPE:%s FRAMES:%d PREGAP:%d PGTYPE:%s PGSUB:%s POSTGAP:%d\x00",
			i+1, t.Type, t.Sub, t.Frames, t.Pregap, pgType, t.PregapSub, t.Postgap)
		entries[i] = format.MetadataEntry{Tag: format.TagCHT2, Flags: format.FlagChecksum, Data: []byte(s)}
	}
	return entries
}

// ParseMetadata builds a TOC from CHT2 or CHTR entries, ignoring CHSE and
// anything else. GD-ROM entries yield chderr.UnsupportedDiscType.
func ParseMetadata(entries []format.MetadataEntry) (TOC, error) {
	toc := TOC{Sessions: 1}
	for _, e := range entries {
		if e.Tag == format.TagCHGD || e.Tag == format.TagCHGT {
			return TOC{}, fmt.Errorf("%w: GD-ROM track metadata", chderr.UnsupportedDiscType)
		}
	}
	var parsed []parsedTrack
	for _, e := range entries {
		if e.Tag != format.TagCHT2 && e.Tag != format.TagCHTR {
			continue
		}
		p, err := parseTrack(e)
		if err != nil {
			return TOC{}, err
		}
		parsed = append(parsed, p)
	}
	if len(parsed) == 0 {
		return TOC{}, fmt.Errorf("%w: no CD track metadata", chderr.CorruptData)
	}
	toc.Tracks = make([]Track, len(parsed))
	for _, p := range parsed {
		if p.num == 0 || p.num > len(parsed) {
			return TOC{}, fmt.Errorf("%w: track number %d out of range", chderr.CorruptData, p.num)
		}
		toc.Tracks[p.num-1] = p.track
	}
	setOffsets(toc.Tracks)
	return toc, nil
}

type parsedTrack struct {
	num   int
	track Track
}

// parseTrack follows the sscanf of cdrom_file::parse_metadata: every field is
// a "KEY:value" token, in a fixed order.
func parseTrack(e format.MetadataEntry) (parsedTrack, error) {
	bad := func(why string) (parsedTrack, error) {
		return parsedTrack{}, fmt.Errorf("%w: CD track metadata %q: %s", chderr.CorruptData, e.Data, why)
	}
	keys := []string{"TRACK", "TYPE", "SUBTYPE", "FRAMES"}
	if e.Tag == format.TagCHT2 {
		keys = append(keys, "PREGAP", "PGTYPE", "PGSUB", "POSTGAP")
	}
	fields := strings.Fields(string(bytes.TrimRight(e.Data, "\x00")))
	if len(fields) != len(keys) {
		return bad("wrong field count")
	}
	v := make(map[string]string, len(keys))
	for i, f := range fields {
		k, val, ok := strings.Cut(f, ":")
		if !ok || k != keys[i] {
			return bad("unexpected field " + strconv.Quote(f))
		}
		v[k] = val
	}
	num := func(k string) (uint32, bool) {
		n, err := strconv.ParseUint(v[k], 10, 31)
		return uint32(n), err == nil
	}
	trackNum, ok1 := num("TRACK")
	frames, ok2 := num("FRAMES")
	if !ok1 || !ok2 {
		return bad("bad number")
	}
	t := Track{Frames: frames}
	info, ok := typeByString[v["TYPE"]]
	if !ok {
		return bad("unknown type")
	}
	t.Type, t.DataSize = info.typ, info.dataSize
	t.Sub, t.SubSize = subFromString(v["SUBTYPE"])
	t.ExtraFrames = (frames+TrackPadding-1)/TrackPadding*TrackPadding - frames
	if e.Tag == format.TagCHT2 {
		pregap, ok1 := num("PREGAP")
		postgap, ok2 := num("POSTGAP")
		if !ok1 || !ok2 {
			return bad("bad number")
		}
		t.Pregap, t.Postgap = pregap, postgap
		if pregap > 0 {
			if pg, ok := strings.CutPrefix(v["PGTYPE"], "V"); ok {
				if info, ok := typeByString[pg]; ok {
					t.PregapType, t.PregapDataSize = info.typ, info.dataSize
				}
			}
			t.PregapSub, _ = subFromString(v["PGSUB"])
		}
	}
	return parsedTrack{num: int(trackNum), track: t}, nil
}

// setOffsets fills in the frame offsets, as the cdrom_file constructor does.
func setOffsets(tracks []Track) {
	var phys, chd, log uint32
	for i := range tracks {
		t := &tracks[i]
		if t.PregapDataSize == 0 {
			// The pregap is not in the data: it only shifts the logical
			// position of the track.
			log += t.Pregap
			t.LogFrameOfs = 0
		} else {
			t.LogFrameOfs = t.Pregap
		}
		t.PhysFrameOfs = phys
		t.ChdFrameOfs = chd
		t.LogFrameOfs += log
		log += t.Postgap
		phys += t.Frames
		chd += t.Frames + t.ExtraFrames
		log += t.Frames
	}
}
