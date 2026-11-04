package format

// CRC16 returns the CRC-16/CCITT-FALSE checksum (polynomial 0x1021, initial
// value 0xffff) that CHD uses for map and hunk checks.
//
// Matches MAME's crc16_creator (BSD-3-Clause); see NOTICE.
func CRC16(b []byte) uint16 {
	crc := uint16(0xffff)
	for _, v := range b {
		crc ^= uint16(v) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
