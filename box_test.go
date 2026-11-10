package mp4merge

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestReadBoxRejectsSizeAboveInt64(t *testing.T) {
	var data [16]byte
	binary.BigEndian.PutUint32(data[:4], 1)
	binary.BigEndian.PutUint32(data[4:8], fourCC("free"))
	binary.BigEndian.PutUint64(data[8:], uint64(1)<<63)

	if _, _, _, _, err := ReadBox(bytes.NewReader(data[:])); err == nil {
		t.Fatal("ReadBox accepted a box size larger than MaxInt64")
	}
}
