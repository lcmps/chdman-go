package codec

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/lcmps/chdman-go/internal/chderr"
)

// zstdCompressor emits one complete zstd frame per call at the highest
// level, matching chd_zstd_compressor.
type zstdCompressor struct {
	enc *zstd.Encoder
}

func newZstdCompressor() (*zstdCompressor, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(1),
		zstd.WithZeroFrames(true),
	)
	if err != nil {
		return nil, err
	}
	return &zstdCompressor{enc: enc}, nil
}

func (c *zstdCompressor) Compress(dst, src []byte) ([]byte, error) {
	return c.enc.EncodeAll(src, dst[:0]), nil
}

type zstdDecompressor struct {
	dec *zstd.Decoder
}

func newZstdDecompressor() (*zstdDecompressor, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &zstdDecompressor{dec: dec}, nil
}

func (d *zstdDecompressor) Decompress(dst, src []byte) error {
	out, err := d.dec.DecodeAll(src, dst[:0])
	if err != nil {
		return fmt.Errorf("%w: zstd: %v", chderr.CorruptData, err)
	}
	if len(out) != len(dst) {
		return fmt.Errorf("%w: zstd: got %d bytes, want %d", chderr.CorruptData, len(out), len(dst))
	}
	return nil
}
