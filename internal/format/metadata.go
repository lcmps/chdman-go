package format

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"github.com/lcmps/chdman-go/internal/chderr"
)

// Metadata tags, from chd.h and cdrom.h.
const (
	TagCHT2 uint32 = 'C'<<24 | 'H'<<16 | 'T'<<8 | '2'
	TagCHTR uint32 = 'C'<<24 | 'H'<<16 | 'T'<<8 | 'R'
	TagCHSE uint32 = 'C'<<24 | 'H'<<16 | 'S'<<8 | 'E'
	TagCHGD uint32 = 'C'<<24 | 'H'<<16 | 'G'<<8 | 'D'
	TagCHGT uint32 = 'C'<<24 | 'H'<<16 | 'G'<<8 | 'T'
	TagDVD  uint32 = 'D'<<24 | 'V'<<16 | 'D'<<8 | ' '
	TagGDDD uint32 = 'G'<<24 | 'D'<<16 | 'D'<<8 | 'D'
)

// FlagChecksum marks metadata that contributes to the overall SHA-1.
const FlagChecksum uint8 = 1

// metadataHeaderSize is the length of a metadata entry header.
const metadataHeaderSize = 16

// MetadataEntry is one metadata record.
type MetadataEntry struct {
	Tag   uint32
	Flags uint8
	Data  []byte
}

// ReadMetadata walks the metadata chain starting at h.MetaOffset. It returns
// an error wrapping chderr.CorruptData for a chain that loops, or that
// points past the end of the file, and whose entries together claim more
// bytes than fileSize, which only overlapping entries can do.
func ReadMetadata(r io.ReaderAt, h Header, fileSize int64) ([]MetadataEntry, error) {
	var entries []MetadataEntry
	var total int64
	seen := map[uint64]bool{}
	for off := h.MetaOffset; off != 0; {
		if seen[off] {
			return nil, fmt.Errorf("%w: metadata chain loops at offset %d", chderr.CorruptData, off)
		}
		seen[off] = true
		var hdr [metadataHeaderSize]byte
		if off > 1<<62 {
			return nil, fmt.Errorf("%w: metadata offset %d", chderr.CorruptData, off)
		}
		if err := ReadFullAt(r, hdr[:], int64(off)); err != nil {
			return nil, fmt.Errorf("%w: metadata header at %d: %v", chderr.CorruptData, off, err)
		}
		lenFlags := binary.BigEndian.Uint32(hdr[4:])
		e := MetadataEntry{Tag: binary.BigEndian.Uint32(hdr[:]), Flags: uint8(lenFlags >> 24)}
		// The 24-bit length is read in chunks so that a lying length cannot
		// force a large allocation on a short file.
		var buf bytes.Buffer
		n := int64(lenFlags & 0xffffff)
		if total += metadataHeaderSize + n; total > fileSize {
			return nil, fmt.Errorf("%w: metadata entries total %d bytes in a %d byte file", chderr.CorruptData, total, fileSize)
		}
		if _, err := io.CopyN(&buf, io.NewSectionReader(r, int64(off)+metadataHeaderSize, n), n); err != nil {
			return nil, fmt.Errorf("%w: metadata data at %d: %v", chderr.CorruptData, off, err)
		}
		e.Data = buf.Bytes()
		entries = append(entries, e)
		off = binary.BigEndian.Uint64(hdr[8:])
	}
	return entries, nil
}

// MarshalMetadata encodes e as an on-disk metadata entry whose successor is at
// offset next (zero for the last entry).
func MarshalMetadata(e MetadataEntry, next uint64) []byte {
	b := make([]byte, metadataHeaderSize+len(e.Data))
	binary.BigEndian.PutUint32(b, e.Tag)
	binary.BigEndian.PutUint32(b[4:], uint32(e.Flags)<<24|uint32(len(e.Data))&0xffffff)
	binary.BigEndian.PutUint64(b[8:], next)
	copy(b[metadataHeaderSize:], e.Data)
	return b
}

// OverallSHA1 computes the header's sha1 field from the raw data SHA-1 and the
// checksummed metadata, as compute_overall_sha1 does: the sorted (tag,
// SHA-1 of data) records are hashed after the raw SHA-1.
func OverallSHA1(raw [20]byte, entries []MetadataEntry) [20]byte {
	var records [][]byte
	for _, e := range entries {
		if e.Flags&FlagChecksum == 0 {
			continue
		}
		sum := sha1.Sum(e.Data)
		rec := binary.BigEndian.AppendUint32(nil, e.Tag)
		records = append(records, append(rec, sum[:]...))
	}
	slices.SortFunc(records, bytes.Compare)
	h := sha1.New()
	h.Write(raw[:])
	for _, rec := range records {
		h.Write(rec)
	}
	return [20]byte(h.Sum(nil))
}
