package chdman

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
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
// same file as one of inputs, even through a symlink or a hard link. Forcing
// over a regular file keeps its permissions; forcing over a symlink replaces
// the link itself and leaves its target alone.
func writeOutputAtomically(ctx context.Context, path string, force bool, inputs []string, fn func(f *os.File) error) error {
	return writeOutputsAtomically(ctx, []string{path}, force, inputs, func(files []*os.File) error {
		return fn(files[0])
	})
}

// writeOutputsAtomically is writeOutputAtomically for several files that
// belong together. Every path is checked before any work starts, fn gets one
// temporary file per path, in order, and the files are renamed into place
// together at the end, last path first. If a rename fails, the files already
// renamed that did not exist before are removed again; one that replaced an
// old file cannot be undone. The paths must be distinct files.
func writeOutputsAtomically(ctx context.Context, paths []string, force bool, inputs []string, fn func(files []*os.File) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, path := range paths {
		if slices.ContainsFunc(paths[:i], func(p string) bool { return filepath.Clean(p) == filepath.Clean(path) }) {
			return fmt.Errorf("chdman: %s is named as two outputs", path)
		}
		if err := checkOutput(path, force, inputs); err != nil {
			return err
		}
		// Existing outputs that are one file would write over each other.
		a, err := os.Stat(path)
		if err != nil {
			continue
		}
		for _, other := range paths[:i] {
			if b, err := os.Stat(other); err == nil && os.SameFile(a, b) {
				return fmt.Errorf("chdman: outputs %s and %s are the same file", other, path)
			}
		}
	}

	tmps := make([]*os.File, 0, len(paths))
	// Cleanup runs unless every step succeeded, which includes a panic: the
	// panic goes on to the caller once the temporary files are gone.
	done := false
	defer func() {
		if !done {
			for _, tmp := range tmps {
				tmp.Close()
				os.Remove(tmp.Name())
			}
		}
	}()
	for _, path := range paths {
		tmp, err := createTemp(path)
		if err != nil {
			return err
		}
		tmps = append(tmps, tmp)
	}
	if err := fn(tmps); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	existed := make([]bool, len(paths))
	for i, path := range paths {
		tmp := tmps[i]
		if old, err := os.Lstat(path); err == nil {
			existed[i] = true
			// A replaced file keeps its permissions. A symlink is replaced by
			// the new file, not written through, so it has none to keep.
			if old.Mode().IsRegular() {
				if err := tmp.Chmod(old.Mode().Perm()); err != nil {
					return err
				}
			}
		}
		if err := tmp.Sync(); err != nil {
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
	}
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Rename(tmps[i].Name(), paths[i]); err != nil {
			for j := i + 1; j < len(paths); j++ {
				if !existed[j] {
					os.Remove(paths[j])
				}
			}
			return err
		}
	}
	done = true
	return nil
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
