package chdman

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func chdInfo(t *testing.T, path string) Info {
	t.Helper()
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer r.Close()
	return r.Info()
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestCreateDVDHashParity(t *testing.T) {
	oracle.Require(t)
	tests := []struct {
		name   string
		opts   CreateOptions
		codecs string
		hs     int
	}{
		{"default", CreateOptions{}, "", 0},
		{"hunk8192", CreateOptions{HunkBytes: 8192}, "", 8192},
		{"zlib", CreateOptions{Codecs: []string{"zlib"}}, "zlib", 0},
		{"workers1", CreateOptions{Workers: 1}, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			iso := testimg.ISO(t, dir, 16, testimg.Mixed)
			want := filepath.Join(dir, "want.chd")
			oracle.CreateDVD(t, iso, want, tc.codecs, tc.hs)

			got := filepath.Join(dir, "got.chd")
			res, err := CreateDVD(context.Background(), iso, got, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			w, g := chdInfo(t, want), chdInfo(t, got)
			if res.RawSHA1 != w.RawSHA1 || res.SHA1 != w.SHA1 {
				t.Errorf("result hashes %s/%s, chdman %s/%s", res.RawSHA1, res.SHA1, w.RawSHA1, w.SHA1)
			}
			if g.RawSHA1 != w.RawSHA1 || g.SHA1 != w.SHA1 {
				t.Errorf("stored hashes %s/%s, chdman %s/%s", g.RawSHA1, g.SHA1, w.RawSHA1, w.SHA1)
			}
			if g.Disc != DiscDVD || g.UnitBytes != 2048 {
				t.Errorf("disc %v unit %d", g.Disc, g.UnitBytes)
			}
			if err := oracle.Verify(t, got); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestCreateDVDCodecsNone(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 16, testimg.Mixed)
	out := filepath.Join(dir, "out.chd")
	if _, err := CreateDVD(context.Background(), iso, out, CreateOptions{Codecs: []string{"none"}}); err != nil {
		t.Fatal(err)
	}
	if got := chdInfo(t, out).Codecs; !slices.Equal(got, []string{"none"}) {
		t.Errorf("codecs %v", got)
	}
	if err := oracle.Verify(t, out); err != nil {
		t.Error(err)
	}
}

func TestCreateDVDProgress(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 16, testimg.Mixed)
	var last, total uint64
	_, err := CreateDVD(context.Background(), iso, filepath.Join(dir, "o.chd"), CreateOptions{
		Progress: func(done, tot uint64) {
			if done < last {
				t.Errorf("progress went back: %d after %d", done, last)
			}
			last, total = done, tot
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if last != 16*2048 || total != 16*2048 {
		t.Errorf("final progress %d/%d", last, total)
	}
}

func TestCreateDVDRejectsBadSize(t *testing.T) {
	dir := t.TempDir()
	iso := filepath.Join(dir, "bad.iso")
	if err := os.WriteFile(iso, make([]byte, 2047), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.chd")
	if _, err := CreateDVD(context.Background(), iso, out, CreateOptions{}); err == nil {
		t.Fatal("expected an error")
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"bad.iso"}) {
		t.Errorf("directory holds %v", got)
	}
}

func TestCreateDVDRejectsBadOptions(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 4, testimg.Zeros)
	for name, opts := range map[string]CreateOptions{
		"unknown codec": {Codecs: []string{"nope"}},
		"hunk size":     {HunkBytes: 3000},
	} {
		if _, err := CreateDVD(context.Background(), iso, filepath.Join(dir, "o.chd"), opts); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"image.iso"}) {
		t.Errorf("directory holds %v", got)
	}
}

func TestCreateDVDSmallerThanHunk(t *testing.T) {
	oracle.Require(t)
	// chdman 0.289 sizes the compressed map's buffer without counting the
	// exported Huffman tree (chd.cpp:2175-2176), so for some files of one to
	// three hunks, whatever their content, the map tail it writes is garbled
	// and chdman cannot read its own file back. The header hashes stay
	// valid, so they are read from the raw header, skipping the map.
	for name, shape := range map[string]testimg.Shape{"zeros": testimg.Zeros, "random": testimg.Random, "mixed": testimg.Mixed} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			iso := testimg.ISO(t, dir, 1, shape)
			got := filepath.Join(dir, "got.chd")
			res, err := CreateDVD(context.Background(), iso, got, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(dir, "want.chd")
			oracle.CreateDVD(t, iso, want, "", 0)
			raw, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			h, err := format.ParseHeader(raw)
			if err != nil {
				t.Fatal(err)
			}
			if w, g := hex.EncodeToString(h.RawSHA1[:]), res.RawSHA1; w != g {
				t.Errorf("raw SHA-1 %s, chdman %s", g, w)
			}
			if w, g := hex.EncodeToString(h.SHA1[:]), res.SHA1; w != g {
				t.Errorf("SHA-1 %s, chdman %s", g, w)
			}
			data, _ := os.ReadFile(iso)
			if sum := sha1.Sum(data); res.RawSHA1 != hex.EncodeToString(sum[:]) {
				t.Errorf("raw SHA-1 %s is not the SHA-1 of the data", res.RawSHA1)
			}
			if err := oracle.Verify(t, got); err != nil {
				t.Errorf("chdman rejects our file: %v", err)
			}
		})
	}
}

func TestCreateDVDZeroByte(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	iso := filepath.Join(dir, "empty.iso")
	if err := os.WriteFile(iso, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// chdman 0.289 fails ("Input start offset is beyond end of input") and
	// leaves no output.
	want := filepath.Join(dir, "want.chd")
	if err := oracle.Run(t, "createdvd", "-f", "-i", iso, "-o", want); err == nil {
		t.Fatal("chdman now accepts a zero-byte ISO; revisit CreateDVD")
	}
	if _, err := os.Stat(want); err == nil {
		t.Fatal("chdman left an output file")
	}
	got := filepath.Join(dir, "got.chd")
	if _, err := CreateDVD(context.Background(), iso, got, CreateOptions{}); err == nil {
		t.Error("expected an error")
	}
	if _, err := os.Stat(got); err == nil {
		t.Error("CreateDVD left an output file")
	}
}

func TestCreateDVDOver4GiB(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 5 GiB sparse file")
	}
	oracle.Require(t)
	start := time.Now()
	dir := t.TempDir()
	iso := filepath.Join(dir, "big.iso")
	f, err := os.Create(iso)
	if err != nil {
		t.Fatal(err)
	}
	const size = 5 << 30
	// Large hunks keep the run short; the point is offsets and sizes past
	// 32 bits, which do not depend on the hunk size.
	const hunk = 1 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	want := filepath.Join(dir, "want.chd")
	oracle.CreateDVD(t, iso, want, "zlib", hunk)

	stop := make(chan struct{})
	peak := make(chan uint64)
	go func() {
		var m runtime.MemStats
		var hi uint64
		for tick := time.NewTicker(50 * time.Millisecond); ; {
			select {
			case <-tick.C:
				runtime.ReadMemStats(&m)
				hi = max(hi, m.HeapInuse)
			case <-stop:
				tick.Stop()
				peak <- hi
				return
			}
		}
	}()
	got := filepath.Join(dir, "got.chd")
	res, err := CreateDVD(context.Background(), iso, got, CreateOptions{Codecs: []string{"zlib"}, HunkBytes: hunk})
	close(stop)
	heap := <-peak
	if err != nil {
		t.Fatal(err)
	}
	if heap > 512<<20 {
		t.Errorf("heap in use peaked at %d MiB", heap>>20)
	}
	w := chdInfo(t, want)
	if res.RawSHA1 != w.RawSHA1 || res.SHA1 != w.SHA1 {
		t.Errorf("hashes %s/%s, chdman %s/%s", res.RawSHA1, res.SHA1, w.RawSHA1, w.SHA1)
	}
	if g := chdInfo(t, got); g.LogicalBytes != size {
		t.Errorf("logical bytes %d", g.LogicalBytes)
	}
	t.Logf("5 GiB run took %v, peak heap %d MiB", time.Since(start), heap>>20)
}

func TestCreateCancelLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 8192, testimg.Mixed)
	out := filepath.Join(dir, "out.chd")
	old := []byte("existing output")
	if err := os.WriteFile(out, old, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := CreateDVD(ctx, iso, out, CreateOptions{Force: true, Progress: func(_, _ uint64) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, old) {
		t.Error("existing output was modified")
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"image.iso", "out.chd"}) {
		t.Errorf("directory holds %v", got)
	}

	// Without an existing output, nothing remains at all.
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if _, err := CreateDVD(ctx, iso, out, CreateOptions{Progress: func(_, _ uint64) { cancel() }}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"image.iso"}) {
		t.Errorf("directory holds %v", got)
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 4, testimg.Mixed)
	out := filepath.Join(dir, "out.chd")
	old := []byte("keep me")
	if err := os.WriteFile(out, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateDVD(context.Background(), iso, out, CreateOptions{}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want os.ErrExist", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, old) {
		t.Error("existing output was modified")
	}
	if _, err := CreateDVD(context.Background(), iso, out, CreateOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if chdInfo(t, out).Disc != DiscDVD {
		t.Error("Force did not replace the output")
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"image.iso", "out.chd"}) {
		t.Errorf("directory holds %v", got)
	}
}

func TestCreateRefusesOutputEqualsInput(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 4, testimg.Mixed)
	orig, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.chd")
	if err := os.Symlink(iso, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	hard := filepath.Join(dir, "hard.chd")
	if err := os.Link(iso, hard); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{iso, link, hard} {
		for _, force := range []bool{false, true} {
			if _, err := CreateDVD(context.Background(), iso, out, CreateOptions{Force: force}); err == nil {
				t.Errorf("%s force=%v: expected an error", filepath.Base(out), force)
			}
		}
	}
	if got, _ := os.ReadFile(iso); !bytes.Equal(got, orig) {
		t.Error("input was modified")
	}
}

func TestParseCodecs(t *testing.T) {
	z, l := format.CodecZlib, format.CodecLZMA
	ok := []struct {
		in   []string
		want [4]format.CodecTag
	}{
		{nil, [4]format.CodecTag{}},
		{[]string{"zlib"}, [4]format.CodecTag{z}},
		{[]string{"lzma", "zlib"}, [4]format.CodecTag{l, z}},
		{[]string{"lzma", "zlib", "huff", "flac"}, [4]format.CodecTag{l, z, format.CodecHuffman, format.CodecFLAC}},
		{[]string{"cdlz", "cdzl", "cdfl", "cdzs"}, [4]format.CodecTag{format.CodecCDLZMA, format.CodecCDZlib, format.CodecCDFLAC, format.CodecCDZstd}},
		{[]string{"none"}, [4]format.CodecTag{}},
		// chdman reads the first four names and never looks at the rest.
		{[]string{"lzma", "zlib", "huff", "flac", "zstd"}, [4]format.CodecTag{l, z, format.CodecHuffman, format.CodecFLAC}},
		{[]string{"lzma", "zlib", "huff", "flac", "not a codec", ""}, [4]format.CodecTag{l, z, format.CodecHuffman, format.CodecFLAC}},
	}
	defaults := [4]format.CodecTag{format.CodecHuffman}
	for _, tc := range ok {
		got, err := parseCodecs(tc.in, defaults)
		if err != nil {
			t.Errorf("%v: %v", tc.in, err)
			continue
		}
		want := tc.want
		if tc.in == nil {
			want = defaults
		}
		if got != want {
			t.Errorf("%v: got %v, want %v", tc.in, got, want)
		}
	}
	bad := [][]string{
		{"nope"}, {"zl"}, {""}, {"zlib", ""}, {"none", "zlib"}, {"zlib", "none"},
		{"avhu"}, {"lzma", "zlib", "huff", "", "flac"},
	}
	for _, in := range bad {
		if _, err := parseCodecs(in, defaults); err == nil {
			t.Errorf("%v: expected an error", in)
		}
	}
}

func TestParseHunkBytes(t *testing.T) {
	tests := []struct {
		hs, gran, def uint32
		want          uint32
		ok            bool
	}{
		{0, 2048, 4096, 4096, true},
		{8192, 2048, 4096, 8192, true},
		{2048, 2048, 4096, 2048, true},
		{1 << 20, 2048, 4096, 1 << 20, true},
		{2048 * 1024, 2048, 4096, 0, false}, // above the maximum
		{3000, 2048, 4096, 0, false},        // not a multiple
		{8, 8, 4096, 0, false},              // below the minimum of 16
		{16, 8, 4096, 16, true},
		{4896, 2448, 19584, 4896, true},
	}
	for _, tc := range tests {
		got, err := parseHunkBytes(tc.hs, tc.gran, tc.def)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseHunkBytes(%d, %d, %d) = %d, %v", tc.hs, tc.gran, tc.def, got, err)
		}
	}
}

func TestCreateForceKeepsModeAndReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 4, testimg.Mixed)
	out := filepath.Join(dir, "out.chd")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateDVD(context.Background(), iso, out, CreateOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(out); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode after Force = %v, %v; want 0600", st.Mode().Perm(), err)
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.chd")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := CreateDVD(context.Background(), iso, link, CreateOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the link was written through, not replaced: %v, %v", st.Mode(), err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Errorf("the link's target changed to %q", got)
	}
}
