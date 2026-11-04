package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lcmps/chdman-go"
)

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var in string
	var workers int
	fs := newFlags("verify", stderr)
	fs.str(&in, "i", "input", "input CHD file")
	fs.integer(&workers, "np", "numprocessors", "number of processors to use")
	if code, ok := fs.parse(args, stderr, map[string]*string{"input": &in}); !ok {
		return code
	}
	r, err := chdman.Open(in)
	if err != nil {
		return fail(stderr, err)
	}
	defer r.Close()
	cb, finish := progress(stderr, "Verifying")
	res, err := r.Verify(ctx, chdman.VerifyOptions{Workers: workers, Progress: cb})
	finish()
	switch {
	case res.Status == chdman.VerifyNotVerifiable:
		fmt.Fprintln(stdout, "No verification to be done; CHD is not verifiable (it stores no checksum)")
		return exitNotVerifiable
	case res.Status == chdman.VerifyMismatch || errors.Is(err, chdman.ErrHashMismatch):
		if res.ActualRawSHA1 != res.ExpectedRawSHA1 {
			fmt.Fprintf(stderr, "Error: Raw SHA1 in header = %s\n              actual SHA1 = %s\n", res.ExpectedRawSHA1, res.ActualRawSHA1)
		} else {
			fmt.Fprintln(stdout, "Raw SHA1 verification successful!")
		}
		if res.ActualSHA1 != res.ExpectedSHA1 {
			fmt.Fprintf(stderr, "Error: Overall SHA1 in header = %s\n                  actual SHA1 = %s\n", res.ExpectedSHA1, res.ActualSHA1)
		}
		return exitFailure
	case err != nil:
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "Raw SHA1 verification successful!")
	fmt.Fprintln(stdout, "Overall SHA1 verification successful!")
	return exitOK
}
