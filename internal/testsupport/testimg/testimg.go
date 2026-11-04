// Package testimg generates deterministic ISO and bin/cue test images.
package testimg

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lcmps/chdman-go/internal/cdrom"
)

// Shape selects the content pattern of generated sector payloads.
type Shape int

const (
	// Zeros is all-zero payload.
	Zeros Shape = iota
	// Random is incompressible payload. For raw CD data tracks it also means
	// the whole sector is random, so sync and ECC are not valid.
	Random
	// Repeated is a short pattern repeated through each sector.
	Repeated
	// Mixed cycles zero, random, repeated and half-random sectors.
	Mixed
)

const (
	isoSector = 2048
	rawSector = cdrom.SectorSize
)

// Track describes one cue track. Frames is the number of frames the track
// occupies in its bin, including IndexZero frames. Pregap frames are declared
// with a PREGAP command and are not stored in the bin.
type Track struct {
	Mode      string // cue mode, such as "MODE1/2352" or "AUDIO"
	Frames    int
	Pregap    int
	Postgap   int
	IndexZero int // frames of INDEX 00 data at the start of the track
	Shape     Shape
}

// Disc describes a cue sheet. Session2From is the 1-based number of the first
// track of a second session, or zero for a single session.
type Disc struct {
	Tracks       []Track
	Split        bool
	Session2From int
}

// ISO writes sectors 2048-byte sectors to dir/image.iso and returns its path.
func ISO(t testing.TB, dir string, sectors int, s Shape) string {
	t.Helper()
	buf := make([]byte, 0, sectors*isoSector)
	for i := range sectors {
		buf = append(buf, payload(s, i, isoSector)...)
	}
	path := filepath.Join(dir, "image.iso")
	write(t, path, buf)
	return path
}

// Cue writes the bins and a cue sheet (disc.cue) into dir and returns the cue
// path. Data sectors have valid sync and ECC unless the track Shape is Random.
func Cue(t testing.TB, dir string, d Disc) string {
	t.Helper()
	var cue strings.Builder
	var whole []byte // the shared bin when not split
	lba := 0         // absolute frame, used for sector headers

	for n, tr := range d.Tracks {
		num := n + 1
		var bin []byte
		for i := range tr.Frames {
			bin = append(bin, sector(t, tr, i, lba+i)...)
		}
		lba += tr.Frames

		start := 0
		if d.Split {
			name := fmt.Sprintf("track%02d.bin", num)
			write(t, filepath.Join(dir, name), bin)
			fmt.Fprintf(&cue, "FILE %q BINARY\n", name)
		} else {
			if n == 0 {
				cue.WriteString("FILE \"disc.bin\" BINARY\n")
			}
			start = len(whole) / rawSectorFor(t, tr)
			whole = append(whole, bin...)
		}
		if d.Session2From == num {
			cue.WriteString("REM SESSION 02\n")
		}
		fmt.Fprintf(&cue, "  TRACK %02d %s\n", num, tr.Mode)
		if tr.Pregap > 0 {
			fmt.Fprintf(&cue, "    PREGAP %s\n", msf(tr.Pregap))
		}
		if tr.IndexZero > 0 {
			fmt.Fprintf(&cue, "    INDEX 00 %s\n", msf(start))
		}
		fmt.Fprintf(&cue, "    INDEX 01 %s\n", msf(start+tr.IndexZero))
		if tr.Postgap > 0 {
			fmt.Fprintf(&cue, "    POSTGAP %s\n", msf(tr.Postgap))
		}
	}
	if !d.Split {
		write(t, filepath.Join(dir, "disc.bin"), whole)
	}
	path := filepath.Join(dir, "disc.cue")
	write(t, path, []byte(cue.String()))
	return path
}

// rawSectorFor returns the byte size of one sector of the track's mode.
func rawSectorFor(t testing.TB, tr Track) int {
	t.Helper()
	switch tr.Mode {
	case "MODE1/2352", "MODE2/2352", "AUDIO":
		return rawSector
	case "MODE1/2048":
		return isoSector
	}
	t.Fatalf("testimg: unsupported track mode %q", tr.Mode)
	return 0
}

// sector builds frame i of a track; lba is its absolute frame number.
func sector(t testing.TB, tr Track, i, lba int) []byte {
	size := rawSectorFor(t, tr)
	if tr.Mode == "AUDIO" || tr.Mode == "MODE1/2048" || tr.Shape == Random {
		return payload(tr.Shape, lba, size)
	}
	mode := byte(1)
	if tr.Mode == "MODE2/2352" {
		mode = 2
	}
	s := make([]byte, rawSector)
	copy(s, []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00})
	m, sec, f := lbaToMSF(lba + 150)
	s[12], s[13], s[14], s[15] = bcd(m), bcd(sec), bcd(f), mode
	copy(s[16:16+isoSector], payload(tr.Shape, lba, isoSector))
	cdrom.ECCGenerate(s)
	return s
}

// payload returns size deterministic bytes for sector idx of shape s.
func payload(s Shape, idx, size int) []byte {
	if s == Mixed {
		s = [...]Shape{Zeros, Random, Repeated, Random}[idx%4]
		if idx%8 == 7 { // half random, half zero
			b := payload(Random, idx, size)
			clear(b[size/2:])
			return b
		}
	}
	b := make([]byte, size)
	switch s {
	case Random:
		rng := rand.New(rand.NewPCG(0x6368646d, uint64(idx)))
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
	case Repeated:
		pat := []byte{0xde, 0xad, 0xbe, 0xef, byte(idx)}
		copy(b, bytes.Repeat(pat, size/len(pat)+1))
	}
	return b
}

func lbaToMSF(lba int) (m, s, f int) {
	return lba / (60 * 75), lba / 75 % 60, lba % 75
}

func bcd(v int) byte { return byte(v/10<<4 | v%10) }

func msf(frames int) string {
	m, s, f := lbaToMSF(frames)
	return fmt.Sprintf("%02d:%02d:%02d", m, s, f)
}

func write(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
