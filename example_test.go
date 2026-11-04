package chdman_test

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/lcmps/chdman-go"
)

// Open a CHD, read the first user-data sector of a data track, and tell the
// kinds of failure apart.
func Example() {
	r, err := chdman.Open("game.chd")
	switch {
	case errors.Is(err, os.ErrNotExist):
		log.Fatal("no such file")
	case errors.Is(err, chdman.ErrCorruptData):
		log.Fatal("the file is damaged or truncated")
	case errors.Is(err, chdman.ErrUnsupportedVersion), errors.Is(err, chdman.ErrParentRequired):
		log.Fatal("not a standalone CHD v5")
	case err != nil:
		log.Fatal(err) // for example an I/O error: not evidence of corruption
	}
	defer r.Close()

	// Logical addressing, as filesystem structures use it: sector 16 holds
	// the ISO 9660 volume descriptor.
	buf := make([]byte, 2048)
	switch _, err := r.ReadUserData(16, buf, chdman.ReadOptions{}); {
	case errors.Is(err, io.EOF):
		fmt.Println("the disc ends before sector 16")
	case errors.Is(err, chdman.ErrNoUserData):
		fmt.Println("sector 16 is audio")
	case errors.Is(err, chdman.ErrCorruptData):
		fmt.Println("a hunk is damaged")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Printf("%q\n", buf[1:6]) // "CD001"
	}

	tracks, err := r.Tracks()
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tracks {
		fmt.Println(t.Number, t.Type, t.Frames, t.StartLBA)
	}
}
