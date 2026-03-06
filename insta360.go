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
		if fh.size <= instaHeaderSize {
			continue
		}

		currentPos, err := fh.stream.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}

		if _, err := fh.stream.Seek(-int64(instaHeaderSize), io.SeekEnd); err != nil {
			_, _ = fh.stream.Seek(currentPos, io.SeekStart)
			continue
		}

		buf := make([]byte, instaHeaderSize)
		if _, err := io.ReadFull(fh.stream, buf); err != nil {
			_, _ = fh.stream.Seek(currentPos, io.SeekStart)
			continue
		}
		if !bytes.Equal(buf[instaHeaderSize-32:], instaMagic) {
			_, _ = fh.stream.Seek(currentPos, io.SeekStart)
			continue
		}

		extraSize := int64(binary.LittleEndian.Uint32(buf[32:36]))
		if extraSize <= 0 || extraSize > fh.size {
			_, _ = fh.stream.Seek(currentPos, io.SeekStart)
			continue
		}
		dataVersion := binary.LittleEndian.Uint32(buf[36:40])
		extraStart := fh.size - extraSize

		offset := int64(instaHeaderSize + 6)
		if _, err := fh.stream.Seek(-offset+1, io.SeekEnd); err != nil {
			_, _ = fh.stream.Seek(currentPos, io.SeekStart)
			continue
		}

		var firstID [1]byte
		if _, err := io.ReadFull(fh.stream, firstID[:]); err != nil {
			_, _ = fh.stream.Seek(currentPos, io.SeekStart)
			continue
		}

		if firstID[0] == 0 {
			var sizeBuf [4]byte
			if _, err := io.ReadFull(fh.stream, sizeBuf[:]); err != nil {
				_, _ = fh.stream.Seek(currentPos, io.SeekStart)
				continue
			}
			recordSize := int64(binary.LittleEndian.Uint32(sizeBuf[:]))
			if recordSize <= 0 {
				_, _ = fh.stream.Seek(currentPos, io.SeekStart)
				continue
			}
			dataBuf := make([]byte, recordSize)
			if _, err := fh.stream.Seek(-offset-recordSize, io.SeekEnd); err != nil {
				_, _ = fh.stream.Seek(currentPos, io.SeekStart)
				continue
			}
			if _, err := io.ReadFull(fh.stream, dataBuf); err != nil {
				_, _ = fh.stream.Seek(currentPos, io.SeekStart)
				continue
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
				if id > 0 && size > 0 {
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
					break
				}
				header := make([]byte, 6)
				if _, err := io.ReadFull(fh.stream, header); err != nil {
					break
				}
				format := header[0]
				id := header[1]
				size := int64(binary.LittleEndian.Uint32(header[2:6]))
				if size <= 0 {
					break
				}

				if _, err := fh.stream.Seek(-offset-size, io.SeekEnd); err != nil {
					break
				}
				if id > 0 {
					pos, err := fh.stream.Seek(0, io.SeekCurrent)
					if err != nil {
						break
					}
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

		if _, err := fh.stream.Seek(currentPos, io.SeekStart); err != nil {
			return nil, err
		}

		sort.Slice(result[i], func(a, b int) bool {
			return result[i][a].Offset < result[i][b].Offset
		})
	}

	return result, nil
}

func mergeInsta360Metadata(files []*fileHandle, offsets [][]instaRecord, out io.WriteSeeker) error {
	if len(files) != len(offsets) {
		return errors.New("mp4merge: mismatched insta360 metadata inputs")
	}
	if len(files) == 0 || len(offsets) == 0 || len(offsets[0]) == 0 {
		return nil
	}

	totalSize := int64(0)
	dataVersion := uint32(3)

	first := files[0].stream
	for _, record := range offsets[0] {
		dataVersion = record.DataVersion
		if record.Size <= 0 {
			continue
		}
		if _, err := first.Seek(int64(record.Offset), io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(out, first, record.Size); err != nil {
			return err
		}
		var trailer [6]byte
		if _, err := io.ReadFull(first, trailer[:]); err != nil {
			return err
		}
		format := trailer[0]
		id := trailer[1]
		size2 := int64(binary.LittleEndian.Uint32(trailer[2:6]))

		if id != 0 && id != 1 && id != 2 && id != 5 {
			for fileIdx := 1; fileIdx < len(files); fileIdx++ {
				for _, rec := range offsets[fileIdx] {
					if rec.ID != id {
						continue
					}
					stream := files[fileIdx].stream
					if _, err := stream.Seek(int64(rec.Offset), io.SeekStart); err != nil {
						return err
					}
					if _, err := io.CopyN(out, stream, rec.Size); err != nil {
						return err
					}
					size2 += rec.Size
				}
			}
		}

		if _, err := out.Write([]byte{format, id}); err != nil {
			return err
		}
		if err := binary.Write(out, binary.LittleEndian, uint32(size2)); err != nil {
			return err
		}
		totalSize += size2 + 6
	}

	zero := make([]byte, 16)
	if _, err := out.Write(zero); err != nil {
		return err
	}
	if _, err := out.Write(zero); err != nil {
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
