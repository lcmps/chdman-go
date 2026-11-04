// Package codec implements the CHD hunk codecs and the codec group that
// picks the smallest result for each hunk.
package codec

import (
	"fmt"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/format"
)

// Compressor compresses one hunk at a time. Instances are not safe for
// concurrent use; each worker owns its own.
type Compressor interface {
	// Compress appends the compressed form of src to dst[:0] and returns it.
	Compress(dst, src []byte) ([]byte, error)
}

// Decompressor decompresses one hunk at a time. Instances are not safe for
// concurrent use; each worker owns its own.
type Decompressor interface {
	// Decompress fills dst, which must be exactly the expected length, from src.
	Decompress(dst, src []byte) error
}

// NewCompressor returns a compressor for tag, or chderr.UnsupportedCodec.
func NewCompressor(t format.CodecTag, hunkBytes uint32) (Compressor, error) {
	switch t {
	case format.CodecZlib:
		return newDeflateCompressor()
	case format.CodecZstd:
		return newZstdCompressor()
	case format.CodecLZMA:
		return newLZMACompressor(hunkBytes), nil
	case format.CodecHuffman:
		return huffCompressor{}, nil
	case format.CodecFLAC:
		return newFLACCompressor(hunkBytes), nil
	case format.CodecCDZlib:
		return newCDCompressor(format.CodecZlib, format.CodecZlib, hunkBytes)
	case format.CodecCDZstd:
		return newCDCompressor(format.CodecZstd, format.CodecZstd, hunkBytes)
	case format.CodecCDLZMA:
		return newCDCompressor(format.CodecLZMA, format.CodecZlib, hunkBytes)
	case format.CodecCDFLAC:
		return newCDFLACCompressor(hunkBytes)
	}
	return nil, fmt.Errorf("%w: %v", chderr.UnsupportedCodec, t)
}

// NewDecompressor returns a decompressor for tag, or chderr.UnsupportedCodec.
func NewDecompressor(t format.CodecTag, hunkBytes uint32) (Decompressor, error) {
	switch t {
	case format.CodecZlib:
		return newDeflateDecompressor(), nil
	case format.CodecZstd:
		return newZstdDecompressor()
	case format.CodecLZMA:
		return newLZMADecompressor(hunkBytes), nil
	case format.CodecHuffman:
		return huffDecompressor{}, nil
	case format.CodecFLAC:
		return &flacDecompressor{}, nil
	case format.CodecCDZlib:
		return newCDDecompressor(format.CodecZlib, format.CodecZlib, hunkBytes)
	case format.CodecCDZstd:
		return newCDDecompressor(format.CodecZstd, format.CodecZstd, hunkBytes)
	case format.CodecCDLZMA:
		return newCDDecompressor(format.CodecLZMA, format.CodecZlib, hunkBytes)
	case format.CodecCDFLAC:
		return newCDFLACDecompressor(hunkBytes)
	}
	return nil, fmt.Errorf("%w: %v", chderr.UnsupportedCodec, t)
}
