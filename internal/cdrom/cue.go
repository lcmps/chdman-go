package cdrom

// This file is a translation of cdrom_file::parse_cue, cdrom_file::is_gdicue
// and their helpers (tokenize, msf_to_frames, get_file_path, get_file_size)
// from MAME 0.289, src/lib/util/cdrom.cpp, BSD-3-Clause, copyright the MAME
// team. Its quirks are kept on purpose: the results are written into hashed
// CHD metadata, so they must match chdman's.

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/lcmps/chdman-go/internal/chderr"
)

// CD flags, from cdrom.h.
const (
	flagGDROM        = 0x1
	flagMultisession = 0x4
)

const (
	maxIndex = 99
	// maxLine is what chdman's fgets(linebuffer, 511, ...) reads at most: a
	// longer line is read as several lines.
	maxLine = 510
)

// Warning kinds returned by ParseCue.
const (
	// WarnTrailingBytes is a bin whose size is not a whole number of frames
	// for its track: chdman drops the partial frame at the end.
	WarnTrailingBytes = "trailing-bytes-dropped"
	// WarnOffsetTruncated is a track that follows a track of 4 GiB or more in
	// the same bin: chdman computes the raw size of that track in 32 bits, so
	// it starts reading the wrong bytes.
	WarnOffsetTruncated = "track-offset-truncated"
	// WarnFramesOverwritten is a single-bin multisession cue with REM
	// LEAD-OUT and a non-.img file: chdman writes zeros over real frames.
	WarnFramesOverwritten = "frames-overwritten"
)

// TrackSource says where the frames of one track come from. Frames of the
// track are read from Path, starting at byte Offset, as consecutive frames of
// the track's data size plus subcode size, and followed by Pad frames of
// zeros. Frames plus Pad is Track.Frames. When Swap is set the bytes of each
// 2352-byte sector are swapped in pairs.
type TrackSource struct {
	Path   string
	Offset uint64
	Swap   bool
	Frames uint32
	Pad    uint32
}

// Warning reports an input that chdman converts with silent data loss. The
// conversion is unchanged: a warning only describes it.
type Warning struct {
	Kind    string
	Track   int // 1-based track number
	Message string
}

// cueTrack is track_input_entry: what the cue says about one track.
type cueTrack struct {
	fname   string
	offset  uint64
	swap    bool
	idx     [maxIndex + 1]int
	leadin  int
	leadout int
}

// reset gives the track the state of a track that no command has touched.
func (c *cueTrack) reset() {
	c.leadin, c.leadout = -1, -1
	for i := range c.idx {
		c.idx[i] = -1
	}
}

// ParseCue reads a cue sheet the way chdman 0.289 does and returns the
// table of contents, where each track's data comes from, and warnings about
// data that chdman drops or overwrites. Errors wrap chderr.InvalidCue, except
// for the GD-ROM multi-cue layout, which wraps chderr.UnsupportedDiscType.
func ParseCue(path string) (TOC, []TrackSource, []Warning, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TOC{}, nil, nil, fmt.Errorf("%w: %w", chderr.InvalidCue, err)
	}
	lines := cueLines(data)
	if isGDICue(lines) {
		return TOC{}, nil, nil, fmt.Errorf("%w: %s is a GD-ROM multi-cue", chderr.UnsupportedDiscType, path)
	}
	p := cueParser{dir: filePath(path), trknum: -1, leadin: -1}
	for i := range p.info {
		p.info[i].reset()
	}
	p.beforeInfo.reset()
	for _, line := range lines {
		if err := p.line(line); err != nil {
			return TOC{}, nil, nil, err
		}
	}
	return p.finish()
}

// cueParser holds the state of parse_cue.
type cueParser struct {
	dir    string
	flags  uint32
	tracks [maxTracks]Track
	info   [maxTracks]cueTrack
	// before is the slot of "track -1", which chdman writes to when a
	// command comes before the first TRACK, or after a TRACK 00. Nothing
	// reads it.
	before     Track
	beforeInfo cueTrack

	trknum     int
	sessionnum int
	sessionPre int
	leadin     int
	lastfname  string
	isMultibin bool
	warnings   []Warning
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", chderr.InvalidCue, fmt.Sprintf(format, args...))
}

// current returns the track that a command applies to. chdman indexes its
// arrays with -1 when a command comes before any TRACK, which writes to
// memory that is never read; the commands have no effect then.
func (p *cueParser) current() (*Track, *cueTrack) {
	if p.trknum < 0 {
		return &p.before, &p.beforeInfo
	}
	return &p.tracks[p.trknum], &p.info[p.trknum]
}

// line interprets one line of the cue, the body of parse_cue's read loop.
func (p *cueParser) line(buf []byte) error {
	l := cueLine{buf: buf}
	switch token := l.token(); token {
	case "REM":
		return p.rem(&l)
	case "FILE":
		return p.file(&l)
	case "TRACK":
		return p.track(&l)
	case "INDEX":
		idx := int(int32(strtoul(l.token())))
		frames := msfToFrames(l.token())
		if idx < 0 || idx > maxIndex {
			return invalid("encountered invalid index %d", idx)
		}
		t, in := p.current()
		in.idx[idx] = frames
		if idx == 1 {
			if t.Pregap == 0 && in.idx[0] != -1 {
				t.Pregap = uint32(frames - in.idx[0])
				t.PregapType = t.Type
				t.PregapDataSize = t.DataSize
			} else if in.idx[0] == -1 {
				// The pregap sectors are not in the file, but idx 0 is
				// always used for the track length.
				in.idx[0] = frames
			}
		}
	case "PREGAP":
		t, _ := p.current()
		t.Pregap = uint32(msfToFrames(l.token()))
	case "POSTGAP":
		t, _ := p.current()
		t.Postgap = uint32(msfToFrames(l.token()))
	}
	// FLAGS only sets control flags, which are not part of the metadata.
	return nil
}

func (p *cueParser) rem(l *cueLine) error {
	l.skipSpace()
	rest := l.rest()
	switch {
	case bytes.HasPrefix(rest, []byte("SESSION")):
		// IsoBuster extension.
		l.token()
		p.sessionnum = int(int32(strtoul(l.token()) - 1))
		if p.sessionnum >= 1 {
			p.flags |= flagMultisession
		}
	case p.flags&flagMultisession != 0 && bytes.HasPrefix(rest, []byte("PREGAP")):
		// Redump extension: a pregap that belongs to the session.
		l.token()
		p.sessionPre = msfToFrames(l.token())
	case bytes.HasPrefix(rest, []byte("LEAD-OUT")):
		// IsoBuster and ImgBurn (single bin): the start of the lead-out.
		// Redump and DiscImageCreator (multiple bins): its duration.
		l.token()
		_, in := p.current()
		in.leadout = msfToFrames(l.token())
	case bytes.HasPrefix(rest, []byte("LEAD-IN")):
		l.token()
		p.leadin = msfToFrames(l.token())
	}
	return nil
}

func (p *cueParser) file(l *cueLine) error {
	name := p.dir + l.token()
	if !p.isMultibin {
		prev := p.lastfname
		p.lastfname = name
		p.isMultibin = prev != "" && name != prev
	} else {
		p.lastfname = name
	}
	// The file type applies to the next track, whatever that turns out to be.
	if p.trknum+1 >= maxTracks {
		return invalid("too many tracks")
	}
	switch kind := l.token(); kind {
	case "BINARY":
		p.info[p.trknum+1].swap = false
	case "MOTOROLA":
		p.info[p.trknum+1].swap = true
	case "WAVE":
		return invalid("WAVE files are not supported (%s)", p.lastfname)
	default:
		return invalid("unhandled file type %q", kind)
	}
	return nil
}

func (p *cueParser) track(l *cueLine) error {
	// TRACK 00, and anything that is not a number, is track -1.
	trknum := int(int32(strtoul(l.token()) - 1))
	if trknum < -1 || trknum >= maxTracks {
		return invalid("track number %d is out of range", trknum+1)
	}
	p.trknum = trknum
	typeName := l.token()
	t, in := p.current()
	t.Session = uint32(p.sessionnum)
	t.Sub, t.SubSize = None, 0
	t.PregapSub = None
	t.Pregap, t.PadFrames, t.DataSize = 0, 0, 0
	in.offset = 0
	for i := range in.idx {
		in.idx[i] = -1
	}
	in.leadout = -1
	in.leadin = p.leadin // the lead-in saved earlier
	p.leadin = -1
	if p.sessionPre != 0 {
		// The pregap of a session transition belongs to the lead-in.
		if in.leadin == -1 {
			in.leadin = p.sessionPre
		} else {
			in.leadin += p.sessionPre
		}
		p.sessionPre = 0
	}
	in.fname = p.lastfname
	typ, dataSize, _, ok := TypeFromCue(typeName)
	if !ok {
		return invalid("unknown track type %q (track %d)", typeName, trknum+1)
	}
	t.Type, t.DataSize = typ, dataSize
	t.Sub, t.SubSize = subFromString(l.token())
	return nil
}

// finish sets the track lengths and offsets, the second half of parse_cue.
func (p *cueParser) finish() (TOC, []TrackSource, []Warning, error) {
	numtrks := p.trknum + 1
	if numtrks == 0 {
		return TOC{}, nil, nil, invalid("no tracks")
	}
	tracks, info := p.tracks[:numtrks], p.info[:numtrks]
	bytesPerFrame := func(i int) uint64 { return uint64(tracks[i].DataSize + tracks[i].SubSize) }

	for n := range numtrks {
		t, in := &tracks[n], &info[n]
		if in.idx[1] == -1 {
			return TOC{}, nil, nil, invalid("track %d is missing its INDEX 01 marker", n+1)
		}
		// True for cue/bin and cue/iso.
		if t.Type == AUDIO {
			in.swap = true
		}
		// (chdman skips tracks with an offset already, which only WAVE
		// files have. They are rejected while parsing.)
		last := n+1 >= numtrks
		bps := bytesPerFrame(n)
		switch {
		case last && n > 0 && in.fname == info[n-1].fname:
			// The last track shares its bin with the one before it.
			tlen := fileSize(in.fname)
			if tlen == 0 {
				return TOC{}, nil, nil, missingBin(in.fname)
			}
			in.offset = info[n-1].offset + p.previousRawSize(n)
			if in.offset > tlen {
				return TOC{}, nil, nil, invalid("track %d starts past the end of %s", n+1, in.fname)
			}
			t.Frames = uint32((tlen - in.offset) / bps)
			p.trailing(n, (tlen-in.offset)%bps)
		case !last && in.fname == info[n+1].fname:
			// The next track is in the same bin.
			diff := info[n+1].idx[0] - in.idx[0]
			if diff <= 0 {
				return TOC{}, nil, nil, invalid("unable to determine the size of track %d, missing INDEX 01 markers?", n+1)
			}
			t.Frames = uint32(diff)
			if n > 0 {
				in.offset = info[n-1].offset + p.previousRawSize(n)
			}
		case t.Frames == 0:
			// The tracks are in different files.
			tlen := fileSize(in.fname)
			if tlen == 0 {
				return TOC{}, nil, nil, missingBin(in.fname)
			}
			t.Frames = uint32(tlen / bps)
			in.offset = 0
			p.trailing(n, tlen%bps)
		}
		if p.flags&flagMultisession == 0 {
			continue
		}
		if p.isMultibin {
			if in.leadout == -1 && !last && t.Session != tracks[n+1].Session {
				// A standard lead-out before the session changes. The first
				// session's (1m30s) is longer than the others' (30s).
				in.leadout = 2250
				if t.Session == 0 {
					in.leadout = 6750
				}
			}
			if in.leadin == -1 && n > 0 && t.Session != tracks[n-1].Session {
				in.leadin = 4500 // a standard lead-in of 1m
			}
			continue
		}
		if in.leadout != -1 {
			// A lead-out time in a multisession CD trims this track to the
			// lead-out instead of the idx 0 of the next track.
			end := in.leadout - in.idx[0]
			if t.Frames >= uint32(end) {
				t.Frames = uint32(end)
				if !last {
					// The lead-out becomes the gap between it and the next
					// track's pregap.
					in.leadout = info[n+1].idx[0] - in.leadout
				}
			}
		}
		if n > 0 && info[n-1].leadout != -1 {
			// ImgBurn bin/cue files hold dummy data between the lead-out
			// and the next track, DiscImageCreator img/cue files do not.
			// chdman can only tell them apart by the extension.
			if !strings.HasSuffix(strings.ToLower(info[n-1].fname), ".img") {
				gap := uint32(info[n-1].leadout)
				if gap > t.Frames || tracks[n-1].PadFrames+gap > tracks[n-1].Frames {
					return TOC{}, nil, nil, invalid("lead-out of track %d does not fit its tracks", n)
				}
				tracks[n-1].PadFrames += gap
				t.Frames -= gap
				in.offset += uint64(gap) * bps
				if gap > 0 {
					p.warnings = append(p.warnings, Warning{
						Kind:  WarnFramesOverwritten,
						Track: n,
						Message: fmt.Sprintf("track %d: chdman replaces the last %d frames with zeros and skips %d frames at the start of track %d (REM LEAD-OUT in a single non-.img file)",
							n, gap, gap, n+1),
					})
				}
			}
		}
	}

	toc := TOC{Tracks: tracks, Sessions: p.sessionnum + 1, Flags: p.flags}
	srcs := make([]TrackSource, numtrks)
	for i := range toc.Tracks {
		t := &toc.Tracks[i]
		t.ExtraFrames = (t.Frames+TrackPadding-1)/TrackPadding*TrackPadding - t.Frames
		srcs[i] = TrackSource{Path: info[i].fname, Offset: info[i].offset, Swap: info[i].swap, Frames: t.Frames - t.PadFrames, Pad: t.PadFrames}
	}
	setOffsets(toc.Tracks)
	return toc, srcs, p.warnings, nil
}

// previousRawSize is the size in bytes that chdman takes track n-1 to have
// when it places track n after it in the same bin. It multiplies two uint32,
// so a track of 4 GiB or more wraps; the value is kept, and warned about,
// because the offsets and sizes that follow from it are written into the CHD.
func (p *cueParser) previousRawSize(n int) uint64 {
	prev := &p.tracks[n-1]
	size := uint64(prev.Frames) * uint64(prev.DataSize+prev.SubSize)
	if size > math.MaxUint32 {
		p.warnings = append(p.warnings, Warning{
			Kind:  WarnOffsetTruncated,
			Track: n + 1,
			Message: fmt.Sprintf("track %d: track %d is %d bytes, which chdman computes in 32 bits as %d; it reads track %d from the wrong place in %s",
				n+1, n, size, uint32(size), n+1, p.info[n].fname),
		})
	}
	return uint64(uint32(size))
}

// trailing records the bytes after the last whole frame of track n.
func (p *cueParser) trailing(n int, extra uint64) {
	if extra == 0 {
		return
	}
	p.warnings = append(p.warnings, Warning{
		Kind:    WarnTrailingBytes,
		Track:   n + 1,
		Message: fmt.Sprintf("track %d: %d bytes at the end of %s are not a whole frame and are dropped", n+1, extra, p.info[n].fname),
	})
}

func missingBin(name string) error {
	return fmt.Errorf("%w: couldn't find bin file %s (or it is empty): %w", chderr.InvalidCue, name, os.ErrNotExist)
}

// fileSize is get_file_size: zero for a file that cannot be opened.
func fileSize(name string) uint64 {
	fi, err := os.Stat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return uint64(fi.Size())
}

// filePath is get_file_path: everything up to the last backslash, or when
// there is none, up to the last slash.
func filePath(path string) string {
	if i := strings.LastIndexByte(path, '\\'); i >= 0 {
		return path[:i+1]
	}
	return path[:strings.LastIndexByte(path, '/')+1]
}

// isGDICue is is_gdicue: it looks for both "REM SINGLE-DENSITY AREA" and
// "REM HIGH-DENSITY AREA".
func isGDICue(lines [][]byte) bool {
	var single, high bool
	for _, buf := range lines {
		l := cueLine{buf: buf}
		if l.token() != "REM" {
			continue
		}
		l.skipSpace()
		rest := l.rest()
		switch {
		case bytes.HasPrefix(rest, []byte("SINGLE-DENSITY AREA")):
			single = true
		case bytes.HasPrefix(rest, []byte("HIGH-DENSITY AREA")):
			high = true
		}
	}
	return single && high
}

// cueLines splits data the way fgets(buf, 511, f) reads it: a line ends after
// a newline, or after maxLine bytes.
func cueLines(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		n := min(len(data), maxLine)
		if i := bytes.IndexByte(data[:n], '\n'); i >= 0 {
			n = i + 1
		}
		lines = append(lines, data[:n])
		data = data[n:]
	}
	return lines
}

// cueLine is a line buffer with chdman's tokenizer. Like the C string it is,
// it ends at the first NUL byte.
type cueLine struct {
	buf []byte
	i   int
}

// at returns the byte at i, or NUL past the end.
func (l *cueLine) at(i int) byte {
	if i < len(l.buf) {
		return l.buf[i]
	}
	return 0
}

func (l *cueLine) skipSpace() {
	for isSpace(l.at(l.i)) {
		l.i++
	}
}

// rest is the unread part of the line, up to the first NUL.
func (l *cueLine) rest() []byte {
	if l.i >= len(l.buf) {
		return nil
	}
	rest := l.buf[l.i:]
	if n := bytes.IndexByte(rest, 0); n >= 0 {
		rest = rest[:n]
	}
	return rest
}

// token is tokenize: it skips white space, then reads up to the next white
// space outside quotes. Quote characters are dropped, and a quote of one
// kind inside the other is not special.
func (l *cueLine) token() string {
	l.skipSpace()
	var tok []byte
	var single, double bool
	for ; l.at(l.i) != 0; l.i++ {
		c := l.buf[l.i]
		switch {
		case !single && c == '"':
			double = !double
		case !double && c == '\'':
			single = !single
		case !single && !double && isSpace(c):
			return string(tok)
		default:
			tok = append(tok, c)
		}
	}
	return string(tok)
}

// isSpace is isspace in the C locale.
func isSpace(c byte) bool {
	return c == ' ' || (c >= '\t' && c <= '\r')
}

// strtoul is strtoul(s, nullptr, 10) with a 64-bit unsigned long.
func strtoul(s string) uint64 {
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	var v uint64
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		v = v*10 + uint64(s[i]-'0')
	}
	if neg {
		return -v
	}
	return v
}

// msfToFrames is msf_to_frames: sscanf("%d:%d:%d") into minutes, seconds and
// frames. A lone number is a frame count; fields that are missing count as 0.
func msfToFrames(token string) int {
	var f [3]int32
	n := 0
	pos := 0
	for n < 3 {
		if n > 0 {
			if pos >= len(token) || token[pos] != ':' {
				break
			}
			pos++
		}
		v, next, ok := scanInt(token, pos)
		if !ok {
			break
		}
		f[n], pos = v, next
		n++
	}
	m, s, fr := f[0], f[1], f[2]
	if n == 1 {
		return int(m)
	}
	s += m * 60
	fr += s * 75
	return int(fr)
}

// scanInt reads one %d conversion of sscanf at pos.
func scanInt(s string, pos int) (v int32, next int, ok bool) {
	for pos < len(s) && isSpace(s[pos]) {
		pos++
	}
	neg := false
	if pos < len(s) && (s[pos] == '+' || s[pos] == '-') {
		neg = s[pos] == '-'
		pos++
	}
	start := pos
	var u uint32
	for ; pos < len(s) && s[pos] >= '0' && s[pos] <= '9'; pos++ {
		u = u*10 + uint32(s[pos]-'0')
	}
	if pos == start {
		return 0, pos, false
	}
	if neg {
		return -int32(u), pos, true
	}
	return int32(u), pos, true
}
