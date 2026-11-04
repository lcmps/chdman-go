package chdman

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/pipeline"
)

// ExtractOptions control ExtractDVD.
type ExtractOptions struct {
	// Workers is the number of decompression goroutines; zero means one per CPU.
	Workers int
	// Force replaces an existing output file. The replacement keeps the old
	// file's permissions. If the output path is a symbolic link, the link
	// itself is replaced and its target is left untouched.
	Force bool
	// Progress, if set, is called as output bytes are written.
	Progress func(done, total uint64)
}

// ExtractResult describes an extraction.
type ExtractResult struct {
	Warnings []Warning
}

// extractChunkBytes is the target size of one unit of extraction work. It
// bounds memory to about three chunks per worker.
const extractChunkBytes = 1 << 20

// ExtractDVD writes the ISO image held by the DVD CHD at chdPath to isoPath,
// like chdman extractdvd. A CHD of another disc type yields
// ErrUnsupportedDiscType. The output appears only if the whole run succeeds.
func ExtractDVD(ctx context.Context, chdPath, isoPath string, opts ExtractOptions) (ExtractResult, error) {
	r, err := Open(chdPath)
	if err != nil {
		return ExtractResult{}, err
	}
	defer r.Close()
	info := r.Info()
	if info.Disc != DiscDVD {
		return ExtractResult{}, fmt.Errorf("%w: %s is a %v CHD, not a DVD", chderr.UnsupportedDiscType, chdPath, info.Disc)
	}
	err = writeOutputAtomically(ctx, isoPath, opts.Force, []string{chdPath}, func(f *os.File) error {
		return copyLogical(ctx, r, f, workerCount(opts.Workers), opts.Progress)
	})
	return ExtractResult{}, err
}

// copyLogical writes the whole logical content of r to w, decoding chunks in
// parallel and writing them in order.
func copyLogical(ctx context.Context, r *Reader, w *os.File, workers int, progress func(done, total uint64)) error {
	info := r.Info()
	chunk := uint64(max(1, extractChunkBytes/info.HunkBytes)) * uint64(info.HunkBytes)
	total := info.LogicalBytes
	pool := sync.Pool{New: func() any { b := make([]byte, chunk); return &b }}

	type piece struct {
		buf *[]byte
		n   int
	}
	offsets := func(yield func(uint64) bool) error {
		for off := uint64(0); off < total; off += chunk {
			if !yield(off) {
				return nil
			}
		}
		return nil
	}
	newWorker := func() (func(uint64) (piece, error), error) {
		return func(off uint64) (piece, error) {
			buf := pool.Get().(*[]byte)
			n := int(min(chunk, total-off))
			if _, err := r.ReadAt((*buf)[:n], int64(off)); err != nil {
				pool.Put(buf)
				return piece{}, err
			}
			return piece{buf, n}, nil
		}, nil
	}
	var done uint64
	sink := func(p piece) error {
		defer pool.Put(p.buf)
		if _, err := w.Write((*p.buf)[:p.n]); err != nil {
			return err
		}
		done += uint64(p.n)
		if progress != nil {
			progress(done, total)
		}
		return nil
	}
	return pipeline.Ordered(ctx, workers, offsets, newWorker, sink)
}
