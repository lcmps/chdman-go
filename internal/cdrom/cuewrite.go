package cdrom

// This file is a translation of the MODE_CUEBIN branch of output_track_metadata
// from MAME 0.289, src/tools/chdman.cpp, BSD-3-Clause, copyright the MAME team.

import (
	"fmt"
	"io"
	"strings"
)

// WriteCueEntry writes the cue sheet lines for one track, as chdman extractcd
// does. binName is the name of the bin that holds the track. discOfs is the
// frame at which the track starts in that bin. fileOfs is the number of bytes
// already in the bin: only when it is zero does the entry start with a FILE
// line.
//
// Like chdman 0.289, it writes no REM SESSION line.
func WriteCueEntry(w io.Writer, trackNum int, t Track, binName string, discOfs, fileOfs uint32) error {
	var b strings.Builder
	if fileOfs == 0 {
		fmt.Fprintf(&b, "FILE \"%s\" BINARY\n", binName)
	}
	var mode string
	switch t.Type {
	case MODE1, MODE1_RAW:
		mode = fmt.Sprintf("MODE1/%04d", t.DataSize)
	case MODE2, MODE2_FORM1, MODE2_FORM2, MODE2_FORM_MIX, MODE2_RAW:
		mode = fmt.Sprintf("MODE2/%04d", t.DataSize)
	case AUDIO:
		mode = "AUDIO"
	}
	fmt.Fprintf(&b, "  TRACK %02d %s\n", trackNum, mode)
	switch {
	case t.Pregap > 0 && t.PregapDataSize == 0:
		// The pregap is not in the bin.
		fmt.Fprintf(&b, "    PREGAP %s\n", msfString(t.Pregap))
		fmt.Fprintf(&b, "    INDEX 01 %s\n", msfString(discOfs))
	case t.Pregap > 0:
		fmt.Fprintf(&b, "    INDEX 00 %s\n", msfString(discOfs))
		fmt.Fprintf(&b, "    INDEX 01 %s\n", msfString(discOfs+t.Pregap))
	default:
		fmt.Fprintf(&b, "    INDEX 01 %s\n", msfString(discOfs))
	}
	if t.Postgap > 0 {
		fmt.Fprintf(&b, "    POSTGAP %s\n", msfString(t.Postgap))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// msfString formats a frame count as minutes:seconds:frames.
func msfString(frames uint32) string {
	return fmt.Sprintf("%02d:%02d:%02d", frames/(75*60), frames/75%60, frames%75)
}
