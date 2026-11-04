package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lcmps/chdman-go/internal/chdwriter"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// invoke runs the CLI and returns its exit code, stdout and stderr.
func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantCode(t *testing.T, want int, args ...string) (string, string) {
	t.Helper()
	code, out, errOut := invoke(t, args...)
	if code != want {
		t.Fatalf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errOut)
	}
	return out, errOut
}

func makeDiscCue(t *testing.T, dir string) string {
	return testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 30, Shape: testimg.Mixed},
	}})
}

func TestCommandsEndToEnd(t *testing.T) {
	t.Run("dvd", func(t *testing.T) {
		dir := t.TempDir()
		iso := testimg.ISO(t, dir, 64, testimg.Mixed)
		chd := filepath.Join(dir, "out.chd")
		back := filepath.Join(dir, "back.iso")
		wantCode(t, 0, "createdvd", "-i", iso, "-o", chd)
		wantCode(t, 0, "verify", "-i", chd)
		wantCode(t, 0, "extractdvd", "-i", chd, "-o", back)
		if !bytes.Equal(mustRead(t, iso), mustRead(t, back)) {
			t.Error("extracted ISO differs")
		}
	})
	t.Run("cd", func(t *testing.T) {
		dir := t.TempDir()
		cue := makeDiscCue(t, dir)
		chd := filepath.Join(dir, "out.chd")
		wantCode(t, 0, "createcd", "-i", cue, "-o", chd)
		wantCode(t, 0, "verify", "-i", chd)

		single := filepath.Join(dir, "single")
		os.Mkdir(single, 0o755)
		wantCode(t, 0, "extractcd", "-i", chd, "-o", filepath.Join(single, "d.cue"), "-ob", filepath.Join(single, "all.bin"))
		if !bytes.Equal(mustRead(t, filepath.Join(single, "all.bin")), mustRead(t, filepath.Join(dir, "disc.bin"))) {
			t.Error("extracted bin differs")
		}

		split := filepath.Join(dir, "split")
		os.Mkdir(split, 0o755)
		wantCode(t, 0, "extractcd", "-i", chd, "-o", filepath.Join(split, "d.cue"), "-sb")
		entries, _ := os.ReadDir(split)
		if len(entries) != 3 {
			t.Errorf("split output has %d files, want cue plus 2 bins", len(entries))
		}
	})
}

func TestShortAndLongFlags(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 64, testimg.Mixed)
	short := filepath.Join(dir, "short.chd")
	long := filepath.Join(dir, "long.chd")
	wantCode(t, 0, "createdvd", "-i", iso, "-o", short, "-c", "zlib", "-hs", "8192", "-np", "2")
	wantCode(t, 0, "createdvd", "--input", iso, "--output", long, "--compression", "zlib", "--hunksize", "8192", "--numprocessors", "2")
	_, infoShort, _ := invoke(t, "info", "-i", short)
	_, infoLong, _ := invoke(t, "info", "--input", long)
	strip := func(s, p string) string { return strings.ReplaceAll(s, p, "FILE") }
	if strip(infoShort, short) != strip(infoLong, long) {
		t.Errorf("info differs:\n%s\n%s", infoShort, infoLong)
	}
	if !strings.Contains(infoShort, "8,192 bytes") && !strings.Contains(infoShort, "8192") {
		t.Errorf("hunk size not applied:\n%s", infoShort)
	}
	// -f / --force
	wantCode(t, 1, "createdvd", "-i", iso, "-o", short)
	wantCode(t, 0, "createdvd", "-i", iso, "-o", short, "-f")
	wantCode(t, 0, "createdvd", "-i", iso, "-o", short, "--force")

	cue := makeDiscCue(t, dir)
	cd := filepath.Join(dir, "cd.chd")
	wantCode(t, 0, "createcd", "-i", cue, "-o", cd)
	for i, flags := range [][]string{
		{"-ob", "x1.bin"}, {"--outputbin", "x2.bin"},
	} {
		out := filepath.Join(dir, "o"+string(rune('a'+i))+".cue")
		bin := filepath.Join(dir, flags[1])
		wantCode(t, 0, "extractcd", "-i", cd, "-o", out, flags[0], bin)
		if _, err := os.Stat(bin); err != nil {
			t.Errorf("%s: %v", flags[0], err)
		}
	}
	for i, f := range []string{"-sb", "--splitbin"} {
		sub := filepath.Join(dir, "s"+string(rune('a'+i)))
		os.Mkdir(sub, 0o755)
		wantCode(t, 0, "extractcd", "-i", cd, "-o", filepath.Join(sub, "d.cue"), f)
		if _, err := os.Stat(filepath.Join(sub, "d (Track 1).bin")); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestVerifyExitCodes(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 64, testimg.Mixed)
	chd := filepath.Join(dir, "a.chd")
	wantCode(t, 0, "createdvd", "-i", iso, "-o", chd)
	out, _ := wantCode(t, 0, "verify", "-i", chd)
	if !strings.Contains(out, "successful") {
		t.Errorf("stdout %q", out)
	}

	// Corrupt the stored raw SHA-1 in the header (offset 64).
	bad := filepath.Join(dir, "bad.chd")
	b := mustRead(t, chd)
	b[64] ^= 0xff
	if err := os.WriteFile(bad, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut := wantCode(t, 1, "verify", "-i", bad)
	if !strings.Contains(errOut, "header") || !strings.Contains(errOut, "actual") {
		t.Errorf("mismatch stderr %q", errOut)
	}

	plain := filepath.Join(dir, "plain.chd")
	wantCode(t, 0, "createdvd", "-i", iso, "-o", plain, "-c", "none")
	out, _ = wantCode(t, 2, "verify", "-i", plain)
	if !strings.Contains(out, "not verifiable") {
		t.Errorf("stdout %q", out)
	}

	wantCode(t, 1, "verify", "-i", filepath.Join(dir, "missing.chd"))
}

func TestWarningsPrinted(t *testing.T) {
	dir := t.TempDir()
	bin := testimg.Bin(t, dir, "disc.bin", testimg.Track{Mode: "MODE1/2352", Frames: 30, Shape: testimg.Mixed})
	f, err := os.OpenFile(bin, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{1, 2, 3})
	f.Close()
	cue := testimg.WriteCue(t, dir, "d.cue", "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
	_, errOut := wantCode(t, 0, "createcd", "-i", cue, "-o", filepath.Join(dir, "d.chd"))
	if !strings.Contains(strings.ToLower(errOut), "warning") || !strings.Contains(errOut, "track 1") {
		t.Errorf("stderr %q", errOut)
	}
}

func TestInfoOutput(t *testing.T) {
	dir := t.TempDir()
	// 73 frames: tracks of 40 and 33 are padded to 40 and 36, 76 frames, where
	// padding to a whole hunk of 8 would give 80.
	cue := testimg.Cue(t, dir, testimg.Disc{Tracks: []testimg.Track{
		{Mode: "MODE1/2352", Frames: 40, Shape: testimg.Mixed},
		{Mode: "AUDIO", Frames: 33, Shape: testimg.Mixed},
	}})
	chd := filepath.Join(dir, "d.chd")
	wantCode(t, 0, "createcd", "-i", cue, "-o", chd, "-c", "cdzl", "-hs", "19584")
	out, _ := wantCode(t, 0, "info", "-i", chd)
	for _, want := range []string{
		"File Version: 5", "Logical size: 186,048 bytes", "Hunk Size:    19,584 bytes",
		"Compression:  cdzl", "SHA1:", "Data SHA1:",
		"TRACK:1 TYPE:MODE1_RAW FRAMES:40", "TRACK:2 TYPE:AUDIO FRAMES:33",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("info lacks %q:\n%s", want, out)
		}
	}
}

// Track metadata that cannot be read is reported, not shown as no tracks.
func TestInfoReportsCorruptTracks(t *testing.T) {
	chd := filepath.Join(t.TempDir(), "dup.chd")
	f, err := os.Create(chd)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	track := []byte("TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0\x00")
	_, _, err = chdwriter.Write(context.Background(), f, zeroSource{}, chdwriter.Options{
		HunkBytes: 19584, UnitBytes: 2448, LogicalBytes: 4 * 19584, Workers: 1,
		Metadata: []format.MetadataEntry{
			{Tag: format.TagCHT2, Flags: format.FlagChecksum, Data: track},
			{Tag: format.TagCHT2, Flags: format.FlagChecksum, Data: track},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := wantCode(t, 1, "info", "-i", chd)
	if !strings.Contains(errOut, "track number 1 appears twice") {
		t.Errorf("stderr %q does not report the metadata", errOut)
	}
	if strings.Contains(out, "TRACK:") || !strings.Contains(out, "File Version: 5") {
		t.Errorf("stdout:\n%s", out)
	}
}

type zeroSource struct{}

func (zeroSource) ReadHunk(_ uint32, dst []byte) error { clear(dst); return nil }

func TestUnknownCommand(t *testing.T) {
	_, errOut := wantCode(t, 1, "frobnicate")
	if !strings.Contains(errOut, "Usage") {
		t.Errorf("stderr %q", errOut)
	}
	wantCode(t, 1)
}

func TestRefusesRejectedCommands(t *testing.T) {
	for _, c := range []string{"createhd", "copy"} {
		_, errOut := wantCode(t, 1, c, "-i", "x")
		for _, name := range []string{"createcd", "createdvd", "extractcd", "extractdvd", "verify", "info"} {
			if !strings.Contains(errOut, name) {
				t.Errorf("%s: stderr lacks %q: %q", c, name, errOut)
			}
		}
	}
}

// timeout, systemd and kill all stop a process with SIGTERM, and a run that
// is cancelled removes its temporary files, so SIGTERM must cancel like ^C.
func TestSignalsCancel(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := notifyContext(context.Background())
			defer stop()
			p, err := os.FindProcess(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Signal(sig); err != nil {
				t.Skipf("cannot send %v: %v", sig, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%v did not cancel the context", sig)
			}
		})
	}
}

// chdman rejects an empty -c, and reads only the first four names of a longer
// one.
func TestCompressionOption(t *testing.T) {
	dir := t.TempDir()
	cue := makeDiscCue(t, dir)
	out := filepath.Join(dir, "o.chd")
	if _, errOut := wantCode(t, 1, "createcd", "-i", cue, "-o", out, "-c", ""); !strings.Contains(errOut, "invalid compressor") {
		t.Errorf("empty -c: stderr %q", errOut)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("empty -c left an output")
	}
	wantCode(t, 0, "createcd", "-i", cue, "-o", out, "-c", "cdzl,cdlz,cdfl,cdzs,cdzl")
	wantCode(t, 0, "createcd", "-f", "-i", cue, "-o", out, "-c", "cdzl,cdlz,cdfl,cdzs,bogus")
}
