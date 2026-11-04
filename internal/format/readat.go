package format

import "io"

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
