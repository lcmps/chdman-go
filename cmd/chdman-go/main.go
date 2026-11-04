// Command chdman-go creates, verifies, extracts and inspects CHD files. Its
// commands and flags follow chdman's.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Exit codes.
const (
	exitOK            = 0
	exitFailure       = 1
	exitNotVerifiable = 2
)

const usageText = `Usage: chdman-go <command> [options]

Commands:
  createcd   -i game.cue -o game.chd [-f] [-c codecs] [-hs bytes] [-np n]
  createdvd  -i game.iso -o game.chd [-f] [-c codecs] [-hs bytes] [-np n]
  extractcd  -i game.chd -o game.cue [-ob name.bin] [-sb] [-f] [-np n]
  extractdvd -i game.chd -o game.iso [-f] [-np n]
  verify     -i game.chd [-np n]
  info       -i game.chd

Exit codes: 0 success, 1 failure, 2 not verifiable.
`

// command runs one subcommand and returns its exit code.
type command func(ctx context.Context, args []string, stdout, stderr io.Writer) int

var commands = map[string]command{
	"createcd":   runCreateCD,
	"createdvd":  runCreateDVD,
	"extractcd":  runExtractCD,
	"extractdvd": runExtractDVD,
	"verify":     runVerify,
	"info":       runInfo,
}

// notifyContext returns a context that ends when the process is asked to stop,
// with ^C, SIGTERM or SIGHUP (a closed terminal or a lost SSH session). A cancelled run removes its temporary files; the
// default action of a signal would leave them.
func notifyContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}

func main() {
	ctx, stop := notifyContext(context.Background())
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the command named by args[0] and returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitFailure
	}
	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	}
	cmd, ok := commands[name]
	if !ok {
		if isChdmanCommand(name) {
			fmt.Fprintf(stderr, "chdman-go does not support %q; supported commands: %s\n", name, supportedCommands())
		} else {
			fmt.Fprintf(stderr, "chdman-go: unknown command %q\n\n%s", name, usageText)
		}
		return exitFailure
	}
	return cmd(ctx, rest, stdout, stderr)
}

func supportedCommands() string {
	return "createcd, createdvd, extractcd, extractdvd, verify, info"
}

// isChdmanCommand reports whether name is a chdman command this tool does not implement.
func isChdmanCommand(name string) bool {
	switch name {
	case "createhd", "createraw", "createld", "extracthd", "extractraw", "extractld",
		"copy", "addmeta", "delmeta", "dumpmeta", "listtemplates", "help":
		return true
	}
	return false
}

// flags wraps a FlagSet whose options each have a short and a long name, as in chdman.
type flags struct {
	*flag.FlagSet
}

func newFlags(name string, stderr io.Writer) *flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	return &flags{fs}
}

func (f *flags) str(p *string, short, long, usage string) {
	f.StringVar(p, short, "", usage)
	f.StringVar(p, long, "", usage)
}

func (f *flags) integer(p *int, short, long, usage string) {
	f.IntVar(p, short, 0, usage)
	f.IntVar(p, long, 0, usage)
}

func (f *flags) uint32(p *uint, short, long, usage string) {
	f.UintVar(p, short, 0, usage)
	f.UintVar(p, long, 0, usage)
}

func (f *flags) boolean(p *bool, short, long, usage string) {
	f.BoolVar(p, short, false, usage)
	f.BoolVar(p, long, false, usage)
}

// parse parses args, which must leave no positional arguments. ok is false if
// the command should stop with the returned exit code.
func (f *flags) parse(args []string, stderr io.Writer, required map[string]*string) (code int, ok bool) {
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitFailure, false
	}
	for _, name := range []string{"np", "numprocessors"} {
		if fl := f.Lookup(name); fl != nil && fl.Value.String() != "" {
			if n, err := strconv.Atoi(fl.Value.String()); err == nil && n < 0 {
				fmt.Fprintf(stderr, "chdman-go: -%s must not be negative (0 uses every processor), got %d\n", name, n)
				return exitFailure, false
			}
		}
	}
	if f.NArg() > 0 {
		fmt.Fprintf(stderr, "chdman-go: unexpected argument %q\n", f.Arg(0))
		return exitFailure, false
	}
	for _, opt := range []string{"input", "output"} {
		if v, need := required[opt]; need && *v == "" {
			fmt.Fprintf(stderr, "chdman-go: missing required option -%s\n", opt[:1])
			return exitFailure, false
		}
	}
	return exitOK, true
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return exitFailure
}

// splitCodecs splits the value of -c into codec names. Validation is the
// library's. An option that was given but is empty is one empty name, which
// chdman rejects, not a request for the defaults.
func splitCodecs(s string, given bool) []string {
	if !given {
		return nil
	}
	return strings.Split(s, ",")
}

// given reports whether any of the named options was on the command line.
func (f *flags) given(names ...string) bool {
	var found bool
	f.Visit(func(fl *flag.Flag) {
		found = found || slices.Contains(names, fl.Name)
	})
	return found
}

// progressInterval is the least time between two progress updates. The library
// reports after every hunk, which is far more often than a terminal needs.
const progressInterval = 250 * time.Millisecond

// progress returns a progress callback and a function that ends the progress
// line. It reports only when stderr is a terminal, so logs stay clean.
func progress(stderr io.Writer, label string) (cb func(done, total uint64), finish func()) {
	f, ok := stderr.(*os.File)
	if !ok {
		return nil, func() {}
	}
	if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return nil, func() {}
	}
	return newProgress(f, label, progressInterval, time.Now)
}

// newProgress writes the percentage done to w at most once per interval, and
// the last update at finish, which also ends the line.
func newProgress(w io.Writer, label string, interval time.Duration, now func() time.Time) (cb func(done, total uint64), finish func()) {
	var mu sync.Mutex
	var last time.Time
	var pending string // the latest update that was not written
	shown := false
	write := func(line string) {
		fmt.Fprint(w, line)
		shown, pending = true, ""
	}
	cb = func(done, total uint64) {
		if total == 0 {
			return
		}
		line := fmt.Sprintf("\r%s, %.1f%% complete...", label, 100*float64(done)/float64(total))
		mu.Lock()
		defer mu.Unlock()
		t := now()
		if shown && t.Sub(last) < interval {
			pending = line
			return
		}
		last = t
		write(line)
	}
	finish = func() {
		mu.Lock()
		defer mu.Unlock()
		if pending != "" {
			write(pending)
		}
		if shown {
			fmt.Fprint(w, "\n")
		}
	}
	return cb, finish
}
