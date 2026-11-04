package format

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lcmps/chdman-go/internal/bitstream"
	"github.com/lcmps/chdman-go/internal/chderr"
	"github.com/lcmps/chdman-go/internal/huffman"
)

// Map entry compression types, from chd.cpp:70-100. The first seven are
// stored in decoded maps; the rest are pseudo-types used only by the
// compressed map encoding.
const (
	CompType0  uint8 = 0
	CompType1  uint8 = 1
	CompType2  uint8 = 2
	CompType3  uint8 = 3
	CompNone   uint8 = 4
	CompSelf   uint8 = 5
	CompParent uint8 = 6

	compRLESmall   uint8 = 7
	compRLELarge   uint8 = 8
	compSelf0      uint8 = 9
	compSelf1      uint8 = 10
	compParentSelf uint8 = 11
	compParent0    uint8 = 12
	compParent1    uint8 = 13
)

const (
	mapHeaderSize = 16
	mapTreeCodes  = 16
	mapTreeBits   = 8
	// maxRLERun is the most hunks one RLE_LARGE code can repeat.
	maxRLERun = 3 + 16 + 255
)

// MapEntry describes where one hunk lives.
//
// For CompType0..3 and CompNone, Offset and CompLen locate the stored bytes
// (CompNone has CompLen equal to the hunk size) and CRC16 is the CRC of the
// uncompressed hunk. For CompSelf, Offset is the index of the hunk holding
// the same data. In an uncompressed file, an all-zero hunk that has no
// stored data is a CompNone entry with CompLen 0.
type MapEntry struct {
	Type    uint8
	CompLen uint32
	Offset  uint64
	CRC16   uint16
}

// ReadMap reads the hunk map. fileSize bounds every length and offset read
// from the file. Errors wrap chderr.CorruptData.
func ReadMap(r io.ReaderAt, h Header, fileSize int64) ([]MapEntry, error) {
	if !h.Compressed() {
		return readUncompressedMap(r, h, fileSize)
	}
	return readCompressedMap(r, h, fileSize)
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", chderr.CorruptData, fmt.Sprintf(format, args...))
}

func readUncompressedMap(r io.ReaderAt, h Header, fileSize int64) ([]MapEntry, error) {
	count := uint64(h.HunkCount())
	if h.MapOffset > uint64(fileSize) || count*4 > uint64(fileSize)-h.MapOffset {
		return nil, corrupt("uncompressed map of %d hunks does not fit in the file", count)
	}
	raw := make([]byte, count*4)
	if _, err := r.ReadAt(raw, int64(h.MapOffset)); err != nil {
		return nil, corrupt("reading map: %v", err)
	}
	entries := make([]MapEntry, count)
	for i := range entries {
		v := binary.BigEndian.Uint32(raw[i*4:])
		if v == 0 { // no stored data: an all-zero hunk
			entries[i] = MapEntry{Type: CompNone}
			continue
		}
		off := uint64(v) * uint64(h.HunkBytes)
		if off+uint64(h.HunkBytes) > uint64(fileSize) {
			return nil, corrupt("hunk %d at offset %d is past the end of the file", i, off)
		}
		entries[i] = MapEntry{Type: CompNone, CompLen: h.HunkBytes, Offset: off}
	}
	return entries, nil
}

func readCompressedMap(r io.ReaderAt, h Header, fileSize int64) ([]MapEntry, error) {
	var hdr [mapHeaderSize]byte
	if h.MapOffset > uint64(fileSize) || uint64(fileSize)-h.MapOffset < mapHeaderSize {
		return nil, corrupt("map header is past the end of the file")
	}
	if _, err := r.ReadAt(hdr[:], int64(h.MapOffset)); err != nil {
		return nil, corrupt("reading map header: %v", err)
	}
	mapBytes := binary.BigEndian.Uint32(hdr[0:])
	firstOffset := binary.BigEndian.Uint64(hdr[2:]) & (1<<48 - 1)
	mapCRC := binary.BigEndian.Uint16(hdr[10:])
	lengthBits, selfBits, parentBits := int(hdr[12]), int(hdr[13]), int(hdr[14])
	if uint64(mapBytes) > uint64(fileSize)-h.MapOffset-mapHeaderSize {
		return nil, corrupt("map of %d bytes does not fit in the file", mapBytes)
	}
	if lengthBits > 32 || selfBits > 32 || parentBits > 48 {
		return nil, corrupt("bad map field widths %d/%d/%d", lengthBits, selfBits, parentBits)
	}
	// An RLE_LARGE group (three codes of at least one bit each) is the
	// cheapest way to describe hunks: maxRLERun of them. A map of all-zero or
	// all-self hunks really is that small, so this bound is loose by design;
	// the decoding below commits memory only as map bits are consumed.
	count := uint64(h.HunkCount())
	if count > (uint64(mapBytes)*8/3+1)*maxRLERun {
		return nil, corrupt("map of %d bytes cannot describe %d hunks", mapBytes, count)
	}
	body := make([]byte, mapBytes)
	if _, err := r.ReadAt(body, int64(h.MapOffset)+mapHeaderSize); err != nil {
		return nil, corrupt("reading map: %v", err)
	}
	br := bitstream.NewReader(body)
	dec := huffman.New(mapTreeCodes, mapTreeBits)
	if err := dec.ImportTreeRLE(br); err != nil {
		return nil, fmt.Errorf("map tree: %w", err)
	}

	// First decode the per-hunk types, undoing the RLE.
	// The slices grow as bits are consumed, not to the count the header
	// claims, so a header that overstates the hunks costs only the map's size.
	var types []uint8
	var last uint8
	rep := 0
	for uint64(len(types)) < count {
		if rep > 0 {
			rep--
		} else {
			switch v := uint8(dec.Decode(br)); v {
			case compRLESmall:
				rep = 2 + int(dec.Decode(br))
			case compRLELarge:
				rep = 2 + 16 + int(dec.Decode(br))<<4
				rep += int(dec.Decode(br))
			default:
				last = v
			}
			if br.Overflow() {
				return nil, corrupt("map data ended early")
			}
		}
		types = append(types, last)
	}

	// Then the per-type payload.
	entries := make([]MapEntry, 0, min(count, 1<<16))
	cur := firstOffset
	var lastSelf uint32
	for i, typ := range types {
		e := MapEntry{Offset: cur}
		switch typ {
		case CompType0, CompType1, CompType2, CompType3:
			if h.Codecs[typ] == CodecNone {
				return nil, corrupt("hunk %d uses unconfigured codec %d", i, typ)
			}
			e.Type = typ
			e.CompLen = br.Read(lengthBits)
			e.CRC16 = uint16(br.Read(16))
			cur += uint64(e.CompLen)
		case CompNone:
			e.Type, e.CompLen = CompNone, h.HunkBytes
			e.CRC16 = uint16(br.Read(16))
			cur += uint64(e.CompLen)
		case CompSelf:
			lastSelf = br.Read(selfBits)
			e.Type, e.Offset = CompSelf, uint64(lastSelf)
		case compSelf1:
			lastSelf++
			fallthrough
		case compSelf0:
			e.Type, e.Offset = CompSelf, uint64(lastSelf)
		default: // parents are rejected with the header, and 14-15 do not exist
			return nil, corrupt("hunk %d has unsupported map type %d", i, typ)
		}
		if e.Type == CompSelf {
			if e.Offset >= count {
				return nil, corrupt("hunk %d refers to hunk %d", i, e.Offset)
			}
		} else if e.Offset > uint64(fileSize) || uint64(e.CompLen) > uint64(fileSize)-e.Offset {
			return nil, corrupt("hunk %d at %d+%d is past the end of the file", i, e.Offset, e.CompLen)
		}
		if br.Overflow() {
			return nil, corrupt("map data ended early")
		}
		entries = append(entries, e)
	}
	if br.Overflow() {
		return nil, corrupt("map data ended early")
	}
	if crc := CRC16(rawMap(entries)); crc != mapCRC {
		return nil, fmt.Errorf("%w: map CRC %04x, want %04x", chderr.CorruptData, crc, mapCRC)
	}
	return entries, nil
}

// rawMap returns the 12-byte-per-entry form of the decoded map that the map
// CRC covers: type, 24-bit length, 48-bit offset, CRC-16.
func rawMap(entries []MapEntry) []byte {
	raw := make([]byte, 12*len(entries))
	for i, e := range entries {
		b := raw[i*12:]
		b[0] = e.Type
		b[1], b[2], b[3] = byte(e.CompLen>>16), byte(e.CompLen>>8), byte(e.CompLen)
		for k := range 6 {
			b[4+k] = byte(e.Offset >> (8 * (5 - k)))
		}
		binary.BigEndian.PutUint16(b[10:], e.CRC16)
	}
	return raw
}

// bitsFor returns the number of bits needed to represent 0..v.
func bitsFor(v uint64) int {
	n := 0
	for ; v != 0; v >>= 1 {
		n++
	}
	return n
}

// EncodeCompressedMap encodes entries as a v5 compressed map: the 16-byte map
// header followed by the body. firstOffset is the file offset of the first
// stored hunk. Parent entries are not supported.
func EncodeCompressedMap(entries []MapEntry, firstOffset uint64) ([]byte, error) {
	enc := huffman.New(mapTreeCodes, mapTreeBits)
	enc.HistoReset()

	// RLE-compress the types, promoting self references to compact forms.
	rle := make([]uint8, 0, len(entries))
	emit := func(sym uint8) {
		rle = append(rle, sym)
		enc.HistoOne(uint32(sym))
	}
	var maxSelf uint64
	var lastSelf uint32
	var maxLen uint32
	var lastComp uint8
	count := 0
	for i, e := range entries {
		cur := e.Type
		switch cur {
		case CompSelf:
			ref := e.Offset
			if ref > 1<<32-1 {
				return nil, fmt.Errorf("format: hunk %d self reference %d out of range", i, ref)
			}
			switch uint32(ref) {
			case lastSelf:
				cur = compSelf0
			case lastSelf + 1:
				cur = compSelf1
			default:
				maxSelf = max(maxSelf, ref)
			}
			lastSelf = uint32(ref)
		case CompType0, CompType1, CompType2, CompType3, CompNone:
			maxLen = max(maxLen, e.CompLen)
		default:
			return nil, fmt.Errorf("format: hunk %d has unsupported map type %d", i, e.Type)
		}

		if cur == lastComp {
			count++
		}
		if cur != lastComp || i == len(entries)-1 {
			for count != 0 {
				switch {
				case count < 3:
					emit(lastComp)
					count--
				case count <= 3+15:
					emit(compRLESmall)
					emit(uint8(count - 3))
					count = 0
				default:
					n := min(count, maxRLERun)
					emit(compRLELarge)
					emit(uint8((n - 3 - 16) >> 4))
					emit(uint8((n - 3 - 16) & 15))
					count -= n
				}
			}
			if cur != lastComp {
				lastComp = cur
				emit(cur)
			}
		}
	}

	lengthBits, selfBits := bitsFor(uint64(maxLen)), bitsFor(maxSelf)
	bw := bitstream.NewWriter()
	if err := enc.ComputeTree(); err != nil {
		return nil, err
	}
	if err := enc.ExportTreeRLE(bw); err != nil {
		return nil, err
	}
	for _, sym := range rle {
		enc.Encode(bw, uint32(sym))
	}

	// Replay the RLE stream to write each entry's payload.
	lastComp, count = 0, 0
	src := 0
	for _, e := range entries {
		if count == 0 {
			switch v := rle[src]; v {
			case compRLESmall:
				count = 2 + int(rle[src+1])
				src += 2
			case compRLELarge:
				count = 2 + 16 + int(rle[src+1])<<4 + int(rle[src+2])
				src += 3
			default:
				lastComp = v
				src++
			}
		} else {
			count--
		}
		switch lastComp {
		case CompType0, CompType1, CompType2, CompType3:
			bw.Write(e.CompLen, lengthBits)
			bw.Write(uint32(e.CRC16), 16)
		case CompNone:
			bw.Write(uint32(e.CRC16), 16)
		case CompSelf:
			bw.Write(uint32(e.Offset), selfBits)
		}
	}

	body := bw.Bytes()
	out := make([]byte, mapHeaderSize, mapHeaderSize+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	for k := range 6 {
		out[4+k] = byte(firstOffset >> (8 * (5 - k)))
	}
	binary.BigEndian.PutUint16(out[10:], CRC16(rawMap(entries)))
	out[12], out[13] = byte(lengthBits), byte(selfBits)
	return append(out, body...), nil
}
