package cdrom

// UserDataSize is the size of the user data of a data sector.
const UserDataSize = 2048

// UserDataOffset returns where the 2048 bytes of user data start in a stored
// sector of the given track type. ok is false for types with no 2048-byte
// payload: audio and MODE2_FORM2, whose payload is 2324 bytes.
//
// The offsets follow read_data's conversions to MODE1 (cdrom.cpp:488-526):
// 16 behind the sync and header of a raw MODE1 sector, 24 behind the
// subheader too in a raw MODE2 sector, and 8 behind the subheader of a
// 2336-byte MODE2_FORM_MIX sector. Two types are not conversions read_data
// makes. MODE2 (MODE2/2336) is refused by read_data and read at offset 8
// here, like MODE2_FORM_MIX. MODE2_FORM1 (MODE2/2048) is stored as exactly
// the 2048 bytes of payload, so offset 0 is its payload; read_data's offset 24
// would run into the padding.
func UserDataOffset(t TrackType) (off int, ok bool) {
	switch t {
	case MODE1, MODE2_FORM1:
		return 0, true
	case MODE1_RAW:
		return 16, true
	case MODE2, MODE2_FORM_MIX:
		return 8, true
	case MODE2_RAW:
		return 24, true
	}
	return 0, false
}
