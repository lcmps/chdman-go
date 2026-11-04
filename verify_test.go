package chdman

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/oracle"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

func TestVerifyOK(t *testing.T) {
	for name, chd := range map[string]string{
		"dvd": func() string { p, _ := makeDVD(t, testimg.Mixed, 300, "", 0); return p }(),
		"cd":  makeCD(t, ""),
	} {
		t.Run(name, func(t *testing.T) {
			r := openT(t, chd)
			var done, total uint64
			res, err := r.Verify(context.Background(), VerifyOptions{Progress: func(d, tot uint64) { done, total = d, tot }})
			if err != nil || res.Status != VerifyOK {
				t.Fatalf("Verify = %+v, %v", res, err)
			}
			if res.ExpectedRawSHA1 != res.ActualRawSHA1 || res.ExpectedSHA1 != res.ActualSHA1 || res.ActualSHA1 == "" {
				t.Errorf("hashes: %+v", res)
			}
			if total != r.Info().LogicalBytes || done != total {
				t.Errorf("progress ended at %d of %d", done, total)
			}
			if err := oracle.Verify(t, chd); err != nil {
				t.Errorf("chdman rejects the file: %v", err)
			}
		})
	}
}

func TestVerifyNotVerifiable(t *testing.T) {
	chd, _ := makeDVD(t, testimg.Mixed, 100, "none", 0)
	res, err := openT(t, chd).Verify(context.Background(), VerifyOptions{})
	if err != nil || res.Status != VerifyNotVerifiable {
		t.Fatalf("Verify = %+v, %v; want VerifyNotVerifiable and nil", res, err)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		chd := makeCD(t, "")
		var off int64 = -1
		r := openT(t, chd)
		for _, e := range metaOffsets(t, chd) {
			if e.Flags&format.FlagChecksum != 0 && len(e.Data) > 0 {
				off = e.dataOff
			}
		}
		r.Close()
		if off < 0 {
			t.Fatal("no checksummed metadata")
		}
		flipByte(t, chd, off)
		res, err := openT(t, chd).Verify(context.Background(), VerifyOptions{})
		if res.Status != VerifyMismatch || !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("Verify = %+v, %v; want VerifyMismatch and ErrHashMismatch", res, err)
		}
		if res.ExpectedRawSHA1 != res.ActualRawSHA1 || res.ExpectedSHA1 == res.ActualSHA1 {
			t.Errorf("only the overall hash should differ: %+v", res)
		}
	})
	t.Run("data", func(t *testing.T) {
		chd, _ := makeDVD(t, testimg.Random, 60, "zlib", 4096)
		flipByte(t, chd, int64(storedHunk(t, openT(t, chd)).Offset)+5)
		res, err := openT(t, chd).Verify(context.Background(), VerifyOptions{})
		if !errors.Is(err, ErrCorruptData) || res.Status == VerifyOK {
			t.Fatalf("Verify = %+v, %v; want an ErrCorruptData error", res, err)
		}
	})
}

type metaLoc struct {
	format.MetadataEntry
	dataOff int64
}

// metaOffsets walks the metadata chain and returns each entry's data offset.
func metaOffsets(t testing.TB, path string) []metaLoc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	var out []metaLoc
	for off := h.MetaOffset; off != 0; off = binary.BigEndian.Uint64(b[off+8:]) {
		lf := binary.BigEndian.Uint32(b[off+4:])
		n := int64(lf & 0xffffff)
		out = append(out, metaLoc{format.MetadataEntry{
			Tag: binary.BigEndian.Uint32(b[off:]), Flags: uint8(lf >> 24), Data: b[off+16 : off+16+uint64(n)],
		}, int64(off) + 16})
	}
	return out
}

func TestVerifyWorkersIrrelevant(t *testing.T) {
	chd, _ := makeDVD(t, testimg.Mixed, 500, "zstd,zlib", 4096)
	r := openT(t, chd)
	a, errA := r.Verify(context.Background(), VerifyOptions{Workers: 1})
	b, errB := r.Verify(context.Background(), VerifyOptions{Workers: 8})
	if errA != nil || errB != nil || a != b || a.Status != VerifyOK {
		t.Fatalf("1 worker: %+v, %v; 8 workers: %+v, %v", a, errA, b, errB)
	}
}

func TestVerifyCancelled(t *testing.T) {
	chd, _ := makeDVD(t, testimg.Mixed, 300, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := openT(t, chd).Verify(ctx, VerifyOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify = %v, want context.Canceled", err)
	}
}

func TestVerifyHardDisk(t *testing.T) {
	oracle.Require(t)
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.img")
	data := bytes.Repeat([]byte("hard disk sector content "), 512*400/25+1)[:512*400]
	if err := os.WriteFile(raw, data, 0o644); err != nil {
		t.Fatal(err)
	}
	chd := filepath.Join(dir, "disk.chd")
	if err := oracle.Run(t, "createhd", "-f", "-i", raw, "-o", chd); err != nil {
		t.Fatal(err)
	}
	r := openT(t, chd)
	if r.Info().Disc != DiscHD {
		t.Errorf("Disc = %v, want DiscHD", r.Info().Disc)
	}
	res, err := r.Verify(context.Background(), VerifyOptions{})
	if err != nil || res.Status != VerifyOK {
		t.Fatalf("Verify = %+v, %v", res, err)
	}
}

func TestVerifyAcceptsCHSE(t *testing.T) {
	chd := makeCD(t, "")
	b, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := format.ReadMetadata(bytes.NewReader(b), h, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	// Link a new entry after the last one in the chain.
	last := h.MetaOffset
	for next := binary.BigEndian.Uint64(b[last+8:]); next != 0; next = binary.BigEndian.Uint64(b[last+8:]) {
		last = next
	}
	binary.BigEndian.PutUint64(b[last+8:], uint64(len(b)))
	extra := format.MetadataEntry{Tag: format.TagCHSE, Flags: format.FlagChecksum, Data: []byte("session data")}
	b = append(b, format.MarshalMetadata(extra, 0)...)
	h.SHA1 = format.OverallSHA1(h.RawSHA1, append(meta, extra))
	hdr := h.Marshal()
	copy(b, hdr[:])
	path := filepath.Join(t.TempDir(), "chse.chd")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := openT(t, path).Verify(context.Background(), VerifyOptions{})
	if err != nil || res.Status != VerifyOK {
		t.Fatalf("Verify = %+v, %v", res, err)
	}
	_ = sha1.Size
}
