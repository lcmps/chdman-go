# chdman-go

A pure Go library and command-line tool for CD and DVD game images in the CHD v5 format. It creates CHDs from bin/cue and ISO, verifies them, extracts them back, and reads individual sectors without extracting.

Files work both ways with `chdman` 0.289, with matching SHA1s. No CGO and no other programs needed at runtime.

## Install

```sh
go install github.com/lcmps/chdman-go/cmd/chdman-go@latest
```

As a library:

```sh
go get github.com/lcmps/chdman-go
```

Go 1.27 or newer.

## Command line

Commands and flags follow chdman, so existing habits carry over.

```sh
chdman-go createcd   -i game.cue -o game.chd [-f] [-c codecs] [-hs bytes] [-np n]
chdman-go createdvd  -i game.iso -o game.chd [-f] [-c codecs] [-hs bytes] [-np n]
chdman-go extractcd  -i game.chd -o game.cue [-ob name.bin] [-sb] [-f] [-np n]
chdman-go extractdvd -i game.chd -o game.iso [-f] [-np n]
chdman-go verify     -i game.chd [-np n]
chdman-go info       -i game.chd
```

- `-f` replaces an existing output. Without it nothing is overwritten.
- `-c` takes up to four comma-separated codecs (`zlib`, `zstd`, `lzma`, `huff`, `flac`, `cdzl`, `cdzs`, `cdlz`, `cdfl`) or `none`. The default is chdman's: `cdlz,cdzl,cdfl` for CDs and `lzma,zlib,huff,flac` for DVDs.
- `-hs` is the hunk size in bytes. The defaults are 19584 (eight CD frames) and 4096.
- `-np` is the number of worker goroutines, one per CPU by default (`0`); a negative value is an error. The result never depends on it.
- `extractcd` writes one bin by default and one per track with `-sb`; `-ob` names the bin.
- A failed or cancelled run leaves no partial output behind. ^C, `SIGTERM` and `SIGHUP` (a closed terminal or a lost SSH session) cancel a run and remove its temporary files.
- Progress is shown on a terminal only, a few updates a second.

Exit codes: `0` success, `1` failure, `2` not verifiable (see below).

## Library

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/lcmps/chdman-go"
)

func main() {
	ctx := context.Background()

	// Create a CHD from a cue sheet.
	res, err := chdman.CreateCD(ctx, "game.cue", "game.chd", chdman.CreateOptions{Force: true})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("sha1", res.SHA1, "rawsha1", res.RawSHA1)
	for _, w := range res.Warnings { // input data that chdman also loses
		fmt.Printf("warning (%s): %s\n", w.Kind, w.Message)
	}

	// Open it and verify.
	r, err := chdman.Open("game.chd")
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()

	v, err := r.Verify(ctx, chdman.VerifyOptions{})
	if err != nil {
		log.Fatal(err) // errors.Is(err, chdman.ErrHashMismatch) and friends
	}
	fmt.Println("verify:", v.Status)

	// Read one sector without extracting: the 2048 bytes of user data at
	// logical sector 16, where the ISO 9660 volume descriptor sits.
	buf := make([]byte, 2048)
	if _, err := r.ReadUserData(16, buf, chdman.ReadOptions{}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%q\n", buf[1:6]) // "CD001"

	// The tracks of a CD (nil for a DVD), and the whole sector as stored.
	tracks, err := r.Tracks()
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tracks {
		fmt.Println(t.Number, t.Type, t.Frames, t.StartLBA)
	}
	raw := make([]byte, 2352)
	if _, err := r.ReadSector(0, raw, chdman.ReadOptions{}); err != nil {
		log.Fatal(err)
	}

	// Extract. The output directory must exist.
	if err := os.MkdirAll("out", 0o755); err != nil {
		log.Fatal(err)
	}
	_, err = chdman.ExtractCD(ctx, "game.chd", "out/game.cue", chdman.ExtractCDOptions{SplitBin: true})
	if err != nil {
		log.Fatal(err)
	}
}
```

`Reader` also implements `io.ReaderAt` over the logical content, and `NewReader` opens any `io.ReaderAt`. A `Reader` is safe for concurrent use. Long operations take a `context.Context` for cancellation and an optional progress callback. Errors wrap sentinels you can test with `errors.Is`: `ErrUnsupportedVersion`, `ErrParentRequired`, `ErrUnsupportedCodec`, `ErrUnsupportedDiscType`, `ErrCorruptData`, `ErrHashMismatch`, `ErrInvalidCue`, `ErrNoUserData` (a sector with no 2048-byte payload, such as audio) and `ErrShortBuffer`. `ErrCorruptData` means the file's content is damaged or truncated. A failure of the underlying reader, such as a network error or a closed file, comes back as itself (`errors.Is(err, os.ErrClosed)` works) and is never reported as corruption, so a flaky share does not mark a good file as bad.

`Tracks()` returns `([]Track, error)`: the tracks of a CD, `nil, nil` for a DVD, and `ErrUnsupportedDiscType` for any other kind of CHD. `Track.Pregap` and `Track.Postgap` are signed: a cue sheet whose `INDEX 00` lies in an earlier `FILE` gives a negative pregap, as in chdman's metadata. `Info()` returns copies, so changing its slices does not affect the `Reader`. If a `Progress` callback panics, the work stops, temporary files are removed and the panic is raised again in the caller's goroutine.

On 32-bit platforms images over 2 GiB read correctly.

## Supported scope

| CHD contents | `info`, `verify` | Extract, sector access |
|---|---|---|
| CD (bin/cue, one or more bins, multitrack, multisession) | yes | yes |
| DVD (ISO) | yes | yes |
| GD-ROM, hard disk | yes | no, an "unsupported disc type" error |
| `avhu` codec, a parent CHD, or below v5 | clear error | clear error |

- Creating takes `.cue` sheets with every sector mode (`MODE1/2048`, `MODE1/2352`, `MODE2/2336`, `MODE2/2352`, `AUDIO`), `INDEX 00`, `PREGAP`, `POSTGAP`, `REM SESSION`, and ISO images for DVDs. It does not take `.toc`, `.gdi` or `.nrg`, and writes no CD+G, GD-ROM, hard disk, LaserDisc or parent CHDs.
- Reading supports `zlib`, `zstd`, `lzma`, `huff`, `flac`, `cdzl`, `cdzs`, `cdlz`, `cdfl`, uncompressed files and self-referencing hunks.
- CDs made by chdman from a `.toc` with subcode, or with `MODE2_FORM_MIX` tracks, can be read, verified and extracted. Subcode cannot be stored in bin/cue and is dropped with a warning.
- A cue sheet is read as bytes by chdman: one that starts with a UTF-8 byte order mark hides its first `FILE` line, and is refused, as chdman does, with an error that says so.
- Parsing ISO 9660 or finding game IDs is out of scope; `ReadUserData` gives you the sectors to do it with.

## Differences from chdman

- **Exit code 2: not verifiable.** chdman writes all-zero hashes for `-c none` files, and its `verify` quietly succeeds on them. chdman-go reports them as not verifiable, with its own exit code `2`, because a check that checked nothing should not look like a pass. In the library this is `VerifyNotVerifiable` with a nil error.
- **Warnings when chdman silently loses data.** Some inputs make chdman drop or overwrite data without a word: extra bytes at the end of a bin, a `REM LEAD-OUT` in a single-bin multisession cue, a bin that no track uses, and more. chdman-go gives chdman's exact result and hashes in every such case, so that the files stay interchangeable, and tells you. The CLI prints a warning and the library returns structured `Warning` values (`WarnTrailingBytesDropped`, `WarnFramesOverwritten`, `WarnOffsetTruncated`, `WarnFileUnused`, `WarnFramesDropped`, `WarnSubcodeOmitted`, `WarnTrackDataRepeated`). `WarnTrackDataRepeated` covers a cue sheet where `FILE a` holds tracks 1 and 2 and `FILE b` holds track 3: chdman reads all of `a.bin` from the start for track 2, so the disc repeats track 1's data, and chdman-go does the same. Warnings never change the output.
- **Valid maps for files of one to three hunks.** chdman 0.289 sizes the buffer of its compressed map without the Huffman tree it then exports (`chd.cpp:2175-2176`), so for some files of one to three hunks it writes a garbled map that neither chdman nor chdman-go can read back, although the header hashes are right. chdman-go always writes a valid map, so a tiny ISO or a one-track CD comes out readable, and chdman verifies it.
- **Sector addressing in pregaps.** `ReadSector` and `ReadUserData` use logical addresses by default, the disc positions that file systems use; set `ReadOptions{Physical: true}` to count frames as stored, as `extractcd` does. An address inside a track's pregap belongs to the track that owns the pregap, and a pregap that is not stored, or a postgap, reads as zeros. chdman's `logical_to_chd_lba` assigns those addresses to the previous track instead. No chdman command uses that path, so hashes and extraction match regardless.
- **Reference version and multisession.** Compatibility is with chdman 0.289 (`mame-tools 0.289`), not newer releases: the LZMA level is 8, as 0.289 uses, and no `CHSE` session entries are written. Files from newer chdman that hold `CHSE` entries still open, verify and extract, with those entries ignored. A CHD does not record lead-in and lead-out, so on a multisession disc the sector addresses of later sessions may not come out exactly as the cue sheet gave them, and every track reports session 1. 0.289 behaves the same way. Create, extract and create again keeps both hashes for ordinary inputs, but not `sha1` for tracks whose `INDEX 00` equals `INDEX 01` (multisession ones included) or for `MODE2_FORM_MIX`, and `rawsha1` also changes for tracks with subcode. chdman behaves the same, and chdman-go's round trip matches chdman's own.

## Results on real discs

These results compare chdman-go with chdman 0.289 on two game discs, using default options on one 16-thread Linux machine. Your numbers will differ.

| Disc | Contents |
|---|---|
| PS2 CD game | 639 MB: one data track |
| PS1 game | 688 MB: one data track and two audio tracks |

`rawsha1` and `sha1` match chdman on both discs, and `chdman verify` accepts chdman-go's files.

| Measure | PS2 CD: chdman | PS2 CD: chdman-go | PS1: chdman | PS1: chdman-go |
|---|---|---|---|---|
| Create | 12.9 s | 20.5 s | 14.1 s | 21.3 s |
| Verify | 18.9 s | 3.7 s | 21.6 s | 3.8 s |
| Extract | 16.3 s | 3.2 s | 18.4 s | 3.6 s |
| CHD size | 451.51 MB | 456.78 MB | 461.17 MB | 460.67 MB |

Create times vary between runs. A later run of the same build took 17.5 s and 18.8 s.

## Results on synthetic data

These results compare chdman-go with chdman 0.289 on generated images, not real games, so your numbers will differ. Each tool ran alone with default options on one idle 16-thread Linux machine, with files on tmpfs. They were measured before CD FLAC was skipped for data sectors, which made create about 23% faster on the real discs above, so the create times here are out of date.

| Test image | Contents |
|---|---|
| PS1-like | About 365 MB: `MODE2/2352` data plus 6 audio tracks with `INDEX 00`, as a single bin and as split bins |
| CD | About 697 MB: `MODE1` data plus audio |
| PS2-like DVD | 4.7 GB ISO: zeros, random data, text, textures, code, PCM and duplicate blocks |

### Compatibility

| Check | PS1-like, single bin | PS1-like, split bins | CD | PS2-like DVD |
|---|---|---|---|---|
| `rawsha1` and `sha1` equal chdman's | Yes | Yes | Yes | Yes |
| `chdman verify` accepts chdman-go's file | Yes | Yes | Yes | Yes |
| chdman-go verifies chdman's file | Yes | Yes | Yes | Yes |
| Both tools extract both files to the same data | Yes | Yes | Yes | Yes |

| Other check | Result |
|---|---|
| Unusual cue sheets compared directly with chdman | 196 cases, 0 differences |
| Bugs deliberately injected to test the test suite | 13 of 13 caught |

### Speed and memory

Each cell shows wall time and peak memory. The PS1-like column is the single-bin image.

| Operation | PS1-like: chdman | PS1-like: chdman-go | CD: chdman | CD: chdman-go | DVD: chdman | DVD: chdman-go |
|---|---|---|---|---|---|---|
| Create | 9.9 s, 268 MB | 25.7 s, 185 MB | 20.3 s, 504 MB | 49.7 s, 191 MB | 163 s, 17,142 MB* | 274 s, 263 MB |
| Verify | 15.3 s, 38 MB | 4.8 s, 26 MB | 25.8 s, 38 MB | 7.8 s, 26 MB | 106 s, 51 MB | 36 s, 88 MB |
| Extract | 8.8 s, 39 MB | 2.9 s, 123-150 MB | 24.1 s, 39 MB | 6.2 s, 155 MB | 85 s, 51 MB | 31 s, 210 MB |

\* This peak may be inflated by the files being on tmpfs. It was not investigated.

### CPU time and file size

| Measure | PS1-like: chdman | PS1-like: chdman-go | CD: chdman | CD: chdman-go | DVD: chdman | DVD: chdman-go |
|---|---|---|---|---|---|---|
| Create CPU time | 150 s | 393 s | 295 s | 711 s | 2,257 s | 3,892 s |
| Verify CPU time | 15 s | 46 s | 26 s | 99 s | 106 s | 432 s |
| CHD size compared with chdman's | n/a | +7.5% | n/a | +5.4% | n/a | +2.5% |

In short, creating takes about 1.7 to 2.6 times as long as chdman and the files are slightly larger. Verifying and extracting are about 3 to 4 times faster in wall time because the work runs in parallel, so the speedup depends on your core count. Verifying uses about 3 to 4 times the CPU time.

## Development

```sh
make test            # go test ./...
make release-check   # vet, tests that fail instead of skipping, CGO_ENABLED=0 build, no cgo outside the standard library, race detector
```

`release-check` needs `chdman` 0.289 and `flac` on `PATH` (the FLAC encoder test skips without `flac`, and any skip fails the check), and a C toolchain for the race detector, which is a test-time instrument only. It runs for several minutes, so the target passes `-timeout 30m`. Two multi-gigabyte tests (`TestCreateCDPreviousTrackSizeWraps`, `TestCreateDVDOver4GiB`) and the sparse compatibility cells are skipped under `-race` and run in the plain pass.

The compatibility tests in `compat/` need `chdman` 0.289 on `PATH`. They skip with a message otherwise, and fail when `CHDMAN_GO_RELEASE=1`. Test images are generated, so no game data is in the repository. `go test -short` skips the multi-gigabyte cells.

## License

BSD 3-Clause, see `LICENSE`. Parts are translated from MAME and keep MAME's notice, see `NOTICE`.
