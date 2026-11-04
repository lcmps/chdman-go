// Translated from MAME's src/lib/util/cdrom.cpp (BSD-3-Clause, tag mame0289).

package cdrom

import (
	"fmt"
	"io"
)

// Location says where a disc address is stored in the CHD.
type Location struct {
	// Track is the index into TOC.Tracks of the track the address belongs to.
	Track int
	// Frame is the CHD frame holding the sector. It is meaningless when Zero.
	Frame uint64
	// Zero reports an address in a pregap or postgap that is not stored in the
	// CHD: its sector reads as zeros.
	Zero bool
}

// Locate maps a disc address to the CHD frame that holds it. An address past
// the last frame of the disc yields an error wrapping io.EOF.
//
// Physical addresses count frames as stored, as physical_to_chd_lba does
// (cdrom.cpp:73). Logical addresses are the disc
// positions that filesystem structures use (cdrom.cpp:103): a PREGAP that is
// not in the data shifts the track, and read_partial_sector (cdrom.cpp:379)
// reads it as zeros, as it does a postgap. When the pregap is stored (INDEX 00
// data), the track starts at its index 0 frame and cdrom.cpp:393-396 reads
// from index 1 on, which is the same frame arithmetic: ChdFrameOfs plus the
// distance from index 0.
//
// One deliberate difference: cdrom.cpp finds the track with the first
// logframeofs above the address, so an address in a later track's pregap
// resolves to the previous track and reads whatever follows its frames. Here
// the address belongs to the track that owns the pregap, which is identical
// for stored pregaps of the same sector size and gives zeros for the others.
func (t TOC) Locate(lba uint32, phys bool) (Location, error) {
	addr := int64(lba)
	for i, tr := range t.Tracks {
		if phys {
			start := int64(tr.PhysFrameOfs)
			if addr >= start && addr < start+int64(tr.Frames) {
				return Location{Track: i, Frame: uint64(tr.ChdFrameOfs) + uint64(addr-start)}, nil
			}
			continue
		}
		// All of this is uint32 arithmetic, as in cdrom.cpp: a pregap that
		// chdman stored as a negative count wraps the offsets it implies.
		//
		// first is where the track's stored frames begin on the logical line.
		first := tr.LogFrameOfs
		var virtualPregap uint32
		if tr.PregapDataSize != 0 {
			first -= tr.Pregap
		} else {
			virtualPregap = tr.Pregap
		}
		end := first + tr.Frames
		switch {
		case lba-(first-virtualPregap) < virtualPregap:
			return Location{Track: i, Zero: true}, nil
		case lba-first < tr.Frames:
			return Location{Track: i, Frame: uint64(tr.ChdFrameOfs) + uint64(lba-first)}, nil
		case lba-end < tr.Postgap:
			return Location{Track: i, Zero: true}, nil
		}
	}
	return Location{}, fmt.Errorf("address %d is past the end of the disc: %w", lba, io.EOF)
}
