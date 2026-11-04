package chdman

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// marker returns recognisable 2048-byte user data for sector i.
func marker(i int) []byte {
	return bytes.Repeat([]byte{byte(0xa0 + i), byte(i), 'M'}, 2048/3+1)[:2048]
}

func markers(n int) []byte {
	var b []byte
	for i := range n {
		b = append(b, marker(i)...)
	}
	return b
}

func TestReadUserDataOffsets(t *testing.T) {
	for _, mode := range []string{"MODE1/2048", "MODE1/2352", "MODE2/2352", "MODE2/2336"} {
		t.Run(strings.ReplaceAll(mode, "/", "-"), func(t *testing.T) {
			_, chd := makeCDChd(t, testimg.Disc{Tracks: []testimg.Track{
				{Mode: mode, Frames: 20, Shape: testimg.Zeros, Data: markers(20)},
			}})
			r := openT(t, chd)
			dst := make([]byte, 2048)
			for i := range 20 {
				n, err := r.ReadUserData(uint32(i), dst, ReadOptions{})
				if err != nil || n != 2048 {
					t.Fatalf("lba %d: n=%d err=%v", i, n, err)
				}
				if !bytes.Equal(dst, marker(i)) {
					t.Fatalf("lba %d: payload mismatch", i)
				}
			}
		})
	}
}

func TestReadSectorSizes(t *testing.T) {
	want := map[string]int{"MODE1/2048": 2048, "MODE1/2352": 2352, "MODE2/2352": 2352, "MODE2/2336": 2336, "AUDIO": 2352}
	for mode, size := range want {
		_, chd := makeCDChd(t, testimg.Disc{Tracks: []testimg.Track{{Mode: mode, Frames: 12, Shape: testimg.Random}}})
		r := openT(t, chd)
		dst := make([]byte, 4000)
		n, err := r.ReadSector(3, dst, ReadOptions{})
		if err != nil || n != size {
			t.Errorf("%s: n=%d err=%v, want %d", mode, n, err, size)
		}
		if _, err := r.ReadSector(3, dst[:size-1], ReadOptions{}); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("%s: short dst err = %v, want a buffer error", mode, err)
		}
	}
}

func TestAudioNoUserData(t *testing.T) {
	_, chd := makeCDChd(t, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 12, Shape: testimg.Zeros},
		{Mode: "AUDIO", Frames: 12, Shape: testimg.Random},
	}})
	r := openT(t, chd)
	dst := make([]byte, 2352)
	if _, err := r.ReadUserData(12, dst, ReadOptions{}); !errors.Is(err, ErrNoUserData) {
		t.Fatalf("audio err = %v, want ErrNoUserData", err)
	}
	// ReadSector still works and returns the stored bytes untouched.
	if n, err := r.ReadSector(12, dst, ReadOptions{}); err != nil || n != 2352 {
		t.Fatalf("ReadSector: %d, %v", n, err)
	}
}

func TestUserDataOffsetTable(t *testing.T) {
	// MODE2/2324 cannot be made through a cue sheet generator here, so the
	// mode-to-offset table is checked directly.
	for _, typ := range []cdrom.TrackType{cdrom.MODE2_FORM2, cdrom.AUDIO} {
		if _, ok := cdrom.UserDataOffset(typ); ok {
			t.Errorf("%v has user data", typ)
		}
	}
	for typ, want := range map[cdrom.TrackType]int{
		cdrom.MODE1: 0, cdrom.MODE1_RAW: 16, cdrom.MODE2: 8, cdrom.MODE2_FORM1: 0,
		cdrom.MODE2_FORM_MIX: 8, cdrom.MODE2_RAW: 24,
	} {
		if got, ok := cdrom.UserDataOffset(typ); !ok || got != want {
			t.Errorf("%v: offset %d ok=%v, want %d", typ, got, ok, want)
		}
	}
}

func TestLogicalVsPhysical(t *testing.T) {
	cases := []struct {
		name string
		disc testimg.Disc
		// track 2 INDEX 01: logical start and the physical frame of INDEX 00
		wantStart  uint32
		physIndex0 uint32
		pregap     uint32
		stored     bool // pregap is stored in the bin
	}{
		{"index00", testimg.Disc{Tracks: []testimg.Track{
			{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Random},
			{Mode: "AUDIO", Frames: 30, IndexZero: 10, Shape: testimg.Random},
		}}, 50, 40, 10, true},
		{"pregap-not-in-file", testimg.Disc{Tracks: []testimg.Track{
			{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Random},
			{Mode: "AUDIO", Frames: 30, Pregap: 150, Shape: testimg.Random},
		}}, 190, 40, 150, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cue := testimg.Cue(t, dir, tc.disc)
			bin, err := os.ReadFile(filepath.Join(dir, "disc.bin"))
			if err != nil {
				t.Fatal(err)
			}
			// The CHD stores audio big-endian, a bin file little-endian, so the
			// expected bytes of an audio frame are the bin's with each pair
			// swapped. Track 1 is 40 frames of data.
			frame := func(n uint32) []byte {
				f := bytes.Clone(bin[n*2352 : (n+1)*2352])
				if n >= 40 {
					for i := 0; i < len(f); i += 2 {
						f[i], f[i+1] = f[i+1], f[i]
					}
				}
				return f
			}
			chd := filepath.Join(t.TempDir(), "d.chd")
			if _, err := CreateCD(context.Background(), cue, chd, CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			r := openT(t, chd)

			tr, err := r.Tracks()
			if err != nil {
				t.Fatal(err)
			}
			if len(tr) != 2 || tr[1].StartLBA != tc.wantStart || tr[1].Pregap != int32(tc.pregap) {
				t.Fatalf("Tracks = %+v", tr)
			}
			dst := make([]byte, 2352)
			read := func(lba uint32, phys bool) []byte {
				t.Helper()
				n, err := r.ReadSector(lba, dst, ReadOptions{Physical: phys})
				if err != nil || n != 2352 {
					t.Fatalf("ReadSector(%d, phys=%v) = %d, %v", lba, phys, n, err)
				}
				return bytes.Clone(dst)
			}

			// INDEX 01 of track 2 is the first sector after the pregap; the
			// bin holds it at frame 40+10 or, with no stored pregap, 40.
			index1Frame := tc.physIndex0
			if tc.stored {
				index1Frame += tc.pregap
			}
			if got := read(tc.wantStart, false); !bytes.Equal(got, frame(index1Frame)) {
				t.Error("logical INDEX 01 differs from the bin")
			}
			if tc.stored {
				if got := read(tc.physIndex0+tc.pregap, true); !bytes.Equal(got, frame(index1Frame)) {
					t.Error("physical index 0 + pregap differs from the bin")
				}
			}
			if got := read(tc.physIndex0, true); !bytes.Equal(got, frame(tc.physIndex0)) {
				t.Error("physical index 0 differs from the bin")
			}
			if tc.stored {
				// Logical pregap sectors are the stored INDEX 00 frames.
				for i := range tc.pregap {
					if got := read(tc.physIndex0+i, false); !bytes.Equal(got, frame(tc.physIndex0+i)) {
						t.Fatalf("logical pregap frame %d differs from the bin", i)
					}
				}
			} else {
				zero := make([]byte, 2352)
				for i := range tc.pregap {
					if got := read(tc.physIndex0+i, false); !bytes.Equal(got, zero) {
						t.Fatalf("logical pregap sector %d is not zeros", tc.physIndex0+i)
					}
				}
			}
			// The last frame of the disc, both ways.
			last := uint32(len(bin)/2352 - 1)
			if got := read(last, true); !bytes.Equal(got, frame(last)) {
				t.Error("last physical frame differs from the bin")
			}
			if got := read(tc.wantStart+29-lastOff(tc.stored, tc.pregap), false); !bytes.Equal(got, frame(last)) {
				t.Error("last logical frame differs from the bin")
			}
			if _, err := r.ReadSector(tc.wantStart+30-lastOff(tc.stored, tc.pregap), dst, ReadOptions{}); !errors.Is(err, io.EOF) {
				t.Errorf("one past the end: err = %v, want io.EOF", err)
			}
		})
	}
}

// lastOff converts "frames after INDEX 01" into the track's own length: with a
// stored pregap the 30 frames include the 10 of INDEX 00.
func lastOff(stored bool, pregap uint32) uint32 {
	if stored {
		return pregap
	}
	return 0
}

func TestReadUserDataDVD(t *testing.T) {
	chd, iso := makeDVD(t, testimg.Mixed, 300, "", 0)
	r := openT(t, chd)
	if tracks, err := r.Tracks(); tracks != nil || err != nil {
		t.Errorf("Tracks on a DVD = %v, %v; want nil, nil", tracks, err)
	}
	dst := make([]byte, 2048)
	for _, lba := range []uint32{0, 1, 17, 150, 299} {
		for name, read := range map[string]func(uint32, []byte, ReadOptions) (int, error){"ReadUserData": r.ReadUserData, "ReadSector": r.ReadSector} {
			n, err := read(lba, dst, ReadOptions{})
			if err != nil || n != 2048 || !bytes.Equal(dst, iso[int(lba)*2048:int(lba+1)*2048]) {
				t.Fatalf("%s(%d): n=%d err=%v", name, lba, n, err)
			}
		}
	}
	if _, err := r.ReadUserData(300, dst, ReadOptions{}); !errors.Is(err, io.EOF) {
		t.Errorf("past end err = %v, want io.EOF", err)
	}
}

func TestReadSectorPastEnd(t *testing.T) {
	_, chd := makeCDChd(t, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 41, Shape: testimg.Random},
		{Mode: "AUDIO", Frames: 30, Postgap: 5, Shape: testimg.Random},
	}})
	r := openT(t, chd)
	dst := make([]byte, 2352)
	for _, phys := range []bool{false, true} {
		for _, lba := range []uint32{500, 1 << 20, math.MaxUint32 - 1, math.MaxUint32} {
			if _, err := r.ReadSector(lba, dst, ReadOptions{Physical: phys}); !errors.Is(err, io.EOF) {
				t.Errorf("CD lba %d phys=%v: err = %v, want io.EOF", lba, phys, err)
			}
			if _, err := r.ReadUserData(lba, dst, ReadOptions{Physical: phys}); !errors.Is(err, io.EOF) {
				t.Errorf("CD user data lba %d phys=%v: err = %v, want io.EOF", lba, phys, err)
			}
		}
	}
	// The padding frames of track 1 (41 frames are stored as 44) are not
	// addressable, and the frame before the end still reads.
	if _, err := r.ReadSector(41+30-1, dst, ReadOptions{Physical: true}); err != nil {
		t.Errorf("last physical frame: %v", err)
	}
	if _, err := r.ReadSector(41+30, dst, ReadOptions{Physical: true}); !errors.Is(err, io.EOF) {
		t.Errorf("one past last physical frame: err = %v", err)
	}
	// An empty destination is a clear error, not a panic.
	if _, err := r.ReadSector(0, nil, ReadOptions{}); err == nil {
		t.Error("nil dst: no error")
	}
	if _, err := r.ReadUserData(0, make([]byte, 2047), ReadOptions{}); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("2047-byte dst: err = %v", err)
	}
}

func TestReadSectorUnsupported(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(raw, bytes.Repeat([]byte("hd "), 512*400/3+1)[:512*400], 0o644); err != nil {
		t.Fatal(err)
	}
	chd := filepath.Join(dir, "disk.chd")
	if err := oracle.Run(t, "createhd", "-f", "-i", raw, "-o", chd); err != nil {
		t.Fatal(err)
	}
	r := openT(t, chd)
	dst := make([]byte, 2352)
	if _, err := r.ReadSector(0, dst, ReadOptions{}); !errors.Is(err, ErrUnsupportedDiscType) {
		t.Errorf("ReadSector err = %v", err)
	}
	if _, err := r.ReadUserData(0, dst, ReadOptions{}); !errors.Is(err, ErrUnsupportedDiscType) {
		t.Errorf("ReadUserData err = %v", err)
	}
	if tracks, err := r.Tracks(); tracks != nil || !errors.Is(err, ErrUnsupportedDiscType) {
		t.Errorf("Tracks on a hard disk = %v, %v; want nil, ErrUnsupportedDiscType", tracks, err)
	}
}

// findFile walks the ISO9660 root directory through read, which returns the
// 2048-byte user data of a logical sector, and returns the file's content.
func findFile(t *testing.T, read func(lba uint32) []byte, name string) []byte {
	t.Helper()
	pvd := read(16)
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatalf("no primary volume descriptor at 16: % x", pvd[:8])
	}
	rec := pvd[156:190]
	dirLBA := binary.LittleEndian.Uint32(rec[2:])
	dirLen := binary.LittleEndian.Uint32(rec[10:])
	dir := read(dirLBA)[:dirLen]
	for off := 0; off < len(dir) && dir[off] != 0; off += int(dir[off]) {
		rec := dir[off : off+int(dir[off])]
		if got := string(rec[33 : 33+int(rec[32])]); got == name+";1" {
			lba := binary.LittleEndian.Uint32(rec[2:])
			size := binary.LittleEndian.Uint32(rec[10:])
			var out []byte
			for s := uint32(0); s*2048 < size; s++ {
				out = append(out, read(lba+s)...)
			}
			return out[:size]
		}
	}
	t.Fatalf("%s not found in the root directory", name)
	return nil
}

func TestFilesystemReachable(t *testing.T) {
	cnf := []byte("BOOT2 = cdrom0:\\SLUS_200.62;1\r\nVER = 1.00\r\nVMODE = NTSC\r\n")
	big := bytes.Repeat([]byte("PSP_GAME "), 700) // spans several sectors
	img := testimg.Filesystem(testimg.File{Name: "SYSTEM.CNF", Data: cnf}, testimg.File{Name: "PARAM.SFO", Data: big})
	sectors := len(img) / 2048

	check := func(t *testing.T, r *Reader) {
		dst := make([]byte, 2352)
		read := func(lba uint32) []byte {
			t.Helper()
			n, err := r.ReadUserData(lba, dst, ReadOptions{})
			if err != nil || n != 2048 {
				t.Fatalf("ReadUserData(%d) = %d, %v", lba, n, err)
			}
			return bytes.Clone(dst[:n])
		}
		if got := findFile(t, read, "SYSTEM.CNF"); !bytes.Equal(got, cnf) {
			t.Errorf("SYSTEM.CNF = %q", got)
		}
		if got := findFile(t, read, "PARAM.SFO"); !bytes.Equal(got, big) {
			t.Error("PARAM.SFO differs")
		}
	}

	t.Run("CD-MODE2-2352", func(t *testing.T) {
		_, chd := makeCDChd(t, testimg.Disc{Tracks: []testimg.Track{
			{Mode: "MODE2/2352", Frames: sectors + 4, Shape: testimg.Zeros, Data: img},
		}})
		check(t, openT(t, chd))
	})
	// The filesystem is on the first track, and later tracks, a pregap, a
	// stored index 0 and padding frames come after it. Each shape is also
	// built with one bin per track.
	audio := func(frames int, o ...func(*testimg.Track)) testimg.Track {
		tr := testimg.Track{Mode: "AUDIO", Frames: frames, Shape: testimg.Mixed}
		for _, f := range o {
			f(&tr)
		}
		return tr
	}
	for _, mode := range []string{"MODE1/2048", "MODE1/2352", "MODE2/2336", "MODE2/2352"} {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("CD-%s-multitrack/split=%v", strings.ReplaceAll(mode, "/", "-"), split), func(t *testing.T) {
				_, chd := makeCDChd(t, testimg.Disc{Split: split, Tracks: []testimg.Track{
					{Mode: mode, Frames: sectors + 8, Shape: testimg.Zeros, Data: img},
					audio(90, func(tr *testimg.Track) { tr.IndexZero = 15 }),
					audio(91, func(tr *testimg.Track) { tr.Pregap = 15 }),
				}})
				check(t, openT(t, chd))
			})
		}
	}
	t.Run("DVD", func(t *testing.T) {
		dir := t.TempDir()
		iso := testimg.WriteISO(t, dir, img)
		chd := filepath.Join(dir, "d.chd")
		if _, err := CreateDVD(context.Background(), iso, chd, CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		check(t, openT(t, chd))
	})
}
