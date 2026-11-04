package chdman

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// createTemp creates a new file next to path. Unlike os.CreateTemp it gets
// the permissions of an ordinary create (0666 less the umask), which the
// finished output keeps.
func createTemp(path string) (*os.File, error) {
	for {
		name := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-"+strconv.FormatUint(rand.Uint64(), 36))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
		if !errors.Is(err, os.ErrExist) {
			return f, err
		}
	}
}

// writeOutputAtomically creates path by running fn on a temporary file in the
// same directory, then syncing it and renaming it into place. Nothing is left
// behind when fn or any later step fails, and an existing file at path is
// untouched until the final rename.
//
// An existing path is an error unless force is set, and path may never be the
// same file as one of inputs, even through a symlink or a hard link.
func writeOutputAtomically(ctx context.Context, path string, force bool, inputs []string, fn func(f *os.File) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkOutput(path, force, inputs); err != nil {
		return err
	}
	tmp, err := createTemp(path)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := fn(tmp); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// checkOutput rejects an output that exists without force, or that is one of
// the inputs.
func checkOutput(path string, force bool, inputs []string) error {
	// Stat follows symlinks, so a link to an input is the input.
	out, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, lerr := os.Lstat(path); lerr == nil { // a dangling symlink
			err = nil
			break
		}
		return nil
	case err != nil:
		return err
	}
	for _, in := range inputs {
		st, err := os.Stat(in)
		if err != nil {
			return err
		}
		if out != nil && os.SameFile(out, st) {
			return fmt.Errorf("chdman: output %s is the same file as input %s", path, in)
		}
	}
	if !force {
		return fmt.Errorf("chdman: %s: %w", path, os.ErrExist)
	}
	return nil
}
