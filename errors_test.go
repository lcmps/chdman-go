package chdman

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorMessagesAreConsistent(t *testing.T) {
	for _, err := range []error{ErrUnsupportedVersion, ErrParentRequired, ErrUnsupportedCodec, ErrUnsupportedDiscType,
		ErrCorruptData, ErrHashMismatch, ErrInvalidCue, ErrNoUserData, ErrShortBuffer} {
		if !strings.HasPrefix(err.Error(), "chdman: ") {
			t.Errorf("%q does not start with \"chdman: \"", err)
		}
	}
	for d, want := range map[DiscType]string{
		DiscCD: "a CD CHD", DiscDVD: "a DVD CHD", DiscGD: "a GD-ROM CHD", DiscHD: "a hard disk CHD", DiscUnknown: "a CHD of unknown type",
	} {
		if got := d.description(); got != want {
			t.Errorf("%v.description() = %q, want %q", d, got, want)
		}
	}
}

// The message for a wrong kind of CHD must read as a sentence.
func TestWrongDiscTypeMessage(t *testing.T) {
	chd, _ := makeDVD(t, 0, 16, "", 0)
	r := openT(t, chd)
	_, err := r.Tracks()
	if err != nil {
		t.Fatal("a DVD has no tracks and no error")
	}
	_, err = ExtractCD(t.Context(), chd, t.TempDir()+"/o.cue", ExtractCDOptions{})
	if !errors.Is(err, ErrUnsupportedDiscType) || !strings.Contains(err.Error(), "is a DVD CHD, not a CD") {
		t.Errorf("ExtractCD of a DVD = %v", err)
	}
}
