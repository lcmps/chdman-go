package codec

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"

	"github.com/lcmps/chdman-go/internal/chderr"
)

// deflateCompressor produces a raw deflate stream at level 9, matching
// chd_zlib_compressor.
type deflateCompressor struct {
	buf bytes.Buffer
	w   *flate.Writer
}

func newDeflateCompressor() (*deflateCompressor, error) {
	w, err := flate.NewWriter(io.Discard, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	return &deflateCompressor{w: w}, nil
}

func (c *deflateCompressor) Compress(dst, src []byte) ([]byte, error) {
	c.buf.Reset()
	c.w.Reset(&c.buf)
	if _, err := c.w.Write(src); err != nil {
		return nil, err
	}
	if err := c.w.Close(); err != nil {
		return nil, err
	}
	return append(dst[:0], c.buf.Bytes()...), nil
}

type deflateDecompressor struct {
	r io.ReadCloser
}

func newDeflateDecompressor() *deflateDecompressor { return &deflateDecompressor{} }

func (d *deflateDecompressor) Decompress(dst, src []byte) error {
	br := bytes.NewReader(src)
	if d.r == nil {
		d.r = flate.NewReader(br)
	} else if err := d.r.(flate.Resetter).Reset(br, nil); err != nil {
		return err
	}
	if _, err := io.ReadFull(d.r, dst); err != nil {
		return fmt.Errorf("%w: zlib: %v", chderr.CorruptData, err)
	}
	// Like inflate in chdman, require the stream to end exactly with the hunk.
	var extra [1]byte
	if n, err := d.r.Read(extra[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("%w: zlib: stream does not end with the hunk", chderr.CorruptData)
	}
	return nil
}
