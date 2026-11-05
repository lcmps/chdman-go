package codec

// Translated from MAME's src/lib/util/chdcodec.cpp (BSD-3-Clause, copyright
// Aaron Giles); see NOTICE.

import (
	"bytes"

	"github.com/lcmps/chdman-go/internal/format"
)

// Group holds up to four compressors and picks the best per hunk, like
// chd_compressor_group. It is not safe for concurrent use.
type Group struct {
	hunkBytes   uint32
	compressors [4]Compressor
	audioOnly   [4]bool // slots not worth running on a hunk of data sectors
	scratch     []byte  // compressor output buffer
	best        []byte  // copy of the best result so far
}

// NewGroup creates a compressor for every non-empty tag.
func NewGroup(tags [4]format.CodecTag, hunkBytes uint32) (*Group, error) {
	g := &Group{hunkBytes: hunkBytes}
	hasGeneralCodec := false
	for i, t := range tags {
		if t == format.CodecNone {
			continue
		}
		c, err := NewCompressor(t, hunkBytes)
		if err != nil {
			return nil, err
		}
		g.compressors[i] = c
		hasGeneralCodec = hasGeneralCodec || t != format.CodecCDFLAC
	}
	// CD FLAC models audio and costs as much as the other codecs, so with
	// another codec to fall back on it is skipped for data sectors.
	for i, t := range tags {
		g.audioOnly[i] = t == format.CodecCDFLAC && hasGeneralCodec
	}
	return g, nil
}

// isDataSectorHunk reports whether every frame of a CD hunk starts with the
// sync pattern of a data sector.
func isDataSectorHunk(hunk []byte) bool {
	if len(hunk) == 0 || len(hunk)%cdFrameSize != 0 {
		return false
	}
	for off := 0; off < len(hunk); off += cdFrameSize {
		if !bytes.Equal(hunk[off:off+len(cdSyncHeader)], cdSyncHeader[:]) {
			return false
		}
	}
	return true
}

// Best tries each useful codec and returns the map type (the codec slot, or
// format.CompNone) and the data to store. A codec result is used only if it
// is strictly smaller than hunkBytes and than every earlier slot's result;
// codec errors are swallowed, as in find_best_compressor. For CompNone, data
// is src itself. For a codec slot, data is owned by the group and valid only
// until the next call to Best.
func (g *Group) Best(src []byte) (mapType uint8, data []byte, err error) {
	mapType, data = format.CompNone, src
	best := int(g.hunkBytes)
	g.best = g.best[:0]
	dataHunk := isDataSectorHunk(src)
	for slot, c := range g.compressors {
		if c == nil || (dataHunk && g.audioOnly[slot]) {
			continue
		}
		out, cerr := c.Compress(g.scratch, src)
		if cerr != nil {
			continue
		}
		g.scratch = out[:0]
		if len(out) < best {
			best = len(out)
			mapType = uint8(slot)
			g.best = append(g.best[:0], out...)
			data = g.best
		}
	}
	return mapType, data, nil
}
