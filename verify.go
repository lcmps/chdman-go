package chdman

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
	"github.com/lcmps/chdman-go/internal/pipeline"
)

// VerifyStatus is the outcome of Verify.
type VerifyStatus int

// Verify outcomes. The zero VerifyStatus is not a valid outcome, so a result
// returned alongside an error is never mistaken for success.
const (
	// VerifyOK means both stored hashes match the recomputed ones.
	VerifyOK VerifyStatus = iota + 1
	// VerifyMismatch means a stored hash differs from the recomputed one.
	VerifyMismatch
	// VerifyNotVerifiable means the file stores no hash to check against.
	VerifyNotVerifiable
)

func (s VerifyStatus) String() string {
	switch s {
	case VerifyOK:
		return "ok"
	case VerifyMismatch:
		return "mismatch"
	case VerifyNotVerifiable:
		return "not verifiable"
	}
	return "unknown"
}

// VerifyOptions tunes Verify.
type VerifyOptions struct {
	// Workers is the number of hunks decompressed in parallel; zero or less
	// means one. The result never depends on it.
	Workers int
	// Progress, if set, is called after each hunk with the logical bytes
	// hashed so far and the total. It runs on a single goroutine.
	Progress func(done, total uint64)
}

// VerifyResult holds the stored and recomputed hashes, in hex. The Actual
// fields are empty when the file was not verifiable.
type VerifyResult struct {
	Status                         VerifyStatus
	ExpectedRawSHA1, ActualRawSHA1 string
	ExpectedSHA1, ActualSHA1       string
}

// Verify recomputes the raw and overall SHA-1 and compares them with the
// stored ones. A mismatch returns an error wrapping ErrHashMismatch along
// with the result; a hunk that cannot be decoded returns an error wrapping
// ErrCorruptData. A file with an all-zero stored raw SHA-1 is reported as
// VerifyNotVerifiable with a nil error, without reading any data.
func (r *Reader) Verify(ctx context.Context, opts VerifyOptions) (VerifyResult, error) {
	res := VerifyResult{
		ExpectedRawSHA1: hex.EncodeToString(r.hdr.RawSHA1[:]),
		ExpectedSHA1:    hex.EncodeToString(r.hdr.SHA1[:]),
	}
	if r.hdr.RawSHA1 == [20]byte{} {
		res.Status = VerifyNotVerifiable
		return res, nil
	}

	var bufs sync.Pool
	getBuf := func() []byte {
		if b, ok := bufs.Get().(*[]byte); ok {
			return *b
		}
		return make([]byte, r.hdr.HunkBytes)
	}
	hash := sha1.New()
	remaining, total := r.hdr.LogicalBytes, r.hdr.LogicalBytes
	err := pipeline.Ordered(ctx, opts.Workers,
		func(yield func(uint32) bool) error {
			for n := range uint32(len(r.entries)) {
				if !yield(n) {
					return nil
				}
			}
			return nil
		},
		func() (func(uint32) ([]byte, error), error) {
			return func(n uint32) ([]byte, error) {
				buf := getBuf()
				return buf, r.decodeHunk(n, buf)
			}, nil
		},
		func(buf []byte) error {
			data := buf[:min(uint64(len(buf)), remaining)]
			hash.Write(data)
			remaining -= uint64(len(data))
			bufs.Put(&buf)
			if opts.Progress != nil {
				opts.Progress(total-remaining, total)
			}
			return nil
		})
	if err != nil {
		return VerifyResult{}, err
	}

	var raw [20]byte
	hash.Sum(raw[:0])
	overall := format.OverallSHA1(raw, r.meta)
	res.ActualRawSHA1 = hex.EncodeToString(raw[:])
	res.ActualSHA1 = hex.EncodeToString(overall[:])
	if raw == r.hdr.RawSHA1 && overall == r.hdr.SHA1 {
		res.Status = VerifyOK
		return res, nil
	}
	res.Status = VerifyMismatch
	return res, fmt.Errorf("%w: raw SHA-1 %s (stored %s), SHA-1 %s (stored %s)", chderr.HashMismatch,
		res.ActualRawSHA1, res.ExpectedRawSHA1, res.ActualSHA1, res.ExpectedSHA1)
}
