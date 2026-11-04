package chdman

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
