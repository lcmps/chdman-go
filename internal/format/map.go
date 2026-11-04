package format

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

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

// Map is a decoded hunk map. Runs of hunks that refer to earlier hunks in a
// regular way are held as one run, so a map that RLE-compresses millions of
// hunks into a few kilobytes stays that small in memory.
type Map struct {
	runs  []mapRun
	count uint32
}

// mapRun is count consecutive hunks starting at first. A stored hunk is a run
// of one. For a CompSelf run the hunk first+k refers to hunk offset+step*k,
// and step is 0 or 1.
type mapRun struct {
	first, count uint32
	offset       uint64
	compLen      uint32
	crc16        uint16
	typ, step    uint8
}

// Len returns the number of hunks.
func (m *Map) Len() uint32 { return m.count }

// Entry returns the entry of hunk i, which must be below Len. A CompSelf
// entry's Offset is the index of the hunk that holds the data: it never refers
// to another CompSelf hunk.
func (m *Map) Entry(i uint32) MapEntry {
	r := m.find(i)
	if r.typ == CompSelf {
		return MapEntry{Type: CompSelf, Offset: r.offset + uint64(r.step)*uint64(i-r.first)}
	}
	return MapEntry{Type: r.typ, CompLen: r.compLen, Offset: r.offset, CRC16: r.crc16}
}

// StoredBytes returns the total length of the hunks' stored data. Hunks that
// refer to another hunk add nothing.
func (m *Map) StoredBytes() int64 {
	var n int64
	for _, r := range m.runs {
		if r.typ != CompSelf {
			n += int64(r.compLen) * int64(r.count)
		}
	}
	return n
}

// find returns the run holding hunk i.
func (m *Map) find(i uint32) *mapRun {
	k := sort.Search(len(m.runs), func(k int) bool { return m.runs[k].first > i })
	return &m.runs[k-1]
}

func (m *Map) addStored(e MapEntry) {
	m.runs = append(m.runs, mapRun{first: m.count, count: 1, typ: e.Type, offset: e.Offset, compLen: e.CompLen, crc16: e.CRC16})
	m.count++
}

// addSelf appends n hunks whose k'th refers to hunk ref+step*k. The
// references must be to earlier hunks. They are stored pointing at the hunks
// that hold the data, so a chain of references collapses.
func (m *Map) addSelf(ref uint64, n uint32, step uint8) error {
	if ref >= uint64(m.count) {
		return corrupt("hunk %d refers to hunk %d", m.count, ref)
	}
	for done := uint32(0); done < n; {
		// Resolve the next target: a hunk of a run already in the map.
		x := ref + uint64(step)*uint64(done)
		r := m.find(uint32(x))
		piece, flatRef, flatStep := n-done, x, uint8(1)
		switch {
		case r.typ != CompSelf: // a stored hunk is its own data
			piece = 1
			if step == 0 {
				piece, flatStep = n-done, 0
			}
		case step == 0: // every hunk resolves to the same hunk
			flatRef, flatStep = r.offset+uint64(r.step)*(x-uint64(r.first)), 0
		default:
			flatRef, flatStep = r.offset+uint64(r.step)*(x-uint64(r.first)), r.step
			piece = min(piece, r.first+r.count-uint32(x))
		}
		m.appendSelfRun(flatRef, piece, flatStep)
		done += piece
	}
	return nil
}

// appendSelfRun adds n resolved references, merging them into the previous
// run when they continue it.
func (m *Map) appendSelfRun(ref uint64, n uint32, step uint8) {
	if n == 1 {
		step = 0
	}
	if k := len(m.runs) - 1; k >= 0 && m.runs[k].typ == CompSelf {
		last := &m.runs[k]
		// The step the merged run would have, if there is one.
		var t uint8
		ok := false
		if last.count == 1 {
			if d := ref - last.offset; ref >= last.offset && d <= 1 {
				t, ok = uint8(d), n == 1 || step == uint8(d)
			}
		} else {
			t = last.step
			ok = ref == last.offset+uint64(t)*uint64(last.count) && (n == 1 || step == t)
		}
		if ok {
			last.count += n
			last.step = t
			m.count += n
			return
		}
	}
	m.runs = append(m.runs, mapRun{first: m.count, count: n, offset: ref, typ: CompSelf, step: step})
	m.count += n
}

// ReadMap reads the hunk map. fileSize bounds every length and offset read
// from the file. Errors wrap chderr.CorruptData.
func ReadMap(r io.ReaderAt, h Header, fileSize int64) (*Map, error) {
	if !h.Compressed() {
		return readUncompressedMap(r, h, fileSize)
	}
	return readCompressedMap(r, h, fileSize)
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", chderr.CorruptData, fmt.Sprintf(format, args...))
}

func readUncompressedMap(r io.ReaderAt, h Header, fileSize int64) (*Map, error) {
	count := uint64(h.HunkCount())
	if h.MapOffset > uint64(fileSize) || count*4 > uint64(fileSize)-h.MapOffset {
		return nil, corrupt("uncompressed map of %d hunks does not fit in the file", count)
	}
	raw := make([]byte, count*4)
	if err := ReadFullAt(r, raw, int64(h.MapOffset)); err != nil {
		return nil, corrupt("reading map: %v", err)
	}
	m := &Map{runs: make([]mapRun, 0, count)}
	for i := range count {
		v := binary.BigEndian.Uint32(raw[i*4:])
		if v == 0 { // no stored data: an all-zero hunk
			m.addStored(MapEntry{Type: CompNone})
			continue
		}
		off := uint64(v) * uint64(h.HunkBytes)
		if off+uint64(h.HunkBytes) > uint64(fileSize) {
			return nil, corrupt("hunk %d at offset %d is past the end of the file", i, off)
		}
		m.addStored(MapEntry{Type: CompNone, CompLen: h.HunkBytes, Offset: off})
	}
	return m, nil
}

// typeRun is n consecutive hunks of one map type, packed as type<<9 | n to
// keep the list of runs small. n is at most maxRLERun, which is below 1<<9.
type typeRun uint16

func (t typeRun) typ() uint8 { return uint8(t >> 9) }
func (t typeRun) n() uint32  { return uint32(t & 0x1ff) }

func readCompressedMap(r io.ReaderAt, h Header, fileSize int64) (*Map, error) {
	var hdr [mapHeaderSize]byte
	if h.MapOffset > uint64(fileSize) || uint64(fileSize)-h.MapOffset < mapHeaderSize {
		return nil, corrupt("map header is past the end of the file")
	}
	if err := ReadFullAt(r, hdr[:], int64(h.MapOffset)); err != nil {
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
	// all-self hunks really is that small, so this bound is loose by design.
	// Everything below holds runs, not hunks, so memory follows the map's size.
	count := uint64(h.HunkCount())
	if count > (uint64(mapBytes)*8/3+1)*maxRLERun {
		return nil, corrupt("map of %d bytes cannot describe %d hunks", mapBytes, count)
	}
	body := make([]byte, mapBytes)
	if err := ReadFullAt(r, body, int64(h.MapOffset)+mapHeaderSize); err != nil {
		return nil, corrupt("reading map: %v", err)
	}
	br := bitstream.NewReader(body)
	dec := huffman.New(mapTreeCodes, mapTreeBits)
	if err := dec.ImportTreeRLE(br); err != nil {
		return nil, fmt.Errorf("map tree: %w", err)
	}

	// First decode the per-hunk types, undoing the RLE. Each code adds at most
	// maxRLERun hunks to one run, and costs at least a bit of the map.
	var types []typeRun
	var last uint8
	for total := uint64(0); total < count; {
		n := uint32(1)
		switch v := uint8(dec.Decode(br)); v {
		case compRLESmall:
			n += 2 + uint32(dec.Decode(br))
		case compRLELarge:
			n += 2 + 16 + uint32(dec.Decode(br))<<4
			n += uint32(dec.Decode(br))
		default:
			last = v
		}
		if br.Overflow() {
			return nil, corrupt("map data ended early")
		}
		n = uint32(min(uint64(n), count-total))
		types = append(types, typeRun(last)<<9|typeRun(n))
		total += uint64(n)
	}

	// Then the per-type payload. The map CRC covers the 12-byte-per-hunk form
	// of the entries as stored in the file: type, 24-bit length, 48-bit
	// offset, CRC-16.
	m := &Map{runs: make([]mapRun, 0, min(len(types), 1<<16))}
	crc := uint16(crc16Init)
	var raw [12]byte
	cur := firstOffset
	var lastSelf uint32
	// addSelf adds self references as the file states them; the map resolves
	// them, while the CRC covers them as written.
	addSelf := func(ref uint64, n uint32, step uint8) error {
		if err := m.addSelf(ref, n, step); err != nil {
			return err
		}
		for k := range uint64(n) {
			putRawEntry(&raw, MapEntry{Type: CompSelf, Offset: ref + uint64(step)*k})
			crc = crc16Update(crc, raw[:])
		}
		return nil
	}
	for _, run := range types {
		typ, n := run.typ(), run.n()
		switch typ {
		case CompType0, CompType1, CompType2, CompType3, CompNone:
			if typ != CompNone && h.Codecs[typ] == CodecNone {
				return nil, corrupt("hunk %d uses unconfigured codec %d", m.count, typ)
			}
			for range n {
				e := MapEntry{Type: typ, Offset: cur, CompLen: h.HunkBytes}
				if typ != CompNone {
					e.CompLen = br.Read(lengthBits)
				}
				e.CRC16 = uint16(br.Read(16))
				if br.Overflow() {
					return nil, corrupt("map data ended early")
				}
				if e.Offset > uint64(fileSize) || uint64(e.CompLen) > uint64(fileSize)-e.Offset {
					return nil, corrupt("hunk %d at %d+%d is past the end of the file", m.count, e.Offset, e.CompLen)
				}
				cur += uint64(e.CompLen)
				putRawEntry(&raw, e)
				crc = crc16Update(crc, raw[:])
				m.addStored(e)
			}
		case CompSelf:
			for range n {
				lastSelf = br.Read(selfBits)
				if br.Overflow() {
					return nil, corrupt("map data ended early")
				}
				if err := addSelf(uint64(lastSelf), 1, 0); err != nil {
					return nil, err
				}
			}
		case compSelf0:
			if err := addSelf(uint64(lastSelf), n, 0); err != nil {
				return nil, err
			}
		case compSelf1:
			if err := addSelf(uint64(lastSelf+1), n, 1); err != nil {
				return nil, err
			}
			lastSelf += n
		default: // parents are rejected with the header, and 14-15 do not exist
			return nil, corrupt("hunk %d has unsupported map type %d", m.count, typ)
		}
	}
	if br.Overflow() {
		return nil, corrupt("map data ended early")
	}
	if crc != mapCRC {
		return nil, fmt.Errorf("%w: map CRC %04x, want %04x", chderr.CorruptData, crc, mapCRC)
	}
	return m, nil
}

// putRawEntry writes e in the 12-byte form the map CRC covers.
func putRawEntry(b *[12]byte, e MapEntry) {
	b[0] = e.Type
	b[1], b[2], b[3] = byte(e.CompLen>>16), byte(e.CompLen>>8), byte(e.CompLen)
	for k := range 6 {
		b[4+k] = byte(e.Offset >> (8 * (5 - k)))
	}
	binary.BigEndian.PutUint16(b[10:], e.CRC16)
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
	binary.BigEndian.PutUint16(out[10:], mapCRC(entries))
	out[12], out[13] = byte(lengthBits), byte(selfBits)
	return append(out, body...), nil
}

// mapCRC returns the CRC-16 of the 12-byte-per-hunk form of entries.
func mapCRC(entries []MapEntry) uint16 {
	crc := uint16(crc16Init)
	var raw [12]byte
	for _, e := range entries {
		putRawEntry(&raw, e)
		crc = crc16Update(crc, raw[:])
	}
	return crc
}
