package chdman

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/chdwriter"
	"github.com/lcmps/chdman-go/internal/format"
)

// CreateOptions control CreateDVD and CreateCD. The zero value means chdman's
// defaults.
type CreateOptions struct {
	// Codecs lists up to four codec tags such as "lzma" and "zlib", or just
	// "none" for an uncompressed CHD; further names are ignored, as chdman
	// does. Nil or empty means the default for the media, so an empty name
	// is a name that is not a codec.
	Codecs []string
	// HunkBytes is the hunk size; zero means the default for the media.
	HunkBytes uint32
	// Workers is the number of compression goroutines; zero means one per CPU.
	Workers int
	// Force replaces an existing output file. The replacement keeps the old
	// file's permissions. If the output path is a symbolic link, the link
	// itself is replaced and its target is left untouched.
	Force bool
	// Progress, if set, is called as input bytes are consumed.
	// The callback runs on a goroutine of the library, not the caller's, and
	// must not panic.
	Progress func(done, total uint64)
}

// CreateResult describes a CHD that was written.
type CreateResult struct {
	// RawSHA1 and SHA1 are the hashes stored in the CHD, in hex. They are all
	// zeros for an uncompressed file, which stores none.
	RawSHA1, SHA1 string
	Warnings      []Warning
}

// WarningKind identifies a kind of Warning.
type WarningKind string

// Warning kinds. They report input data that chdman also drops or alters.
const (
	WarnTrailingBytesDropped WarningKind = "trailing-bytes-dropped"
	WarnFramesOverwritten    WarningKind = "frames-overwritten"
	WarnOffsetTruncated      WarningKind = "track-offset-truncated"
	WarnFileUnused           WarningKind = "file-unused"
	WarnFramesDropped        WarningKind = "frames-dropped"
	WarnSubcodeOmitted       WarningKind = "subcode-omitted"
)

// Warning reports input data that was lost or altered.
type Warning struct {
	Kind WarningKind
	// Track is the 1-based track number, or zero if the warning is not about a track.
	Track   int
	Message string
}

const (
	dvdSectorBytes = 2048
	dvdHunkBytes   = 2 * dvdSectorBytes
	minHunkBytes   = 16 // HUNK_SIZE_MIN in chdman.cpp
)

// defaultCDCodecs are chdman's codecs for CDs, s_default_cd_compression.
var defaultCDCodecs = [4]format.CodecTag{format.CodecCDLZMA, format.CodecCDZlib, format.CodecCDFLAC}

// defaultHDCodecs are chdman's codecs for hard disks and DVDs.
var defaultHDCodecs = [4]format.CodecTag{format.CodecLZMA, format.CodecZlib, format.CodecHuffman, format.CodecFLAC}

// codecTags maps the names chdman accepts for -c to codec tags.
var codecTags = map[string]format.CodecTag{
	"zlib": format.CodecZlib, "zstd": format.CodecZstd, "lzma": format.CodecLZMA,
	"huff": format.CodecHuffman, "flac": format.CodecFLAC,
	"cdzl": format.CodecCDZlib, "cdzs": format.CodecCDZstd,
	"cdlz": format.CodecCDLZMA, "cdfl": format.CodecCDFLAC,
}

// parseCodecs converts the names in CreateOptions.Codecs to codec slots, like
// parse_compression in chdman.cpp. No names selects defaults, and "none" as the
// only name selects no compression. An unknown name, or an empty one, is an
// error. Only the first four names are read: chdman stops there, so a fifth
// is ignored without being checked.
func parseCodecs(names []string, defaults [4]format.CodecTag) ([4]format.CodecTag, error) {
	var tags [4]format.CodecTag
	switch {
	case len(names) == 0:
		return defaults, nil
	case len(names) == 1 && names[0] == "none":
		return tags, nil
	}
	names = names[:min(len(names), len(tags))]
	for i, name := range names {
		tag, ok := codecTags[name]
		switch {
		case ok:
			tags[i] = tag
		case name == "avhu":
			return tags, fmt.Errorf("%w: %q", chderr.UnsupportedCodec, name)
		default:
			return tags, fmt.Errorf("chdman: invalid compressor %q", name)
		}
	}
	return tags, nil
}

// parseHunkBytes validates a requested hunk size like parse_hunk_size in
// chdman.cpp. Zero selects def. A size must lie within chdman's limits and be
// a multiple of granularity.
func parseHunkBytes(requested, granularity, def uint32) (uint32, error) {
	size := def
	if requested != 0 {
		size = requested
		if size < minHunkBytes {
			return 0, fmt.Errorf("chdman: invalid hunk size %d (minimum %d)", size, minHunkBytes)
		}
		if size > format.MaxHunkBytes {
			return 0, fmt.Errorf("chdman: invalid hunk size %d (maximum %d)", size, format.MaxHunkBytes)
		}
	}
	if size%granularity != 0 {
		return 0, fmt.Errorf("chdman: hunk size %d bytes is not a whole multiple of %d", size, granularity)
	}
	return size, nil
}

func workerCount(n int) int {
	if n < 1 {
		return runtime.NumCPU()
	}
	return n
}

// CreateDVD compresses the ISO image at isoPath into the CHD at outPath, like
// chdman createdvd. The image must be a non-empty multiple of 2048 bytes. The
// output appears only if the whole run succeeds.
func CreateDVD(ctx context.Context, isoPath, outPath string, opts CreateOptions) (CreateResult, error) {
	codecs, err := parseCodecs(opts.Codecs, defaultHDCodecs)
	if err != nil {
		return CreateResult{}, err
	}
	hunk, err := parseHunkBytes(opts.HunkBytes, dvdSectorBytes, dvdHunkBytes)
	if err != nil {
		return CreateResult{}, err
	}
	in, err := os.Open(isoPath)
	if err != nil {
		return CreateResult{}, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return CreateResult{}, err
	}
	size := st.Size()
	switch {
	case size == 0:
		return CreateResult{}, fmt.Errorf("chdman: %s is empty", isoPath)
	case size%dvdSectorBytes != 0:
		return CreateResult{}, fmt.Errorf("chdman: data size %d is not divisible by sector size %d", size, dvdSectorBytes)
	}

	var res CreateResult
	err = writeOutputAtomically(ctx, outPath, opts.Force, []string{isoPath}, func(f *os.File) error {
		raw, sum, err := chdwriter.Write(ctx, f, &isoSource{r: in, size: size}, chdwriter.Options{
			Codecs:       codecs,
			HunkBytes:    hunk,
			UnitBytes:    dvdSectorBytes,
			LogicalBytes: uint64(size),
			Metadata:     []format.MetadataEntry{{Tag: format.TagDVD, Flags: format.FlagChecksum, Data: []byte{0}}},
			Workers:      workerCount(opts.Workers),
			Progress:     opts.Progress,
		})
		res.RawSHA1, res.SHA1 = hex.EncodeToString(raw[:]), hex.EncodeToString(sum[:])
		return err
	})
	if err != nil {
		return CreateResult{}, err
	}
	return res, nil
}

// isoSource serves hunks of a flat file; the last is zero-padded.
type isoSource struct {
	r    io.ReaderAt
	size int64
}

func (s *isoSource) ReadHunk(index uint32, dst []byte) error {
	off := int64(index) * int64(len(dst))
	want := int(min(int64(len(dst)), s.size-off))
	n, err := s.r.ReadAt(dst[:want], off)
	if n < want {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	clear(dst[want:])
	return nil
}

// CreateCD compresses the CD described by the cue sheet at cuePath into the
// CHD at outPath, like chdman createcd. The bins are found next to the cue.
// Input that chdman drops or overwrites is converted the same way and
// reported in CreateResult.Warnings. The output appears only if the whole run
// succeeds.
func CreateCD(ctx context.Context, cuePath, outPath string, opts CreateOptions) (CreateResult, error) {
	codecs, err := parseCodecs(opts.Codecs, defaultCDCodecs)
	if err != nil {
		return CreateResult{}, err
	}
	hunk, err := parseHunkBytes(opts.HunkBytes, cdrom.FrameSize, cdrom.FramesPerHunk*cdrom.FrameSize)
	if err != nil {
		return CreateResult{}, err
	}
	cue, err := cdrom.ParseCue(cuePath)
	if err != nil {
		return CreateResult{}, err
	}
	toc, srcs, warns := cue.TOC, cue.Sources, cue.Warnings
	inputs, err := checkCueBins(toc, srcs, cuePath, cue.Files)
	if err != nil {
		return CreateResult{}, err
	}

	var frames uint64
	for _, t := range toc.Tracks {
		frames += uint64(t.Frames + t.ExtraFrames)
	}
	src := &cdSource{toc: toc, srcs: srcs, framesPerHunk: hunk / cdrom.FrameSize}
	defer src.Close()

	var res CreateResult
	err = writeOutputAtomically(ctx, outPath, opts.Force, inputs, func(f *os.File) error {
		raw, sum, err := chdwriter.Write(ctx, f, src, chdwriter.Options{
			Codecs:       codecs,
			HunkBytes:    hunk,
			UnitBytes:    cdrom.FrameSize,
			LogicalBytes: frames * cdrom.FrameSize,
			Metadata:     cdrom.Metadata(toc),
			Workers:      workerCount(opts.Workers),
			Progress:     opts.Progress,
		})
		res.RawSHA1, res.SHA1 = hex.EncodeToString(raw[:]), hex.EncodeToString(sum[:])
		return err
	})
	if err != nil {
		return CreateResult{}, err
	}
	for _, w := range warns {
		res.Warnings = append(res.Warnings, Warning{Kind: WarningKind(w.Kind), Track: w.Track, Message: w.Message})
	}
	return res, nil
}

// checkCueBins makes sure that every track's frames lie within its bin, so a
// short file fails before anything is written, and returns the paths that the
// output must not overwrite: the cue and each bin that it names, which
// includes one that no track reads.
func checkCueBins(toc cdrom.TOC, srcs []cdrom.TrackSource, cuePath string, files []string) ([]string, error) {
	inputs := []string{cuePath}
	sizes := map[string]uint64{}
	for i, s := range srcs {
		if s.Frames == 0 {
			continue
		}
		size, ok := sizes[s.Path]
		if !ok {
			st, err := os.Stat(s.Path)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", chderr.InvalidCue, err)
			}
			size = uint64(st.Size())
			sizes[s.Path] = size
		}
		t := toc.Tracks[i]
		if end := s.Offset + uint64(s.Frames)*uint64(t.DataSize+t.SubSize); end > size {
			return nil, fmt.Errorf("%w: track %d needs %d bytes of %s, which has %d", chderr.InvalidCue, i+1, end, s.Path, size)
		}
	}
	for _, name := range files {
		// A file that is named and missing is no input, and no error: chdman
		// does not look at a file that no track reads.
		if _, err := os.Stat(name); err == nil {
			inputs = append(inputs, name)
		}
	}
	return inputs, nil
}

// cdSource serves hunks of a CD as chdman lays it out, a translation of
// chd_cd_compressor::read_data in chdman.cpp: each track in order, every
// frame 2352 bytes of sector then 96 of subcode, followed by the track's
// pad and extra frames of zeros. It keeps at most one bin open, the one that
// the current track reads from.
type cdSource struct {
	toc           cdrom.TOC
	srcs          []cdrom.TrackSource
	framesPerHunk uint32

	f       *os.File
	path    string
	scratch []byte

	track      int    // the track that the last frame read belonged to
	trackFirst uint32 // the frame at which that track starts
}

// Close closes the open bin. It is safe to call more than once.
func (s *cdSource) Close() error {
	f := s.f
	s.f, s.path = nil, ""
	if f == nil {
		return nil
	}
	return f.Close()
}

func (s *cdSource) ReadHunk(index uint32, dst []byte) error {
	clear(dst)
	frame := uint64(index) * uint64(s.framesPerHunk)
	if frame < uint64(s.trackFirst) {
		s.track, s.trackFirst = 0, 0
	}
	for n := uint32(0); n < s.framesPerHunk; {
		// Find the track holding the frame; past the last one the hunk is
		// padding.
		for s.track < len(s.toc.Tracks) {
			t := s.toc.Tracks[s.track]
			if frame < uint64(s.trackFirst)+uint64(t.Frames+t.ExtraFrames) {
				break
			}
			s.trackFirst += t.Frames + t.ExtraFrames
			s.track++
		}
		if s.track == len(s.toc.Tracks) {
			return nil
		}
		t, src := s.toc.Tracks[s.track], s.srcs[s.track]
		rel := uint32(frame - uint64(s.trackFirst))
		inTrack := min(s.framesPerHunk-n, t.Frames+t.ExtraFrames-rel)
		if rel < src.Frames {
			// Frames that come from the bin; the rest of the track is zeros.
			run := min(inTrack, src.Frames-rel)
			if err := s.readFrames(dst[n*cdrom.FrameSize:], t, src, rel, run); err != nil {
				return err
			}
		}
		n += inTrack
		frame += uint64(inTrack)
	}
	return nil
}

// readFrames reads count frames of a track, starting at its frame rel, into
// dst, whose frames are cdrom.FrameSize apart.
func (s *cdSource) readFrames(dst []byte, t cdrom.Track, src cdrom.TrackSource, rel, count uint32) error {
	if s.path != src.Path || s.f == nil {
		if err := s.Close(); err != nil {
			return err
		}
		f, err := os.Open(src.Path)
		if err != nil {
			return err
		}
		s.f, s.path = f, src.Path
	}
	perFrame := int(t.DataSize + t.SubSize)
	s.scratch = slices.Grow(s.scratch[:0], int(count)*perFrame)[:int(count)*perFrame]
	off := src.Offset + uint64(rel)*uint64(perFrame)
	if _, err := s.f.ReadAt(s.scratch, int64(off)); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("reading %s: %w", src.Path, err)
	}
	for i := range int(count) {
		frame := dst[i*cdrom.FrameSize : (i+1)*cdrom.FrameSize]
		copy(frame, s.scratch[i*perFrame:(i+1)*perFrame])
		if src.Swap {
			for j := 0; j < cdrom.SectorSize; j += 2 {
				frame[j], frame[j+1] = frame[j+1], frame[j]
			}
		}
	}
	return nil
}
