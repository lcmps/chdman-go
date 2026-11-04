package format

var crc16Table = func() (t [256]uint16) {
	for i := range t {
		crc := uint16(i) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return t
}()

// crc16Init is the initial CRC-16 value.
const crc16Init = 0xffff

// crc16Update continues a CRC-16 over b.
func crc16Update(crc uint16, b []byte) uint16 {
	for _, v := range b {
		crc = crc<<8 ^ crc16Table[byte(crc>>8)^v]
	}
	return crc
}

// CRC16 returns the CRC-16/CCITT-FALSE checksum (polynomial 0x1021, initial
// value 0xffff) that CHD uses for map and hunk checks.
//
// Matches MAME's crc16_creator (BSD-3-Clause); see NOTICE.
func CRC16(b []byte) uint16 { return crc16Update(crc16Init, b) }
