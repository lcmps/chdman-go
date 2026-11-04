package main

import (
	"context"
	"io"

	"fmt"

	"github.com/lcmps/chdman-go"
)

func runExtractCD(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var in, out, bin string
	var workers int
	var force, split bool
	fs := newFlags("extractcd", stderr)
	fs.str(&in, "i", "input", "input CHD file")
	fs.str(&out, "o", "output", "output cue file")
	fs.str(&bin, "ob", "outputbin", "output bin file name")
	fs.boolean(&split, "sb", "splitbin", "write one bin per track")
	fs.boolean(&force, "f", "force", "overwrite existing output files")
	fs.integer(&workers, "np", "numprocessors", "number of processors to use")
	if code, ok := fs.parse(args, stderr, map[string]*string{"input": &in, "output": &out}); !ok {
		return code
	}
	cb, finish := progress(stderr, "Extracting")
	res, err := chdman.ExtractCD(ctx, in, out, chdman.ExtractCDOptions{
		ExtractOptions: chdman.ExtractOptions{Workers: workers, Force: force, Progress: cb},
		OutputBin:      bin, SplitBin: split,
	})
	finish()
	printWarnings(stderr, res.Warnings)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Extracted %s\n", out)
	return exitOK
}

func runExtractDVD(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var in, out string
	var workers int
	var force bool
	fs := newFlags("extractdvd", stderr)
	fs.str(&in, "i", "input", "input CHD file")
	fs.str(&out, "o", "output", "output ISO file")
	fs.boolean(&force, "f", "force", "overwrite an existing output file")
	fs.integer(&workers, "np", "numprocessors", "number of processors to use")
	if code, ok := fs.parse(args, stderr, map[string]*string{"input": &in, "output": &out}); !ok {
		return code
	}
	cb, finish := progress(stderr, "Extracting")
	res, err := chdman.ExtractDVD(ctx, in, out, chdman.ExtractOptions{Workers: workers, Force: force, Progress: cb})
	finish()
	printWarnings(stderr, res.Warnings)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Extracted %s\n", out)
	return exitOK
}
