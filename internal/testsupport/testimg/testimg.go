// Package testimg generates deterministic ISO and bin/cue test images.
package testimg

import (
	"bytes"
	"encoding/binary"
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

// RepeatBlock is the number of sectors after which the Repeated shape
// starts over.
const RepeatBlock = 64

const (
	isoSector  = 2048
	mode2Plain = 2336
	// userOffset2336 is where the payload sits in a MODE2/2336 sector: after
	// the 8-byte XA subheader.
	userOffset2336 = 8
	userOffsetRaw1 = 16
	userOffsetRaw2 = 24
	rawSector      = cdrom.SectorSize
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
	// Data, if set, is user data placed in the payload of the first
	// len(Data)/2048 sectors of the track (rounded up, zero padded), at the
	// offset where the mode keeps it: 0 for MODE1/2048, 8 for MODE2/2336, 16
	// for MODE1/2352 and 24 for MODE2/2352. AUDIO tracks ignore it.
	Data []byte
}

// Disc describes a cue sheet. Session2From is the 1-based number of the first
// track of a second session, or zero for a single session.
type Disc struct {
	Tracks       []Track
	Split        bool
	Session2From int
}

// Layout is a named cue layout for tests that run over many disc shapes.
type Layout struct {
	Name string
	Disc Disc
}

// Layouts returns the disc layouts that cover the cue shapes Cue can express:
// every sector mode, one bin or one per track, INDEX 00, pregap, postgap,
// tracks that are not a multiple of four frames, and a second session.
func Layouts() []Layout {
	disc := func(split bool, session2From int, tracks ...Track) Disc {
		return Disc{Tracks: tracks, Split: split, Session2From: session2From}
	}
	data := func(mode string, frames int) Track { return Track{Mode: mode, Frames: frames, Shape: Mixed} }
	audio := func(frames int) Track { return Track{Mode: "AUDIO", Frames: frames, Shape: Mixed} }
	return []Layout{
		{"mode1-2048", disc(false, 0, data("MODE1/2048", 40))},
		{"mode1-2352", disc(false, 0, data("MODE1/2352", 40))},
		{"mode2-2336", disc(false, 0, data("MODE2/2336", 40))},
		{"mode2-2352", disc(false, 0, data("MODE2/2352", 40))},
		{"data-audio-single", disc(false, 0, data("MODE1/2352", 40), audio(30))},
		{"data-audio-split", disc(true, 0, data("MODE1/2352", 40), audio(30))},
		{"index00-single", disc(false, 0, data("MODE1/2352", 40),
			Track{Mode: "AUDIO", Frames: 30, IndexZero: 10, Shape: Mixed},
			Track{Mode: "AUDIO", Frames: 25, IndexZero: 7, Shape: Mixed})},
		{"index00-split", disc(true, 0, data("MODE1/2352", 40),
			Track{Mode: "AUDIO", Frames: 30, IndexZero: 10, Shape: Mixed})},
		{"pregap", disc(false, 0, data("MODE1/2352", 40), Track{Mode: "AUDIO", Frames: 30, Pregap: 150, Shape: Mixed})},
		{"postgap", disc(false, 0, Track{Mode: "MODE1/2352", Frames: 40, Postgap: 75, Shape: Mixed}, audio(30))},
		{"not-multiple-of-4", disc(false, 0, data("MODE1/2352", 41), audio(30), audio(7))},
		{"not-multiple-of-4-split", disc(true, 0, data("MODE1/2352", 41), audio(30), audio(7))},
		{"multisession-single-bin", disc(false, 2, data("MODE1/2352", 40), audio(30), data("MODE1/2352", 20))},
		{"multisession-multi-bin", disc(true, 2, data("MODE1/2352", 40), audio(30), data("MODE1/2352", 20))},
	}
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

// Bin writes the frames of one track to dir/name and returns its path. The
// content is the same as Cue generates for a track at the start of the disc.
func Bin(t testing.TB, dir, name string, tr Track) string {
	t.Helper()
	var bin []byte
	for i := range tr.Frames {
		bin = append(bin, sector(t, tr, i, i)...)
	}
	path := filepath.Join(dir, name)
	write(t, path, bin)
	return path
}

// WriteCue writes text verbatim to dir/name, for cue layouts that Cue cannot
// express, and returns its path. name may contain subdirectories of dir.
func WriteCue(t testing.TB, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, path, []byte(text))
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
	case "MODE2/2336":
		return mode2Plain
	}
	t.Fatalf("testimg: unsupported track mode %q", tr.Mode)
	return 0
}

// sector builds frame i of a track; lba is its absolute frame number.
func sector(t testing.TB, tr Track, i, lba int) []byte {
	size := rawSectorFor(t, tr)
	userData, hasData := tr.userData(i)
	if tr.Mode == "AUDIO" || tr.Mode == "MODE1/2048" || tr.Mode == "MODE2/2336" || tr.Shape == Random {
		s := payload(tr.Shape, lba, size)
		if hasData {
			switch tr.Mode {
			case "MODE1/2048":
				copy(s, userData)
			case "MODE2/2336":
				copy(s[userOffset2336:], userData)
			}
		}
		return s
	}
	mode, offset := byte(1), userOffsetRaw1
	if tr.Mode == "MODE2/2352" {
		mode, offset = 2, userOffsetRaw2
	}
	s := make([]byte, rawSector)
	copy(s, []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00})
	// Repeated sectors deliberately share a header: the MSF address wraps
	// every RepeatBlock frames, so the whole raw sector (and its ECC)
	// repeats and chdman can emit self-references.
	hdrLBA := lba
	if tr.Shape == Repeated {
		hdrLBA = lba % RepeatBlock
	}
	m, sec, f := lbaToMSF(hdrLBA + 150)
	s[12], s[13], s[14], s[15] = bcd(m), bcd(sec), bcd(f), mode
	copy(s[offset:offset+isoSector], payload(tr.Shape, lba, isoSector))
	if hasData {
		copy(s[offset:offset+isoSector], userData)
	}
	cdrom.ECCGenerate(s)
	return s
}

// userData returns the 2048 bytes of Data for sector i of the track.
func (tr Track) userData(i int) ([]byte, bool) {
	if tr.Mode == "AUDIO" || i*isoSector >= len(tr.Data) {
		return nil, false
	}
	b := make([]byte, isoSector)
	copy(b, tr.Data[i*isoSector:])
	return b, true
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
		// A block of RepeatBlock sectors repeated verbatim, so whole hunks
		// recur at any hunk size that divides the block.
		pat := []byte{0xde, 0xad, 0xbe, 0xef, byte(idx % RepeatBlock)}
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

// File is a root directory file of an ISO9660 image built by Filesystem.
type File struct {
	Name string // such as "SYSTEM.CNF"; ";1" is appended
	Data []byte
}

// Filesystem builds a minimal ISO9660 image of 2048-byte sectors: sixteen
// empty system sectors, the primary volume descriptor at sector 16, the
// terminator at 17, the root directory at 18, then each file's data, one file
// after another, each starting on a sector boundary.
func Filesystem(files ...File) []byte {
	const rootLBA = 18
	lbas := make([]int, len(files))
	next := rootLBA + 1
	for i, f := range files {
		lbas[i] = next
		next += max(1, (len(f.Data)+isoSector-1)/isoSector)
	}

	record := func(name []byte, lba, size int, flags byte) []byte {
		n := 33 + len(name)
		n += n % 2 // the record length is even
		r := make([]byte, n)
		r[0], r[25], r[32] = byte(n), flags, byte(len(name))
		putBoth32(r[2:], lba)
		putBoth32(r[10:], size)
		r[18], r[19], r[20] = 126, 1, 1 // 2026-01-01
		r[28], r[31] = 1, 1             // volume sequence number
		copy(r[33:], name)
		return r
	}
	var root []byte
	root = append(root, record([]byte{0}, rootLBA, isoSector, 2)...)
	root = append(root, record([]byte{1}, rootLBA, isoSector, 2)...)
	for i, f := range files {
		root = append(root, record([]byte(f.Name+";1"), lbas[i], len(f.Data), 0)...)
	}

	img := make([]byte, next*isoSector)
	pvd := img[16*isoSector:]
	pvd[0] = 1
	copy(pvd[1:], "CD001")
	pvd[6] = 1
	copy(pvd[8:], bytes.Repeat([]byte{' '}, 64))
	copy(pvd[40:], "TESTIMG"+strings.Repeat(" ", 25))
	putBoth32(pvd[80:], next) // volume space size
	putBoth16(pvd[120:], 1)   // volume set size
	putBoth16(pvd[124:], 1)   // volume sequence number
	putBoth16(pvd[128:], isoSector)
	copy(pvd[156:], record([]byte{0}, rootLBA, isoSector, 2)[:34])
	pvd[881] = 1
	term := img[17*isoSector:]
	term[0] = 255
	copy(term[1:], "CD001")
	term[6] = 1
	copy(img[rootLBA*isoSector:], root)
	for i, f := range files {
		copy(img[lbas[i]*isoSector:], f.Data)
	}
	return img
}

func putBoth32(b []byte, v int) {
	binary.LittleEndian.PutUint32(b, uint32(v))
	binary.BigEndian.PutUint32(b[4:], uint32(v))
}

func putBoth16(b []byte, v int) {
	binary.LittleEndian.PutUint16(b, uint16(v))
	binary.BigEndian.PutUint16(b[2:], uint16(v))
}

// WriteISO writes data to dir/image.iso and returns its path.
func WriteISO(t testing.TB, dir string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, "image.iso")
	write(t, path, data)
	return path
}
