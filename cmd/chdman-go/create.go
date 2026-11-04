package main

import (
	"context"
	"fmt"
	"io"

	"github.com/lcmps/chdman-go"
)

// printWarnings writes library warnings to w.
func printWarnings(w io.Writer, warnings []chdman.Warning) {
	for _, warn := range warnings {
		fmt.Fprintf(w, "Warning: %s\n", warn.Message)
	}
}

func runCreateCD(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runCreate("createcd", chdman.CreateCD, ctx, args, stdout, stderr)
}

func runCreateDVD(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runCreate("createdvd", chdman.CreateDVD, ctx, args, stdout, stderr)
}

type createFunc func(ctx context.Context, in, out string, opts chdman.CreateOptions) (chdman.CreateResult, error)

func runCreate(name string, create createFunc, ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var in, out, codecs string
	var hunk uint
	var workers int
	var force bool
	fs := newFlags(name, stderr)
	fs.str(&in, "i", "input", "input file")
	fs.str(&out, "o", "output", "output CHD file")
	fs.boolean(&force, "f", "force", "overwrite an existing output file")
	fs.str(&codecs, "c", "compression", "none or up to 4 comma-separated codecs")
	fs.uint32(&hunk, "hs", "hunksize", "hunk size in bytes")
	fs.integer(&workers, "np", "numprocessors", "number of processors to use")
	if code, ok := fs.parse(args, stderr, map[string]*string{"input": &in, "output": &out}); !ok {
		return code
	}
	if hunk > 1<<32-1 {
		return fail(stderr, fmt.Errorf("hunk size %d is too large", hunk))
	}
	cb, finish := progress(stderr, "Compressing")
	res, err := create(ctx, in, out, chdman.CreateOptions{
		Codecs: splitCodecs(codecs), HunkBytes: uint32(hunk), Workers: workers, Force: force, Progress: cb,
	})
	finish()
	printWarnings(stderr, res.Warnings)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Created %s\n", out)
	return exitOK
}
