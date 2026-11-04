package cdrom

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

func testSector(mode byte) []byte {
	rng := rand.New(rand.NewPCG(1, uint64(mode)))
	s := make([]byte, SectorSize)
	for i := range s {
		s[i] = byte(rng.Uint32())
	}
	copy(s, []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00})
	s[modeOffset] = mode
	return s
}

func TestECCGenerateVerify(t *testing.T) {
	for _, mode := range []byte{1, 2} {
		sector := testSector(mode)
		if ECCVerify(sector) {
			t.Fatalf("mode %d: random sector unexpectedly verifies", mode)
		}
		ECCGenerate(sector)
		if !ECCVerify(sector) {
			t.Fatalf("mode %d: generated sector does not verify", mode)
		}
		want := bytes.Clone(sector)

		for _, pos := range []int{16, 100, 2064, eccPOffset, eccQOffset + 5} {
			bad := bytes.Clone(want)
			bad[pos] ^= 0x01
			if ECCVerify(bad) {
				t.Errorf("mode %d: flipping byte %d still verifies", mode, pos)
			}
		}

		ECCClear(sector)
		if ECCVerify(sector) {
			t.Errorf("mode %d: cleared sector verifies", mode)
		}
		if !bytes.Equal(sector[:eccPOffset], want[:eccPOffset]) {
			t.Errorf("mode %d: ECCClear touched data before the ECC area", mode)
		}
		ECCGenerate(sector)
		if !bytes.Equal(sector, want) {
			t.Errorf("mode %d: ECCClear then ECCGenerate did not restore the sector", mode)
		}
	}
}

func TestECCMode2IgnoresHeader(t *testing.T) {
	sector := testSector(2)
	ECCGenerate(sector)
	sector[12] ^= 0xff // address bytes are masked in mode 2
	if !ECCVerify(sector) {
		t.Error("mode 2 ECC should not depend on the header address bytes")
	}
}
