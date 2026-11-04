// Package chdman creates, verifies, extracts and randomly reads CHD v5 files.
package chdman

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/chderr"
)

// Sentinel errors, usable with errors.Is.
var (
	ErrUnsupportedVersion  = chderr.UnsupportedVersion
	ErrParentRequired      = chderr.ParentRequired
	ErrUnsupportedCodec    = chderr.UnsupportedCodec
	ErrUnsupportedDiscType = chderr.UnsupportedDiscType
	ErrCorruptData         = chderr.CorruptData
	ErrHashMismatch        = chderr.HashMismatch
	ErrInvalidCue          = chderr.InvalidCue
)

// ErrNoUserData reports a sector with no 2048-byte payload: audio, and
// MODE2/2324 (MODE2_FORM2) sectors.
var ErrNoUserData = errors.New("chdman: sector has no 2048-byte user data")

// ErrShortBuffer reports a destination too small for the sector.
var ErrShortBuffer = errors.New("chdman: destination buffer too small")

// Track describes one track of a CD CHD.
type Track struct {
	Number int
	// Type is the chdman track type, such as "MODE1_RAW", "MODE2_RAW" or "AUDIO".
	Type string
	// DataSize is the stored size of a sector in bytes, SubSize the size of its
	// subcode.
	DataSize, SubSize uint32
	// Frames is the number of frames stored for the track, including a pregap
	// that is stored in the data.
	Frames uint32
	// Pregap and Postgap are in frames. chdman records a negative pregap for
	// a cue sheet whose INDEX 00 lies in an earlier FILE than INDEX 01.
	Pregap, Postgap int32
	// Session is always 1: the CHD metadata does not record sessions.
	Session int
	// StartLBA is the logical address of INDEX 01.
	StartLBA uint32
}

// ReadOptions select the addressing of ReadSector and ReadUserData.
type ReadOptions struct {
	// Physical counts frames as stored in the CHD, as extractcd does. The
	// default is logical addressing, the disc position that filesystem
	// structures use, where a pregap that is not stored shifts later tracks
	// and reads as zeros.
	Physical bool
}

// Tracks returns the tracks of a CD CHD. A DVD has none and gives nil, nil;
// any other kind of CHD (a GD-ROM, a hard disk, or one with no recognised
// metadata) gives ErrUnsupportedDiscType, and track metadata that cannot be
// read gives ErrCorruptData.
func (r *Reader) Tracks() ([]Track, error) {
	if r.info.Disc == DiscDVD {
		return nil, nil
	}
	toc, err := r.cdTOC()
	if err != nil {
		return nil, err
	}
	tracks := make([]Track, len(toc.Tracks))
	for i, t := range toc.Tracks {
		tracks[i] = Track{
			Number: i + 1, Type: t.Type.String(), DataSize: t.DataSize, SubSize: t.SubSize,
			Frames: t.Frames, Pregap: int32(t.Pregap), Postgap: int32(t.Postgap), Session: 1, StartLBA: t.LogFrameOfs,
		}
	}
	return tracks, nil
}

// cdTOC returns the table of contents, or ErrUnsupportedDiscType unless the
// CHD is a CD.
func (r *Reader) cdTOC() (cdrom.TOC, error) {
	if r.info.Disc != DiscCD {
		return cdrom.TOC{}, fmt.Errorf("%w: %v CHD has no tracks", ErrUnsupportedDiscType, r.info.Disc)
	}
	r.tocOnce.Do(func() { r.toc, r.tocErr = cdrom.ParseMetadata(r.meta) })
	return r.toc, r.tocErr
}

// ReadSector reads the sector at lba into dst and returns its length. On a CD
// that is the track's DataSize (2352 for audio and raw data, 2048 or 2336 for
// the cooked modes), as stored: audio is not byte-swapped, so it is
// big-endian as in the CHD, the opposite of a bin file. On a DVD it is 2048
// bytes. dst must hold the whole sector (ErrShortBuffer), and an lba past the
// end gives an error wrapping io.EOF. Other kinds of CHD give
// ErrUnsupportedDiscType.
//
// lba is logical unless opts.Physical is set. Lead-in and lead-out are not
// stored in a CHD, so on a multisession disc the addresses of later sessions
// may not resolve to exactly what the cue sheet gave; 0.289 behaves the same.
func (r *Reader) ReadSector(lba uint32, dst []byte, opts ReadOptions) (int, error) {
	return r.readSector(lba, dst, opts, false)
}

// ReadUserData reads the 2048 bytes of user data of the sector at lba into
// dst and returns 2048. It follows read_data's conversions: it skips the sync,
// header and subheader of raw sectors. Audio and MODE2/2324 sectors give
// ErrNoUserData. MODE2/2336 sectors are read at offset 8, which read_data
// refuses. Addressing, errors and the multisession limit are as for ReadSector.
func (r *Reader) ReadUserData(lba uint32, dst []byte, opts ReadOptions) (int, error) {
	return r.readSector(lba, dst, opts, true)
}

func (r *Reader) readSector(lba uint32, dst []byte, opts ReadOptions, userData bool) (int, error) {
	switch r.info.Disc {
	case DiscDVD:
		return r.readDVDSector(lba, dst)
	case DiscCD:
	default:
		return 0, fmt.Errorf("%w: sector access needs a CD or DVD, this is a %v CHD", ErrUnsupportedDiscType, r.info.Disc)
	}
	toc, err := r.cdTOC()
	if err != nil {
		return 0, err
	}
	loc, err := toc.Locate(lba, opts.Physical)
	if err != nil {
		return 0, err
	}
	track := toc.Tracks[loc.Track]
	size, offset := int(track.DataSize), 0
	if userData {
		off, ok := cdrom.UserDataOffset(track.Type)
		if !ok {
			return 0, fmt.Errorf("%w: track %d is %v", ErrNoUserData, loc.Track+1, track.Type)
		}
		size, offset = cdrom.UserDataSize, off
	}
	if len(dst) < size {
		return 0, fmt.Errorf("%w: sector %d needs %d bytes, have %d", ErrShortBuffer, lba, size, len(dst))
	}
	dst = dst[:size]
	if loc.Zero {
		clear(dst)
		return size, nil
	}
	pos := loc.Frame*cdrom.FrameSize + uint64(offset)
	if pos > math.MaxInt64 {
		return 0, fmt.Errorf("sector %d: %w", lba, io.EOF)
	}
	if n, err := r.ReadAt(dst, int64(pos)); err != nil {
		return n, fmt.Errorf("sector %d: %w", lba, err)
	}
	return size, nil
}

func (r *Reader) readDVDSector(lba uint32, dst []byte) (int, error) {
	const size = cdrom.UserDataSize
	if len(dst) < size {
		return 0, fmt.Errorf("%w: sector %d needs %d bytes, have %d", ErrShortBuffer, lba, size, len(dst))
	}
	pos := uint64(lba) * size
	if pos+size > r.hdr.LogicalBytes {
		return 0, fmt.Errorf("sector %d is past the end of the disc: %w", lba, io.EOF)
	}
	if n, err := r.ReadAt(dst[:size], int64(pos)); err != nil {
		return n, fmt.Errorf("sector %d: %w", lba, err)
	}
	return size, nil
}
