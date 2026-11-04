// Package chdman creates, verifies, extracts and randomly reads CHD v5 files.
package chdman

import "github.com/lcmps/chdman-go/internal/chderr"

// Sentinel errors, usable with errors.Is.
var (
	ErrUnsupportedVersion  = chderr.UnsupportedVersion
	ErrParentRequired      = chderr.ParentRequired
	ErrUnsupportedCodec    = chderr.UnsupportedCodec
	ErrUnsupportedDiscType = chderr.UnsupportedDiscType
	ErrCorruptData         = chderr.CorruptData
	ErrHashMismatch        = chderr.HashMismatch
	ErrInvalidCue          = chderr.InvalidCue
)
