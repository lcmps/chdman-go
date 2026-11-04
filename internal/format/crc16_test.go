package format

import "testing"

func TestCRC16Vector(t *testing.T) {
	if got := CRC16([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("CRC16 = %#04x, want 0x29b1", got)
	}
}
