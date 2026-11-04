package chdman

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// failAt is an io.ReaderAt that fails reads overlapping [from, to) while on
// is set.
type failAt struct {
	b        []byte
	from, to int64
	on       atomic.Bool
	err      error
}

func newFailAt(b []byte, from, to int64, err error) *failAt {
	f := &failAt{b: b, from: from, to: to, err: err}
	f.on.Store(true)
	return f
}

func (f *failAt) ReadAt(p []byte, off int64) (int, error) {
	if f.on.Load() && off+int64(len(p)) > f.from && off < f.to {
		return 0, f.err
	}
	return bytes.NewReader(f.b).ReadAt(p, off)
}

func TestIOErrorsAreNotCorruption(t *testing.T) {
	boom := errors.New("share went away")
	chd, _ := makeDVD(t, testimg.Mixed, 64, "", 4096)
	b, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := format.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, boom) {
			t.Errorf("%s = %v, want the underlying error", what, err)
		}
		if errors.Is(err, ErrCorruptData) {
			t.Errorf("%s = %v, an I/O failure is not corruption", what, err)
		}
	}
	t.Run("open", func(t *testing.T) {
		// In this file the metadata sits between the header and the map.
		for name, span := range map[string][2]int64{
			"header":   {0, format.HeaderSize},
			"metadata": {int64(hdr.MetaOffset), int64(hdr.MapOffset)},
			"map":      {int64(hdr.MapOffset), int64(len(b))},
		} {
			_, err := NewReader(newFailAt(b, span[0], span[1], boom), int64(len(b)))
			check(t, "open failing at "+name, err)
		}
	})
	t.Run("read", func(t *testing.T) {
		src := newFailAt(b, 0, int64(len(b)), boom)
		src.on.Store(false)
		r, err := NewReader(src, int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		src.on.Store(true)
		_, err = r.ReadAt(make([]byte, 16), 0)
		check(t, "ReadAt", err)
		_, err = r.ReadUserData(3, make([]byte, 2048), ReadOptions{})
		check(t, "ReadUserData", err)
		if _, err := r.Verify(context.Background(), VerifyOptions{}); err == nil {
			t.Error("Verify succeeded on a failing source")
		} else {
			check(t, "Verify", err)
		}
	})
	t.Run("after Close", func(t *testing.T) {
		r, err := Open(chd)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = r.ReadAt(make([]byte, 16), 0)
		if !errors.Is(err, os.ErrClosed) {
			t.Errorf("ReadAt after Close = %v, want os.ErrClosed", err)
		}
		if errors.Is(err, ErrCorruptData) {
			t.Errorf("ReadAt after Close = %v, not corruption", err)
		}
	})
}

func TestTruncatedFilesAreCorrupt(t *testing.T) {
	chd, _ := makeDVD(t, testimg.Mixed, 64, "", 4096)
	b, err := os.ReadFile(chd)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := format.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{0, 10, format.HeaderSize, format.HeaderSize + 8, int(hdr.MetaOffset) + 5, len(b) - 1} {
		// The size is honest, or claims more than the reader delivers.
		for name, size := range map[string]int{"honest": cut, "lying": len(b)} {
			r, err := NewReader(bytes.NewReader(b[:cut]), int64(size))
			if err == nil {
				// The cut may leave the metadata intact: reads must catch it.
				_, err = r.Verify(context.Background(), VerifyOptions{})
			}
			if !errors.Is(err, ErrCorruptData) {
				t.Errorf("cut %d (%s size) = %v, want ErrCorruptData", cut, name, err)
			}
		}
	}
}

// Info hands out copies: editing them must not change what Verify hashes.
func TestInfoReturnsCopies(t *testing.T) {
	chd, _ := makeDVD(t, testimg.Mixed, 64, "", 4096)
	r := openT(t, chd)
	info := r.Info()
	if len(info.Metadata) == 0 || len(info.Metadata[0].Data) == 0 || len(info.Codecs) == 0 {
		t.Fatalf("Info has no metadata or codecs: %+v", info)
	}
	want := string(info.Metadata[0].Data)
	for i := range info.Metadata[0].Data {
		info.Metadata[0].Data[i] ^= 0xff
	}
	info.Metadata[0].Tag = "XXXX"
	info.Codecs[0] = "xxxx"
	if got := r.Info(); string(got.Metadata[0].Data) != want || got.Metadata[0].Tag == "XXXX" || got.Codecs[0] == "xxxx" {
		t.Errorf("editing Info changed the Reader's: %+v", got)
	}
	if res, err := r.Verify(context.Background(), VerifyOptions{}); err != nil || res.Status != VerifyOK {
		t.Errorf("Verify after editing Info = %v, %v", res.Status, err)
	}
}
