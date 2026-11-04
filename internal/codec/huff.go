package codec

import (
	"fmt"

	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/huffman"
)

type huffCompressor struct{}

func (huffCompressor) Compress(dst, src []byte) ([]byte, error) {
	out, err := huffman.Encode8(src)
	if err != nil {
		return nil, err
	}
	return append(dst[:0], out...), nil
}

type huffDecompressor struct{}

func (huffDecompressor) Decompress(dst, src []byte) error {
	if err := huffman.Decode8(src, dst); err != nil {
		return fmt.Errorf("%w: huff: %v", chderr.CorruptData, err)
	}
	return nil
}
