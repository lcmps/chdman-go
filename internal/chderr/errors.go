package chderr

import "errors"

var (
	// UnsupportedVersion reports a CHD version other than 5.
	UnsupportedVersion = errors.New("chd: unsupported version")
	// ParentRequired reports a CHD that needs a parent file.
	ParentRequired = errors.New("chd: parent required")
	// UnsupportedCodec reports a codec this library does not implement.
	UnsupportedCodec = errors.New("chd: unsupported codec")
	// UnsupportedDiscType reports a disc type outside CD and DVD.
	UnsupportedDiscType = errors.New("chd: unsupported disc type")
	// CorruptData reports malformed or inconsistent CHD data.
	CorruptData = errors.New("chd: corrupt data")
	// HashMismatch reports a SHA-1 or CRC that does not match.
	HashMismatch = errors.New("chd: hash mismatch")
	// InvalidCue reports a cue sheet that cannot be parsed or used.
	InvalidCue = errors.New("chd: invalid cue sheet")
)
