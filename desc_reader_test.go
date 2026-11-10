package mp4merge

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestSampleTableDecodesSTZ2(t *testing.T) {
	tests := []struct {
		name      string
		fieldSize byte
		count     uint32
		data      []byte
		want      []uint32
	}{
		{name: "4 bit", fieldSize: 4, count: 4, data: []byte{0xab, 0xc0}, want: []uint32{10, 11, 12, 0}},
		{name: "8 bit", fieldSize: 8, count: 3, data: []byte{1, 2, 255}, want: []uint32{1, 2, 255}},
		{name: "16 bit", fieldSize: 16, count: 2, data: []byte{0x12, 0x34, 0x56, 0x78}, want: []uint32{0x1234, 0x5678}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := make([]byte, 12+len(tt.data))
			body[7] = tt.fieldSize
			binary.BigEndian.PutUint32(body[8:12], tt.count)
			copy(body[12:], tt.data)
			var track partTrack
			if err := sampleTable(fourCC("stz2"), body, &track); err != nil {
				t.Fatalf("sampleTable returned an error: %v", err)
			}
			if track.stszCount != tt.count {
				t.Fatalf("sample count = %d, want %d", track.stszCount, tt.count)
			}
			if !reflect.DeepEqual(track.stsz, tt.want) {
				t.Fatalf("sample sizes = %v, want %v", track.stsz, tt.want)
			}
		})
	}
}
