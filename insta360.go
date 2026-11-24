package mp4merge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sort"
)

const instaHeaderSize = 32 + 4 + 4 + 32

var instaMagic = []byte("8db42d694ccc418790edff439fe026bf")

type instaRecord struct {
	Offset      uint64
	DataVersion uint32
	ID          uint8
	Format      uint8
	Size        int64
}

func getInsta360Offsets(files []*fileHandle) ([][]instaRecord, error) {
	result := make([][]instaRecord, len(files))

	for i, fh := range files {
		if fh.size < instaHeaderSize {
			return nil, inFile(i, io.ErrUnexpectedEOF)
		}

		currentPos, err := fh.stream.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, inFile(i, err)
		}

		if _, err := fh.stream.Seek(-int64(instaHeaderSize), io.SeekEnd); err != nil {
			return nil, inFile(i, err)
		}

		buf := make([]byte, instaHeaderSize)
		if _, err := io.ReadFull(fh.stream, buf); err != nil {
			return nil, inFile(i, err)
		}
		if bytes.Equal(buf[instaHeaderSize-32:], instaMagic) {
			extraSize := int64(binary.LittleEndian.Uint32(buf[32:36]))
			if extraSize < instaHeaderSize || extraSize > fh.size {
				return nil, inFile(i, mp4Error("invalid data", "invalid Insta360 metadata size %d", extraSize))
			}
			dataVersion := binary.LittleEndian.Uint32(buf[36:40])
			extraStart := fh.size - extraSize

			offset := int64(instaHeaderSize + 6)
			if offset <= extraSize {
				if _, err := fh.stream.Seek(-offset+1, io.SeekEnd); err != nil {
					return nil, inFile(i, err)
				}

				var firstID [1]byte
				if _, err := io.ReadFull(fh.stream, firstID[:]); err != nil {
					return nil, inFile(i, err)
				}

				if firstID[0] == 0 {
					var sizeBuf [4]byte
					if _, err := io.ReadFull(fh.stream, sizeBuf[:]); err != nil {
						return nil, inFile(i, err)
					}
					recordSize := int64(binary.LittleEndian.Uint32(sizeBuf[:]))
					if recordSize < 0 || offset+recordSize > extraSize {
						return nil, inFile(i, mp4Error("invalid data", "invalid Insta360 offsets record size %d", recordSize))
					}
					dataBuf := make([]byte, recordSize)
					if _, err := fh.stream.Seek(-offset-recordSize, io.SeekEnd); err != nil {
						return nil, inFile(i, err)
					}
					if _, err := io.ReadFull(fh.stream, dataBuf); err != nil {
						return nil, inFile(i, err)
					}
					cursor := 0
					for cursor+10 <= len(dataBuf) {
						id := dataBuf[cursor]
						cursor++
						format := dataBuf[cursor]
						cursor++
						size := int64(binary.LittleEndian.Uint32(dataBuf[cursor : cursor+4]))
						cursor += 4
						off := binary.LittleEndian.Uint32(dataBuf[cursor : cursor+4])
						cursor += 4
						if id > 0 {
							result[i] = append(result[i], instaRecord{
								Offset:      uint64(extraStart) + uint64(off),
								DataVersion: dataVersion,
								ID:          id,
								Format:      format,
								Size:        size,
							})
						}
					}
				} else {
					for offset < extraSize {
						if _, err := fh.stream.Seek(-offset, io.SeekEnd); err != nil {
							return nil, inFile(i, err)
						}
						var header [6]byte
						if _, err := io.ReadFull(fh.stream, header[:]); err != nil {
							return nil, inFile(i, err)
						}
						format := header[0]
						id := header[1]
						size := int64(binary.LittleEndian.Uint32(header[2:6]))
						if size < 0 || offset+size > extraSize {
							return nil, inFile(i, mp4Error("invalid data", "invalid Insta360 record size %d", size))
						}

						pos, err := fh.stream.Seek(-offset-size, io.SeekEnd)
						if err != nil {
							return nil, inFile(i, err)
						}
						if id > 0 {
							result[i] = append(result[i], instaRecord{
								Offset:      uint64(pos),
								DataVersion: dataVersion,
								ID:          id,
								Format:      format,
								Size:        size,
							})
						}
						offset += size + 6
					}
				}
			}
		}

		if _, err := fh.stream.Seek(currentPos, io.SeekStart); err != nil {
			return nil, inFile(i, err)
		}

		// Match Rust's BTreeMap keyed by offset: sorted ascending, last write wins on duplicate offset.
		sort.SliceStable(result[i], func(a, b int) bool {
			return result[i][a].Offset < result[i][b].Offset
		})
		if len(result[i]) > 1 {
			dedup := result[i][:0]
			for _, rec := range result[i] {
				if len(dedup) > 0 && dedup[len(dedup)-1].Offset == rec.Offset {
					dedup[len(dedup)-1] = rec
				} else {
					dedup = append(dedup, rec)
				}
			}
			result[i] = dedup
		}
	}

	return result, nil
}

func mergeInsta360Metadata(files []*fileHandle, offsets [][]instaRecord, out io.WriteSeeker) error {
	if len(files) != len(offsets) {
		return errors.New("mp4merge: mismatched insta360 metadata inputs")
	}
	if len(files) == 0 {
		return nil
	}

	totalSize := int64(0)
	dataVersion := uint32(3)

	first := files[0].stream
	for _, record := range offsets[0] {
		dataVersion = record.DataVersion
		if _, err := first.Seek(int64(record.Offset), io.SeekStart); err != nil {
			return err
		}
		if err := copyExact(out, first, uint64(record.Size)); err != nil {
			return err
		}
		var trailer [6]byte
		if _, err := io.ReadFull(first, trailer[:]); err != nil {
			return err
		}
		format2 := trailer[0]
		id2 := trailer[1]
		size2 := int64(binary.LittleEndian.Uint32(trailer[2:6]))

		if record.ID != id2 || record.Format != format2 || record.Size != size2 {
			return mp4Error("invalid data", "invalid Insta360 metadata record header")
		}

		if id2 != 0 && id2 != 1 && id2 != 2 && id2 != 5 {
			for fileIdx := 1; fileIdx < len(files); fileIdx++ {
				for _, rec := range offsets[fileIdx] {
					if rec.ID != id2 {
						continue
					}
					stream := files[fileIdx].stream
					if _, err := stream.Seek(int64(rec.Offset), io.SeekStart); err != nil {
						return err
					}
					if err := copyExact(out, stream, uint64(rec.Size)); err != nil {
						return err
					}
					size2 += rec.Size
				}
			}
		}

		if _, err := out.Write([]byte{format2, id2}); err != nil {
			return err
		}
		if err := binary.Write(out, binary.LittleEndian, uint32(size2)); err != nil {
			return err
		}
		totalSize += size2 + 6
	}

	var zero [32]byte
	if _, err := out.Write(zero[:]); err != nil {
		return err
	}
	if err := binary.Write(out, binary.LittleEndian, uint32(totalSize+72)); err != nil {
		return err
	}
	if err := binary.Write(out, binary.LittleEndian, dataVersion); err != nil {
		return err
	}
	if _, err := out.Write(instaMagic); err != nil {
		return err
	}

	return nil
}
