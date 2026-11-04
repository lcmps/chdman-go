package format

import (
	"errors"
	"fmt"
	"io"

	"github.com/lcmps/chdman-go/internal/chderr"
)

// ReadFullAt fills p from r at off. Unlike a bare ReadAt error check, it
// accepts a conforming reader that returns len(p) bytes together with io.EOF
// at the end of its input.
func ReadFullAt(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// ReadError describes a failed read of what. Only a read that ran out of
// input (io.EOF or io.ErrUnexpectedEOF, including a short read) means the
// file is damaged and wraps chderr.CorruptData. Any other failure, such as a
// network error or a closed file, says nothing about the file's content, so
// it is wrapped with %w and carries no corruption sentinel.
func ReadError(what string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %s: %v", chderr.CorruptData, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}
