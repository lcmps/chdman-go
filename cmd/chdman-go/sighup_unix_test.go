//go:build unix

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lcmps/chdman-go"
	"github.com/lcmps/chdman-go/internal/testsupport/testimg"
)

// TestMain turns the test binary into the child of TestSighupDuringCreate
// when CHDMAN_GO_SIGHUP_HELPER is set. The child installs the CLI's signal
// handling, says it is ready, waits to be told to go, gives a SIGHUP sent
// meanwhile time to arrive, then runs a create.
func TestMain(m *testing.M) {
	if args := os.Getenv("CHDMAN_GO_SIGHUP_HELPER"); args != "" {
		ctx, stop := notifyContext(context.Background())
		defer stop()
		fmt.Println("ready")
		bufio.NewReader(os.Stdin).ReadString('\n')
		time.Sleep(300 * time.Millisecond)
		in, out, _ := strings.Cut(args, "|")
		os.Exit(run(ctx, []string{"createdvd", "-i", in, "-o", out}, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// runWithSighup runs the helper with SIGHUP ignored or not, sends it SIGHUP
// while it waits, and returns its exit code.
func runWithSighup(t *testing.T, ignored bool, in, out string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", `if [ "$1" = ignore ]; then trap '' HUP; fi; shift; exec "$@"`, "sh",
		map[bool]string{true: "ignore", false: "keep"}[ignored],
		os.Args[0])
	cmd.Env = append(os.Environ(), "CHDMAN_GO_SIGHUP_HELPER="+in+"|"+out)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper said %q", line)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(stdin, "go")
	err = cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func TestSighupDuringCreate(t *testing.T) {
	dir := t.TempDir()
	iso := testimg.ISO(t, dir, 64, testimg.Mixed)

	t.Run("ignored at startup, as under nohup", func(t *testing.T) {
		out := filepath.Join(dir, "nohup.chd")
		if code := runWithSighup(t, true, iso, out); code != 0 {
			t.Fatalf("exit %d, want the run to finish", code)
		}
		r, err := chdman.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if res, err := r.Verify(t.Context(), chdman.VerifyOptions{}); err != nil || res.Status != chdman.VerifyOK {
			t.Errorf("output does not verify: %v %v", res.Status, err)
		}
	})
	t.Run("not ignored, cancels and leaves nothing", func(t *testing.T) {
		out := filepath.Join(dir, "cancelled.chd")
		if code := runWithSighup(t, false, iso, out); code != exitFailure {
			t.Fatalf("exit %d, want %d", code, exitFailure)
		}
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.Contains(e.Name(), "cancelled.chd") {
				t.Errorf("left behind %s", e.Name())
			}
		}
	})
}
