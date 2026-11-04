package chdman

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/chdwriter"
	"github.com/lcmps/chdman-go/internal/format"
)

// CreateOptions control CreateDVD and CreateCD. The zero value means chdman's
// defaults.
type CreateOptions struct {
	// Codecs lists up to four codec tags such as "lzma" and "zlib", or just
	// "none" for an uncompressed CHD. Empty means the default for the media.
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
// only name selects no compression. More than four names, or an unknown one,
// is an error.
func parseCodecs(names []string, defaults [4]format.CodecTag) ([4]format.CodecTag, error) {
	var tags [4]format.CodecTag
	switch {
	case len(names) == 0:
		return defaults, nil
	case len(names) == 1 && names[0] == "none":
		return tags, nil
	case len(names) > len(tags):
		return tags, fmt.Errorf("chdman: %d codecs given, at most %d allowed", len(names), len(tags))
	}
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
