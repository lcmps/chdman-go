package bitstream

import "testing"

func TestBitstreamMSBFirst(t *testing.T) {
	w := NewWriter()
	w.Write(0b101, 3)
	w.Write(0xFF, 8)
	b := w.Bytes()
	if len(b) != 2 || b[0] != 0b10111111 || b[1] != 0b11100000 {
		t.Fatalf("bytes = %08b", b)
	}
	r := NewReader(b)
	if got := r.Read(3); got != 0b101 {
		t.Fatalf("first read = %b", got)
	}
	if got := r.Read(8); got != 0xFF {
		t.Fatalf("second read = %x", got)
	}
	if r.Overflow() {
		t.Fatal("unexpected overflow")
	}
}

func TestWideValues(t *testing.T) {
	w := NewWriter()
	w.Write(0xDEADBEEF, 32)
	w.Write(1, 1)
	w.Write(0x12345, 20)
	r := NewReader(w.Bytes())
	if got := r.Read(32); got != 0xDEADBEEF {
		t.Fatalf("got %x", got)
	}
	if got := r.Read(1); got != 1 {
		t.Fatalf("got %x", got)
	}
	if got := r.Read(20); got != 0x12345 {
		t.Fatalf("got %x", got)
	}
}

func TestPeekDoesNotAdvance(t *testing.T) {
	r := NewReader([]byte{0xA5, 0x0F})
	if r.Peek(4) != 0xA || r.Peek(12) != 0xA50 {
		t.Fatal("peek mismatch")
	}
	r.Remove(4)
	if got := r.Read(8); got != 0x50 {
		t.Fatalf("got %x", got)
	}
	if r.Peek(0) != 0 {
		t.Fatal("peek(0) must be 0")
	}
}

func TestOverflow(t *testing.T) {
	r := NewReader([]byte{0xFF})
	if got := r.Read(8); got != 0xFF || r.Overflow() {
		t.Fatalf("got %x overflow=%v", got, r.Overflow())
	}
	if got := r.Read(4); got != 0 {
		t.Fatalf("past-the-end bits must be zero, got %x", got)
	}
	if !r.Overflow() {
		t.Fatal("expected overflow")
	}
}
