package mp4merge

import (
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"
)

func fourCC(s string) uint32 {
	if len(s) != 4 {
		panic("fourCC requires 4 byte string")
	}
	return uint32(s[0])<<24 |
		uint32(s[1])<<16 |
		uint32(s[2])<<8 |
		uint32(s[3])
}

func hasChildren(typ uint32, reading bool) bool {
	return typ == fourCC("moov") ||
		typ == fourCC("trak") ||
		typ == fourCC("edts") ||
		typ == fourCC("mdia") ||
		typ == fourCC("minf") ||
		typ == fourCC("stbl") ||
		(typ == fourCC("stsd") && reading)
}

func typeToString(typ uint32) string {
	buf := []byte{
		byte(typ >> 24),
		byte(typ >> 16),
		byte(typ >> 8),
		byte(typ),
	}
	if utf8.Valid(buf) {
		return string(buf)
	}
	return fmt.Sprintf("%08X", typ)
}

func readBox(r io.ReadSeeker) (typ uint32, offset int64, size uint64, headerSize int64, err error) {
	offset, err = r.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}

	var size32 uint32
	if err = binary.Read(r, binary.BigEndian, &size32); err != nil {
		return
	}
	if err = binary.Read(r, binary.BigEndian, &typ); err != nil {
		return
	}

	if size32 == 1 {
		var size64 uint64
		if err = binary.Read(r, binary.BigEndian, &size64); err != nil {
			return
		}
		size = size64
		headerSize = 16
	} else {
		size = uint64(size32)
		headerSize = 8
	}
	return
}
