package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/lcmps/chdman-go"
)

func runInfo(_ context.Context, args []string, stdout, stderr io.Writer) int {
	var in string
	fs := newFlags("info", stderr)
	fs.str(&in, "i", "input", "input CHD file")
	if code, ok := fs.parse(args, stderr, map[string]*string{"input": &in}); !ok {
		return code
	}
	r, err := chdman.Open(in)
	if err != nil {
		return fail(stderr, err)
	}
	defer r.Close()
	info := r.Info()

	fmt.Fprintf(stdout, "Input file:   %s\n", in)
	fmt.Fprintf(stdout, "File Version: %d\n", info.Version)
	fmt.Fprintf(stdout, "Logical size: %s bytes\n", withCommas(info.LogicalBytes))
	fmt.Fprintf(stdout, "Hunk Size:    %s bytes\n", withCommas(uint64(info.HunkBytes)))
	fmt.Fprintf(stdout, "Total Hunks:  %s\n", withCommas(uint64(info.HunkCount)))
	fmt.Fprintf(stdout, "Unit Size:    %s bytes\n", withCommas(uint64(info.UnitBytes)))
	fmt.Fprintf(stdout, "Compression:  %s\n", strings.Join(info.Codecs, ", "))
	if info.LogicalBytes > 0 {
		fmt.Fprintf(stdout, "Ratio:        %.1f%%\n", 100*float64(info.CompressedBytes)/float64(info.LogicalBytes))
	}
	if info.Disc != chdman.DiscUnknown {
		fmt.Fprintf(stdout, "Disc type:    %v\n", info.Disc)
	}
	if !allZeros(info.SHA1) {
		fmt.Fprintf(stdout, "SHA1:         %s\n", info.SHA1)
	}
	if !allZeros(info.RawSHA1) {
		fmt.Fprintf(stdout, "Data SHA1:    %s\n", info.RawSHA1)
	}
	for _, m := range info.Metadata {
		fmt.Fprintf(stdout, "Metadata:     Tag='%s'  Length=%d bytes\n", m.Tag, len(m.Data))
	}
	for _, t := range r.Tracks() {
		fmt.Fprintf(stdout, "TRACK:%d TYPE:%s FRAMES:%d PREGAP:%d POSTGAP:%d STARTLBA:%d\n",
			t.Number, t.Type, int32(t.Frames), int32(t.Pregap), int32(t.Postgap), t.StartLBA)
	}
	return exitOK
}

// withCommas formats n with thousands separators, like chdman's big_int_string.
func withCommas(n uint64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func allZeros(hex string) bool { return strings.Trim(hex, "0") == "" }
