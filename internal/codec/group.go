package codec

// Translated from MAME's src/lib/util/chdcodec.cpp (BSD-3-Clause, copyright
// Aaron Giles); see NOTICE.

import "github.com/lcmps/chdman-go/internal/format"

// Group holds up to four compressors and picks the best per hunk, like
// chd_compressor_group. It is not safe for concurrent use.
type Group struct {
	hunkBytes   uint32
	compressors [4]Compressor
	scratch     []byte // compressor output buffer
	best        []byte // copy of the best result so far
}

// NewGroup creates a compressor for every non-empty tag.
func NewGroup(tags [4]format.CodecTag, hunkBytes uint32) (*Group, error) {
	g := &Group{hunkBytes: hunkBytes}
	for i, t := range tags {
		if t == format.CodecNone {
			continue
		}
		c, err := NewCompressor(t, hunkBytes)
		if err != nil {
			return nil, err
		}
		g.compressors[i] = c
	}
	return g, nil
}

// Best tries every codec and returns the map type (the codec slot, or
// format.CompNone) and the data to store. A codec result is used only if it
// is strictly smaller than hunkBytes and than every earlier slot's result;
// codec errors are swallowed, as in find_best_compressor. For CompNone, data
// is src itself. For a codec slot, data is owned by the group and valid only
// until the next call to Best.
func (g *Group) Best(src []byte) (mapType uint8, data []byte, err error) {
	mapType, data = format.CompNone, src
	best := int(g.hunkBytes)
	g.best = g.best[:0]
	for slot, c := range g.compressors {
		if c == nil {
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
