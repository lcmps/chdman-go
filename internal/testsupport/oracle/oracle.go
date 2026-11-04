// Package oracle drives the installed chdman 0.289 binary for tests.
package oracle

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const wantVersion = "0.289"

var (
	probeOnce sync.Once
	probeErr  error
	probeVer  string
)

func probe() {
	path, err := exec.LookPath("chdman")
	if err != nil {
		probeErr = fmt.Errorf("chdman not found in PATH: %w", err)
		return
	}
	out, _ := exec.Command(path).CombinedOutput() // usage exits non-zero
	first, _, _ := strings.Cut(string(out), "\n")
	fields := strings.Fields(first)
	for i, f := range fields {
		if f == "manager" && i+1 < len(fields) {
			probeVer = fields[i+1]
		}
	}
	if probeVer != wantVersion {
		probeErr = fmt.Errorf("chdman version is %q, want %s", probeVer, wantVersion)
	}
}

// Require skips the test when chdman is missing or is not version 0.289,
// printing why. With CHDMAN_GO_RELEASE=1 it fails the test instead.
func Require(t testing.TB) {
	t.Helper()
	probeOnce.Do(probe)
	if probeErr == nil {
		return
	}
	if os.Getenv("CHDMAN_GO_RELEASE") == "1" {
		t.Fatalf("oracle unavailable: %v", probeErr)
	}
	t.Skipf("oracle unavailable: %v", probeErr)
}

// Version returns the version reported by chdman.
func Version(t testing.TB) string {
	t.Helper()
	Require(t)
	return probeVer
}

// Run executes chdman with args and returns an error that carries its
// combined output when it exits unsuccessfully.
func Run(t testing.TB, args ...string) error {
	t.Helper()
	Require(t)
	var out bytes.Buffer
	cmd := exec.Command("chdman", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("chdman %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

func mustRun(t testing.TB, args ...string) {
	t.Helper()
	if err := Run(t, args...); err != nil {
		t.Fatal(err)
	}
}

func createArgs(cmd, in, out, codecs string, hunkBytes int) []string {
	args := []string{cmd, "-f", "-i", in, "-o", out}
	if codecs != "" {
		args = append(args, "-c", codecs)
	}
	if hunkBytes > 0 {
		args = append(args, "-hs", strconv.Itoa(hunkBytes))
	}
	return args
}

// CreateDVD runs chdman createdvd. Empty codecs or zero hunkBytes means the
// chdman default.
func CreateDVD(t testing.TB, iso, chd string, codecs string, hunkBytes int) {
	t.Helper()
	mustRun(t, createArgs("createdvd", iso, chd, codecs, hunkBytes)...)
}

// CreateCD runs chdman createcd. Empty codecs or zero hunkBytes means the
// chdman default.
func CreateCD(t testing.TB, cue, chd, codecs string, hunkBytes int) {
	t.Helper()
	mustRun(t, createArgs("createcd", cue, chd, codecs, hunkBytes)...)
}

// Verify runs chdman verify and returns its failure, if any.
func Verify(t testing.TB, chd string) error {
	t.Helper()
	return Run(t, "verify", "-i", chd)
}

// ExtractDVD runs chdman extractdvd.
func ExtractDVD(t testing.TB, chd, iso string) {
	t.Helper()
	mustRun(t, "extractdvd", "-f", "-i", chd, "-o", iso)
}

// ExtractCD runs chdman extractcd, writing one bin per track when split.
func ExtractCD(t testing.TB, chd, cue string, split bool) {
	t.Helper()
	args := []string{"extractcd", "-f", "-i", chd, "-o", cue}
	if split {
		args = append(args, "-sb")
	}
	mustRun(t, args...)
}
