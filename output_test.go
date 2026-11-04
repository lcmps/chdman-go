package chdman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// A panic in the work must not leave temporary files behind, and must still
// reach the caller.
func TestWriteOutputsCleansUpOnPanic(t *testing.T) {
	dir := t.TempDir()
	defer func() {
		if r := recover(); r != "boom" {
			t.Errorf("recovered %v, want the panic to propagate", r)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("left behind %s", e.Name())
		}
	}()
	writeOutputsAtomically(context.Background(), []string{filepath.Join(dir, "a"), filepath.Join(dir, "b")}, false, nil,
		func([]*os.File) error { panic("boom") })
	t.Fatal("did not panic")
}

// A panic on a pipeline goroutine, here in the caller's Progress callback,
// must reach the caller and leave no temporary file behind.
func TestProgressPanicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 64, testimg.Mixed)
	out := filepath.Join(dir, "out.chd")
	boom := errors.New("boom")
	try := func(name string, fn func()) {
		t.Helper()
		before := dirNames(t, dir)
		func() {
			defer func() {
				if r := recover(); r != boom {
					t.Errorf("%s: recovered %v, want the original panic", name, r)
				}
			}()
			fn()
		}()
		if after := dirNames(t, dir); !slices.Equal(before, after) {
			t.Errorf("%s: directory was %v, now %v", name, before, after)
		}
	}
	panicking := func(done, total uint64) { panic(boom) }
	try("CreateDVD", func() {
		CreateDVD(context.Background(), iso, out, CreateOptions{Progress: panicking})
	})
	if _, err := CreateDVD(context.Background(), iso, out, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	try("Verify", func() {
		r, err := Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		r.Verify(context.Background(), VerifyOptions{Progress: panicking})
	})
	try("ExtractDVD", func() {
		ExtractDVD(context.Background(), out, filepath.Join(dir, "x.iso"), ExtractOptions{Progress: panicking})
	})
}
