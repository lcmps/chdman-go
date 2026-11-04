// Package huffman implements the static Huffman coding used by CHD's huff
// codec and by the CD codec's frame headers.
//
// Translated from MAME's src/lib/util/huffman.cpp and huffman.h
// (BSD-3-Clause, copyright Aaron Giles and contributors); see NOTICE. The
// tree construction must match MAME's exactly so real chdman files decode.
package huffman

import (
	"fmt"
	"slices"

	"github.com/lcmps/chdman-go/internal/bitstream"
	"github.com/lcmps/chdman-go/internal/chderr"
)

// Errors mirror MAME's huffman_error values. The data errors wrap
// chderr.CorruptData.
var (
	ErrInvalidData           = fmt.Errorf("huffman: invalid data: %w", chderr.CorruptData)
	ErrInputBufferTooSmall   = fmt.Errorf("huffman: input buffer too small: %w", chderr.CorruptData)
	ErrInternalInconsistency = fmt.Errorf("huffman: internal inconsistency: %w", chderr.CorruptData)
)

const maxSupportedBits = 24

type node struct {
	parent  int // index into nodes, -1 for none
	count   uint32
	weight  uint32
	bits    uint32
	numBits uint8
}

// Context is a Huffman encoder and decoder over numCodes symbols with codes of
// at most maxBits bits.
type Context struct {
	numCodes int
	maxBits  int
	lookup   []uint32 // decoder table: code<<5 | length
	histo    []uint32
	nodes    []node // 2*numCodes: leaves, then interior nodes
}

// New returns a Context. It panics if maxBits is outside 1..24, which is a
// programming error (MAME throws HUFFERR_TOO_MANY_BITS).
func New(numCodes, maxBits int) *Context {
	if maxBits < 1 || maxBits > maxSupportedBits || numCodes < 1 {
		panic("huffman: invalid context size")
	}
	return &Context{
		numCodes: numCodes,
		maxBits:  maxBits,
		lookup:   make([]uint32, 1<<uint(maxBits)),
		histo:    make([]uint32, numCodes),
		nodes:    make([]node, numCodes*2),
	}
}

func (c *Context) rleNumBits() int {
	switch {
	case c.maxBits >= 16:
		return 5
	case c.maxBits >= 8:
		return 4
	default:
		return 3
	}
}

// rleFullBits is the width of the largest RLE count in the Huffman tree format.
func (c *Context) rleFullBits() int {
	n := 0
	for t := uint32(c.numCodes - 9); t != 0; t >>= 1 {
		n++
	}
	return n
}

// ImportTreeRLE reads an RLE-encoded tree.
func (c *Context) ImportTreeRLE(r *bitstream.Reader) error {
	numBits := c.rleNumBits()
	cur := 0
	for cur < c.numCodes {
		nodeBits := int(r.Read(numBits))
		if nodeBits != 1 {
			c.nodes[cur].numBits = uint8(nodeBits)
			cur++
			continue
		}
		// a one is an escape code; a double one is a single one
		nodeBits = int(r.Read(numBits))
		if nodeBits == 1 {
			c.nodes[cur].numBits = 1
			cur++
			continue
		}
		rep := int(r.Read(numBits)) + 3
		if cur+rep > c.numCodes {
			return ErrInvalidData
		}
		for ; rep > 0; rep-- {
			c.nodes[cur].numBits = uint8(nodeBits)
			cur++
		}
	}
	if err := c.assignCanonicalCodes(); err != nil {
		return err
	}
	c.buildLookupTable()
	if r.Overflow() {
		return ErrInputBufferTooSmall
	}
	return nil
}

// ExportTreeRLE writes the tree in RLE form.
func (c *Context) ExportTreeRLE(w *bitstream.Writer) error {
	numBits := c.rleNumBits()
	last := -1
	rep := 0
	for cur := 0; cur < c.numCodes; cur++ {
		v := int(c.nodes[cur].numBits)
		if v == last {
			rep++
			continue
		}
		if rep != 0 {
			writeRLETreeBits(w, last, rep, numBits)
		}
		last = v
		rep = 1
	}
	writeRLETreeBits(w, last, rep, numBits)
	return nil
}

func writeRLETreeBits(w *bitstream.Writer, value, rep, numBits int) {
	for rep > 0 {
		switch {
		case value == 1: // a one is an escape code, so write it twice
			w.Write(1, numBits)
			w.Write(1, numBits)
			rep--
		case rep <= 2:
			w.Write(uint32(value), numBits)
			rep--
		default:
			cur := min(rep-3, 1<<uint(numBits)-1)
			w.Write(1, numBits)
			w.Write(uint32(value), numBits)
			w.Write(uint32(cur), numBits)
			rep -= cur + 3
		}
	}
}

// ImportTreeHuffman reads a tree that is itself Huffman-coded.
func (c *Context) ImportTreeHuffman(r *bitstream.Reader) error {
	small := New(24, 6)
	small.nodes[0].numBits = uint8(r.Read(3))
	start := int(r.Read(3)) + 1
	count := 0
	for i := 1; i < 24; i++ {
		if i < start || count == 7 {
			small.nodes[i].numBits = 0
			continue
		}
		count = int(r.Read(3))
		if count == 7 {
			small.nodes[i].numBits = 0
		} else {
			small.nodes[i].numBits = uint8(count)
		}
	}
	if err := small.assignCanonicalCodes(); err != nil {
		return err
	}
	small.buildLookupTable()

	rleFullBits := c.rleFullBits()
	last := 0
	cur := 0
	for cur < c.numCodes {
		value := int(small.Decode(r))
		if value != 0 {
			last = value - 1
			c.nodes[cur].numBits = uint8(last)
			cur++
			continue
		}
		count := int(r.Read(3)) + 2
		if count == 7+2 {
			count += int(r.Read(rleFullBits))
		}
		for ; count != 0 && cur < c.numCodes; count-- {
			c.nodes[cur].numBits = uint8(last)
			cur++
		}
	}
	if err := c.assignCanonicalCodes(); err != nil {
		return err
	}
	c.buildLookupTable()
	if r.Overflow() {
		return ErrInputBufferTooSmall
	}
	return nil
}

// ExportTreeHuffman writes the tree Huffman-coded.
func (c *Context) ExportTreeHuffman(w *bitstream.Writer) error {
	rleData := make([]uint8, 0, c.numCodes)
	rleLengths := make([]uint16, 0, c.numCodes/3)
	small := New(24, 6)
	last := -1
	rep := 0

	flush := func() {
		if rep == 1 {
			rleData = append(rleData, uint8(last+1))
		} else {
			rleData = append(rleData, 0)
			rleLengths = append(rleLengths, uint16(rep-2))
		}
	}
	for cur := 0; cur < c.numCodes; cur++ {
		v := int(c.nodes[cur].numBits)
		if v+1 >= small.numCodes {
			return ErrInternalInconsistency
		}
		if v != last && rep > 0 {
			flush()
		}
		if v == last {
			rep++
		} else {
			rleData = append(rleData, uint8(v+1))
			last = v
			rep = 0
		}
	}
	if rep > 0 {
		flush()
	}
	for _, d := range rleData {
		small.HistoOne(uint32(d))
	}
	if err := small.ComputeTree(); err != nil {
		return err
	}

	firstNonZero, lastNonZero := 31, 0
	for i := 1; i < small.numCodes; i++ {
		if small.nodes[i].numBits != 0 {
			if firstNonZero == 31 {
				firstNonZero = i
			}
			lastNonZero = i
		}
	}
	firstNonZero = min(firstNonZero, 8)

	w.Write(uint32(small.nodes[0].numBits), 3)
	w.Write(uint32(firstNonZero-1), 3)
	for i := firstNonZero; i <= lastNonZero; i++ {
		w.Write(uint32(small.nodes[i].numBits), 3)
	}
	w.Write(7, 3)

	rleFullBits := c.rleFullBits()
	li := 0
	for _, d := range rleData {
		small.Encode(w, uint32(d))
		if d == 0 {
			n := int(rleLengths[li])
			li++
			if n < 7 {
				w.Write(uint32(n), 3)
			} else {
				w.Write(7, 3)
				w.Write(uint32(n-7), rleFullBits)
			}
		}
	}
	return nil
}

// HistoReset clears the symbol histogram.
func (c *Context) HistoReset() { clear(c.histo) }

// HistoOne counts one occurrence of sym.
func (c *Context) HistoOne(sym uint32) { c.histo[sym]++ }

// ComputeTree builds the code lengths and canonical codes from the histogram.
func (c *Context) ComputeTree() error {
	var total uint32
	for _, h := range c.histo {
		total += h
	}
	// binary search for the weight that yields the best tree within maxBits
	var lower uint32
	upper := total * 2
	for {
		cur := (upper + lower) / 2
		curMax := c.buildTree(total, cur)
		if curMax <= c.maxBits {
			lower = cur
			if cur == total || upper-lower <= 1 {
				break
			}
		} else {
			upper = cur
		}
	}
	return c.assignCanonicalCodes()
}

// buildTree is MAME's build_tree: it keeps the weighting loop and the
// sorted-list insertion exactly so that equal-weight ties resolve identically.
func (c *Context) buildTree(totalData, totalWeight uint32) int {
	list := make([]int, 0, c.numCodes*2)
	clear(c.nodes[:c.numCodes])
	for code := 0; code < c.numCodes; code++ {
		n := &c.nodes[code]
		n.parent = -1
		if c.histo[code] == 0 {
			continue
		}
		list = append(list, code)
		n.count = c.histo[code]
		n.bits = uint32(code)
		// scale by the current effective length, never going to 0
		n.weight = uint32(uint64(c.histo[code]) * uint64(totalWeight) / uint64(totalData))
		if n.weight == 0 {
			n.weight = 1
		}
	}
	// weight descending, then symbol ascending; int32 mirrors MAME's int cast
	slices.SortFunc(list, func(a, b int) int {
		na, nb := &c.nodes[a], &c.nodes[b]
		if na.weight != nb.weight {
			return int(int32(nb.weight - na.weight))
		}
		return int(na.bits) - int(nb.bits)
	})

	next := c.numCodes
	for len(list) > 1 {
		n1 := list[len(list)-1]
		n0 := list[len(list)-2]
		list = list[:len(list)-2]

		nn := next
		next++
		c.nodes[nn].parent = -1
		c.nodes[n0].parent = nn
		c.nodes[n1].parent = nn
		c.nodes[nn].weight = c.nodes[n0].weight + c.nodes[n1].weight

		pos := 0
		for ; pos < len(list); pos++ {
			if c.nodes[nn].weight > c.nodes[list[pos]].weight {
				break
			}
		}
		list = slices.Insert(list, pos, nn)
	}

	maxBits := 0
	for code := 0; code < c.numCodes; code++ {
		n := &c.nodes[code]
		n.numBits = 0
		n.bits = 0
		if n.weight > 0 {
			for cur := n; cur.parent != -1; cur = &c.nodes[cur.parent] {
				n.numBits++
			}
			if n.numBits == 0 {
				n.numBits = 1
			}
			maxBits = max(maxBits, int(n.numBits))
		}
	}
	return maxBits
}

func (c *Context) assignCanonicalCodes() error {
	var bitHisto [33]uint32
	for code := 0; code < c.numCodes; code++ {
		n := &c.nodes[code]
		if int(n.numBits) > c.maxBits {
			return ErrInternalInconsistency
		}
		if n.numBits <= 32 {
			bitHisto[n.numBits]++
		}
	}
	var curStart uint32
	for l := 32; l > 0; l-- {
		nextStart := (curStart + bitHisto[l]) >> 1
		if l != 1 && nextStart*2 != curStart+bitHisto[l] {
			return ErrInternalInconsistency
		}
		bitHisto[l] = curStart
		curStart = nextStart
	}
	for code := 0; code < c.numCodes; code++ {
		n := &c.nodes[code]
		if n.numBits > 0 {
			n.bits = bitHisto[n.numBits]
			bitHisto[n.numBits]++
			// An over-subscribed tree hands out codes too wide for their
			// length. chdman does not check this; we reject it so the
			// lookup table is never indexed out of range.
			if n.numBits > 32 || uint64(n.bits) >= 1<<n.numBits {
				return ErrInvalidData
			}
		}
	}
	return nil
}

func (c *Context) buildLookupTable() {
	for code := 0; code < c.numCodes; code++ {
		n := &c.nodes[code]
		if n.numBits == 0 {
			continue
		}
		value := uint32(code)<<5 | uint32(n.numBits)&0x1f
		shift := uint(c.maxBits) - uint(n.numBits)
		lo := n.bits << shift
		hi := (n.bits+1)<<shift - 1
		for i := lo; i <= hi; i++ {
			c.lookup[i] = value
		}
	}
}

// Encode writes the code for sym.
func (c *Context) Encode(w *bitstream.Writer, sym uint32) {
	n := &c.nodes[sym]
	w.Write(n.bits, int(n.numBits))
}

// Decode reads one symbol. Codes absent from the tree decode as symbol 0
// consuming no bits, as in MAME.
func (c *Context) Decode(r *bitstream.Reader) uint32 {
	v := c.lookup[r.Peek(c.maxBits)]
	r.Remove(int(v & 0x1f))
	return v >> 5
}

// Encode8 compresses src with a freshly computed 8-bit tree in the huff
// codec payload format (huffman_8bit_encoder::encode).
func Encode8(src []byte) ([]byte, error) {
	c := New(256, 16)
	for _, b := range src {
		c.HistoOne(uint32(b))
	}
	if err := c.ComputeTree(); err != nil {
		return nil, err
	}
	w := bitstream.NewWriter()
	if err := c.ExportTreeHuffman(w); err != nil {
		return nil, err
	}
	for _, b := range src {
		c.Encode(w, uint32(b))
	}
	return w.Bytes(), nil
}

// Decode8 fills dst from a huff codec payload (huffman_8bit_decoder::decode).
func Decode8(src, dst []byte) error {
	c := New(256, 16)
	r := bitstream.NewReader(src)
	if err := c.ImportTreeHuffman(r); err != nil {
		return err
	}
	for i := range dst {
		dst[i] = byte(c.Decode(r))
	}
	if r.Overflow() {
		return ErrInputBufferTooSmall
	}
	return nil
}
