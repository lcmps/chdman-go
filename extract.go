package chdman

// Translated from MAME's src/tools/chdman.cpp (BSD-3-Clause, copyright Aaron
// Giles); see NOTICE.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/lcmps/chdman-go/internal/cdrom"
	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/pipeline"
)

// ExtractOptions control ExtractDVD and ExtractCD.
type ExtractOptions struct {
	// Workers is the number of decompression goroutines; zero means one per CPU.
	Workers int
	// Force replaces an existing output file. The replacement keeps the old
	// file's permissions. If the output path is a symbolic link, the link
	// itself is replaced and its target is left untouched.
	Force bool
	// Progress, if set, is called as output bytes are written.
	// The callback runs on a goroutine of the library, not the caller's. If it
	// panics, the work stops, temporary files are removed and the panic is
	// raised again in the caller's goroutine.
	Progress func(done, total uint64)
}

// ExtractResult describes an extraction.
type ExtractResult struct {
	Warnings []Warning
}

// extractChunkBytes is the target size of one unit of extraction work. It
// bounds memory to about three chunks per worker.
const extractChunkBytes = 1 << 20

// ExtractDVD writes the ISO image held by the DVD CHD at chdPath to isoPath,
// like chdman extractdvd. A CHD of another disc type yields
// ErrUnsupportedDiscType. The output appears only if the whole run succeeds.
func ExtractDVD(ctx context.Context, chdPath, isoPath string, opts ExtractOptions) (ExtractResult, error) {
	r, err := Open(chdPath)
	if err != nil {
		return ExtractResult{}, err
	}
	defer r.Close()
	info := r.Info()
	if info.Disc != DiscDVD {
		return ExtractResult{}, fmt.Errorf("%w: %s is %v, not a DVD", chderr.UnsupportedDiscType, chdPath, info.Disc.description())
	}
	err = writeOutputAtomically(ctx, isoPath, opts.Force, []string{chdPath}, func(f *os.File) error {
		return copyLogical(ctx, r, f, workerCount(opts.Workers), opts.Progress)
	})
	return ExtractResult{}, err
}

// copyLogical writes the whole logical content of r to w, decoding chunks in
// parallel and writing them in order.
func copyLogical(ctx context.Context, r *Reader, w *os.File, workers int, progress func(done, total uint64)) error {
	info := r.Info()
	chunk := uint64(max(1, extractChunkBytes/info.HunkBytes)) * uint64(info.HunkBytes)
	total := info.LogicalBytes
	pool := sync.Pool{New: func() any { b := make([]byte, chunk); return &b }}

	type piece struct {
		buf *[]byte
		n   int
	}
	offsets := func(yield func(uint64) bool) error {
		for off := uint64(0); off < total; off += chunk {
			if !yield(off) {
				return nil
			}
		}
		return nil
	}
	newWorker := func() (func(uint64) (piece, error), error) {
		return func(off uint64) (piece, error) {
			buf := pool.Get().(*[]byte)
			n := int(min(chunk, total-off))
			if _, err := r.ReadAt((*buf)[:n], int64(off)); err != nil {
				pool.Put(buf)
				return piece{}, err
			}
			return piece{buf, n}, nil
		}, nil
	}
	var done uint64
	sink := func(p piece) error {
		defer pool.Put(p.buf)
		if _, err := w.Write((*p.buf)[:p.n]); err != nil {
			return err
		}
		done += uint64(p.n)
		if progress != nil {
			progress(done, total)
		}
		return nil
	}
	return pipeline.Ordered(ctx, workers, offsets, newWorker, sink)
}

// ExtractCDOptions control ExtractCD.
type ExtractCDOptions struct {
	ExtractOptions
	// OutputBin names the bin file, like chdman's -ob. Empty means the cue
	// path without its extension, plus ".bin". Unlike a cue path, the name is
	// not taken relative to the cue's directory: a relative name is relative
	// to the working directory, as with chdman. Only its base name appears in
	// the cue. With SplitBin it must hold the track number variable %t,
	// optionally with a width such as %02t.
	OutputBin string
	// SplitBin writes one bin per track. Without OutputBin the bins are named
	// like "name (Track 1).bin", with two-digit numbers on discs of ten or
	// more tracks.
	SplitBin bool
}

// ExtractCD writes the CD held by the CHD at chdPath as a cue sheet at
// cuePath and its bins, like chdman extractcd. The path must end in ".cue".
// A CHD of another disc type yields ErrUnsupportedDiscType. Audio comes out
// in the byte order cue sheets use, padding frames are left out, and subcode
// cannot be stored in bin/cue, so it is dropped with a WarnSubcodeOmitted
// warning for each track that has it. The cue and the bins appear only if
// the whole run succeeds.
func ExtractCD(ctx context.Context, chdPath, cuePath string, opts ExtractCDOptions) (ExtractResult, error) {
	r, err := Open(chdPath)
	if err != nil {
		return ExtractResult{}, err
	}
	defer r.Close()
	info := r.Info()
	if info.Disc != DiscCD {
		return ExtractResult{}, fmt.Errorf("%w: %s is %v, not a CD", chderr.UnsupportedDiscType, chdPath, info.Disc.description())
	}
	toc, err := cdrom.ParseMetadata(r.meta)
	if err != nil {
		return ExtractResult{}, err
	}
	plan, err := planCDExtract(cuePath, toc, opts)
	if err != nil {
		return ExtractResult{}, err
	}
	var frames uint64
	for _, t := range toc.Tracks {
		frames += uint64(t.Frames + t.ExtraFrames)
	}
	if frames*cdrom.FrameSize > info.LogicalBytes {
		return ExtractResult{}, fmt.Errorf("%w: the tracks need %d frames but the CHD holds %d", chderr.CorruptData,
			frames, info.LogicalBytes/cdrom.FrameSize)
	}

	paths := append([]string{cuePath}, plan.bins...)
	err = writeOutputsAtomically(ctx, paths, opts.Force, []string{chdPath}, func(files []*os.File) error {
		if _, err := files[0].Write(plan.cue); err != nil {
			return err
		}
		return copyTracks(ctx, r, toc, plan, files[1:], workerCount(opts.Workers), opts.Progress)
	})
	if err != nil {
		return ExtractResult{}, err
	}
	return ExtractResult{Warnings: plan.warnings}, nil
}

// cdPlan is everything ExtractCD decides before it writes: the cue text, the
// bins and which bin each track goes to.
type cdPlan struct {
	cue      []byte
	bins     []string // distinct bin paths, in the order the tracks first use them
	trackBin []int    // for each track, its index in bins
	warnings []Warning
}

// planCDExtract translates the setup of do_extract_cd for cue/bin output:
// the output names, the cue sheet and the warnings.
func planCDExtract(cuePath string, toc cdrom.TOC, opts ExtractCDOptions) (cdPlan, error) {
	if !strings.HasSuffix(strings.ToLower(cuePath), ".cue") {
		return cdPlan{}, fmt.Errorf("chdman: output %s must end in .cue; only bin/cue output is supported", cuePath)
	}
	name := cuePath[:strings.LastIndexByte(cuePath, '.')]
	if opts.SplitBin {
		format := "%t"
		if len(toc.Tracks) >= 10 {
			format = "%02t"
		}
		name += " (Track " + format + ")"
	}
	ext := ".bin"
	if opts.OutputBin != "" {
		name = opts.OutputBin
		ext = ".bin"
		if chop := strings.LastIndexByte(name, '.'); chop >= 0 {
			name, ext = name[:chop], name[chop:]
		}
	}
	if strings.Contains(name, `"`) || strings.Contains(ext, `"`) {
		return cdPlan{}, fmt.Errorf("chdman: output bin filename (%s%s) must not contain quotation marks", name, ext)
	}

	var plan cdPlan
	var cue bytes.Buffer
	var current string
	var discOfs uint32
	var fileOfs uint64
	for i, t := range toc.Tracks {
		bin, err := trackBinName(name+ext, i+1, opts.SplitBin)
		if err != nil {
			return cdPlan{}, err
		}
		if len(plan.bins) == 0 || bin != current {
			current, discOfs, fileOfs = bin, 0, 0
			plan.bins = append(plan.bins, bin)
		}
		plan.trackBin = append(plan.trackBin, len(plan.bins)-1)
		err = cdrom.WriteCueEntry(&cue, i+1, t, filepath.Base(bin), discOfs, uint32(min(fileOfs, math.MaxUint32)))
		if err != nil {
			return cdPlan{}, err
		}
		if t.Sub != cdrom.None {
			plan.warnings = append(plan.warnings, Warning{
				Kind: WarnSubcodeOmitted, Track: i + 1,
				Message: fmt.Sprintf("track %d has subcode data, which bin/cue cannot hold and which is omitted", i+1),
			})
		}
		discOfs += t.Frames - t.PadFrames
		fileOfs += uint64(t.Frames-t.PadFrames) * uint64(t.DataSize)
	}
	plan.cue = cue.Bytes()
	return plan, nil
}

// trackVariable matches the format variables of chdman's output bin names.
var trackVariable = regexp.MustCompile(`(%*)(%([+-]?\d+)?([a-zA-Z]))`)

// trackBinName expands the track number variable of a bin name for one track,
// like the variable replacement in do_extract_cd. Variables other than %t,
// and any variable preceded by an odd number of extra percent signs, stay as
// they are, and %t only means something when the bins are split.
func trackBinName(name string, track int, split bool) (string, error) {
	formatted := name
	found := false
	for _, m := range trackVariable.FindAllStringSubmatch(name, -1) {
		escape, full, width, kind := m[1], m[2], m[3], m[4]
		if len(escape)%2 != 0 || kind != "t" || !split {
			continue
		}
		if len(strings.TrimLeft(width, "+-")) > 4 {
			return "", fmt.Errorf("chdman: track number width in %s is too large", full)
		}
		formatted = strings.ReplaceAll(formatted, full, fmt.Sprintf("%"+width+"d", track))
		found = true
	}
	if split && !found {
		return "", fmt.Errorf("chdman: a track number variable (%%t) must be in the output bin filename %q when SplitBin is set", name)
	}
	return formatted, nil
}

// cdChunk is a run of frames of one track, and the bin it goes to.
type cdChunk struct {
	bin, track   int
	first, count uint32
}

// copyTracks writes the sector data of every track to its bin. It decodes
// chunks of frames in parallel and writes them in order. Of each stored frame
// only the track's data size is kept, so subcode and padding frames drop out,
// and audio is byte-swapped back to the order bin/cue uses.
func copyTracks(ctx context.Context, r *Reader, toc cdrom.TOC, plan cdPlan, bins []*os.File, workers int, progress func(done, total uint64)) error {
	var total uint64
	for _, t := range toc.Tracks {
		total += uint64(t.Frames) * uint64(t.DataSize)
	}

	type piece struct {
		bin int
		buf *[]byte
		n   int
	}
	var pool sync.Pool
	pool.New = func() any { b := make([]byte, extractChunkBytes); return &b }
	// The chunks are made as they are needed: a crafted CHD can claim a disc
	// of billions of frames.
	offsets := func(yield func(cdChunk) bool) error {
		for i, t := range toc.Tracks {
			per := uint32(max(1, extractChunkBytes/t.DataSize))
			for first := uint64(0); first < uint64(t.Frames); first += uint64(per) {
				if !yield(cdChunk{plan.trackBin[i], i, uint32(first), min(per, t.Frames-uint32(first))}) {
					return nil
				}
			}
		}
		return nil
	}
	newWorker := func() (func(cdChunk) (piece, error), error) {
		return func(c cdChunk) (piece, error) {
			t := toc.Tracks[c.track]
			size := int(t.DataSize)
			buf := pool.Get().(*[]byte)
			n := int(c.count) * size
			for i := range int(c.count) {
				frame := (*buf)[i*size : (i+1)*size]
				off := (uint64(t.ChdFrameOfs) + uint64(c.first) + uint64(i)) * cdrom.FrameSize
				if _, err := r.ReadAt(frame, int64(off)); err != nil {
					pool.Put(buf)
					return piece{}, err
				}
				if t.Type == cdrom.AUDIO {
					for j := 0; j+1 < len(frame); j += 2 {
						frame[j], frame[j+1] = frame[j+1], frame[j]
					}
				}
			}
			return piece{c.bin, buf, n}, nil
		}, nil
	}
	var done uint64
	sink := func(p piece) error {
		defer pool.Put(p.buf)
		if _, err := bins[p.bin].Write((*p.buf)[:p.n]); err != nil {
			return err
		}
		done += uint64(p.n)
		if progress != nil {
			progress(done, total)
		}
		return nil
	}
	return pipeline.Ordered(ctx, workers, offsets, newWorker, sink)
}
