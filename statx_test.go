package mp4merge

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestDecodeStatxBirthTimeByteOrders(t *testing.T) {
	want := time.Unix(1_700_000_000, 123_456_789)
	for _, tt := range []struct {
		name  string
		order binary.ByteOrder
	}{
		{name: "little endian", order: binary.LittleEndian},
		{name: "big endian", order: binary.BigEndian},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, 256)
			tt.order.PutUint32(data[:4], 0x800)
			tt.order.PutUint64(data[80:88], uint64(want.Unix()))
			tt.order.PutUint32(data[88:92], uint32(want.Nanosecond()))

			got, err := decodeStatxBirthTime(data, tt.order, "fixture")
			if err != nil {
				t.Fatalf("decodeStatxBirthTime returned an error: %v", err)
			}
			if !got.Equal(want) {
				t.Fatalf("birth time = %s, want %s", got, want)
			}
		})
	}
}
