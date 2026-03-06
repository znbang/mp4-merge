package mp4merge

import (
	"encoding/binary"
	"errors"
	"io"
)

func rewriteFromDesc(files []*fileHandle, output io.WriteSeeker, d *desc, track int, maxRead uint64) (uint64, error) {
	var totalRead, totalNew uint64
	tlTrack := track
	first := files[0].stream

	for {
		typ, _, size, headerSize, err := readBox(first)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
		if size == 0 || typ == 0 {
			break
		}

		totalRead += size
		newSize := size

		switch {
		case hasChildren(typ, false):
			if _, err := first.Seek(-headerSize, io.SeekCurrent); err != nil {
				return 0, err
			}
			outPos, err := output.Seek(0, io.SeekCurrent)
			if err != nil {
				return 0, err
			}
			if _, err := io.CopyN(output, first, int64(headerSize)); err != nil {
				return 0, err
			}
			payload := uint64(0)
			if size > uint64(headerSize) {
				payload = size - uint64(headerSize)
			}
			childSize, err := rewriteFromDesc(files, output, d, tlTrack, payload)
			if err != nil {
				return 0, err
			}
			newSize = uint64(headerSize) + childSize
			if typ == fourCC("trak") {
				tlTrack++
			}
			if newSize != size {
				if err := patchUint32(output, outPos, uint32(newSize)); err != nil {
					return 0, err
				}
			}

		case typ == fourCC("mdat"):
			if err := binary.Write(output, binary.BigEndian, uint32(1)); err != nil {
				return 0, err
			}
			if err := binary.Write(output, binary.BigEndian, typ); err != nil {
				return 0, err
			}
			sizePos, err := output.Seek(0, io.SeekCurrent)
			if err != nil {
				return 0, err
			}
			if err := binary.Write(output, binary.BigEndian, uint64(0)); err != nil {
				return 0, err
			}
			newSize = 16
			dataPos, err := output.Seek(0, io.SeekCurrent)
			if err != nil {
				return 0, err
			}
			d.mdatFinalPosition = uint64(dataPos)

			for _, m := range d.mdatPosition {
				if m.fileIndex < 0 {
					continue
				}
				stream := files[m.fileIndex].stream
				prev, err := stream.Seek(0, io.SeekCurrent)
				if err != nil {
					return 0, err
				}
				if _, err := stream.Seek(int64(m.offset), io.SeekStart); err != nil {
					return 0, err
				}
				if _, err := io.CopyN(output, stream, int64(m.size)); err != nil && !errors.Is(err, io.EOF) {
					return 0, err
				}
				if _, err := stream.Seek(prev, io.SeekStart); err != nil {
					return 0, err
				}
				newSize += m.size
			}

			if err := patchUint64(output, sizePos, newSize); err != nil {
				return 0, err
			}

			skip := int64(size) - headerSize
			if skip < 0 {
				skip = 0
			}
			if _, err := first.Seek(skip, io.SeekCurrent); err != nil {
				return 0, err
			}

		case typ == fourCC("mvhd") || typ == fourCC("tkhd") || typ == fourCC("mdhd") || typ == fourCC("elst"):
			version, err := readUint8(first)
			if err != nil {
				return 0, err
			}
			if _, err := first.Seek(3, io.SeekCurrent); err != nil {
				return 0, err
			}
			if _, err := first.Seek(-headerSize-4, io.SeekCurrent); err != nil {
				return 0, err
			}
			outPos, err := output.Seek(0, io.SeekCurrent)
			if err != nil {
				return 0, err
			}
			if _, err := io.CopyN(output, first, int64(size)); err != nil {
				return 0, err
			}
			patchStart := outPos + headerSize + 4

			switch typ {
			case fourCC("mvhd"):
				if version == 1 {
					if err := patchUint64(output, patchStart+8+8+4, d.moovMvhdDuration); err != nil {
						return 0, err
					}
				} else {
					if err := patchUint32(output, patchStart+4+4+4, uint32(d.moovMvhdDuration)); err != nil {
						return 0, err
					}
				}
			default:
				if tlTrack < len(d.moovTracks) {
					td := d.moovTracks[tlTrack]
					if td != nil {
						switch typ {
						case fourCC("tkhd"):
							if version == 1 {
								if err := patchUint64(output, patchStart+8+8+8+4, td.tkhdDuration); err != nil {
									return 0, err
								}
							} else {
								if err := patchUint32(output, patchStart+4+4+4+4, uint32(td.tkhdDuration)); err != nil {
									return 0, err
								}
							}
						case fourCC("mdhd"):
							if version == 1 {
								if err := patchUint64(output, patchStart+8+8+4, td.mdhdDuration); err != nil {
									return 0, err
								}
							} else {
								if err := patchUint32(output, patchStart+4+4+4, uint32(td.mdhdDuration)); err != nil {
									return 0, err
								}
							}
						case fourCC("elst"):
							elstDuration := td.elstSegmentDuration
							if elstDuration != 0 && td.mdhdDuration > elstDuration {
								elstDuration = td.mdhdDuration
							}
							if version == 1 {
								if err := patchUint64(output, patchStart+4, elstDuration); err != nil {
									return 0, err
								}
							} else {
								if err := patchUint32(output, patchStart+4, uint32(elstDuration)); err != nil {
									return 0, err
								}
							}
						}
					}
				}
			}

		case typ == fourCC("stts") || typ == fourCC("stsz") || typ == fourCC("stss") ||
			typ == fourCC("stco") || typ == fourCC("co64") || typ == fourCC("sdtp") || typ == fourCC("stsc"):
			skip := int64(size) - headerSize
			if skip < 0 {
				skip = 0
			}
			if _, err := first.Seek(skip, io.SeekCurrent); err != nil {
				return 0, err
			}

			outPos, err := output.Seek(0, io.SeekCurrent)
			if err != nil {
				return 0, err
			}
			if err := binary.Write(output, binary.BigEndian, uint32(0)); err != nil {
				return 0, err
			}
			newTyp := typ
			if typ == fourCC("stco") {
				newTyp = fourCC("co64")
			}
			if err := binary.Write(output, binary.BigEndian, newTyp); err != nil {
				return 0, err
			}
			if err := binary.Write(output, binary.BigEndian, uint32(0)); err != nil {
				return 0, err
			}
			newSize = 12

			var td *trackDesc
			if tlTrack < len(d.moovTracks) {
				td = d.moovTracks[tlTrack]
			}
			if td == nil {
				td = &trackDesc{}
			}

			switch typ {
			case fourCC("stts"):
				merged := make([][2]uint32, 0, len(td.stts))
				var prev *[2]uint32
				for _, pair := range td.stts {
					if prev != nil && (*prev)[1] == pair[1] {
						(*prev)[0] += pair[0]
						continue
					}
					merged = append(merged, pair)
					prev = &merged[len(merged)-1]
				}
				if err := binary.Write(output, binary.BigEndian, uint32(len(merged))); err != nil {
					return 0, err
				}
				newSize += 4
				for _, entry := range merged {
					if err := binary.Write(output, binary.BigEndian, entry[0]); err != nil {
						return 0, err
					}
					if err := binary.Write(output, binary.BigEndian, entry[1]); err != nil {
						return 0, err
					}
					newSize += 8
				}

			case fourCC("stsz"):
				if err := binary.Write(output, binary.BigEndian, td.stszSampleSize); err != nil {
					return 0, err
				}
				if err := binary.Write(output, binary.BigEndian, td.stszCount); err != nil {
					return 0, err
				}
				newSize += 8
				if td.stszSampleSize == 0 {
					for _, sz := range td.stsz {
						if err := binary.Write(output, binary.BigEndian, sz); err != nil {
							return 0, err
						}
						newSize += 4
					}
				}

			case fourCC("stss"):
				if err := binary.Write(output, binary.BigEndian, uint32(len(td.stss))); err != nil {
					return 0, err
				}
				newSize += 4
				for _, sample := range td.stss {
					if err := binary.Write(output, binary.BigEndian, sample); err != nil {
						return 0, err
					}
					newSize += 4
				}

			case fourCC("stco"), fourCC("co64"):
				if err := binary.Write(output, binary.BigEndian, uint32(len(td.stco))); err != nil {
					return 0, err
				}
				newSize += 4
				pos, err := output.Seek(0, io.SeekCurrent)
				if err != nil {
					return 0, err
				}
				td.co64FinalPosition = pos
				for _, offs := range td.stco {
					if err := binary.Write(output, binary.BigEndian, offs+d.mdatFinalPosition); err != nil {
						return 0, err
					}
					newSize += 8
				}

			case fourCC("sdtp"):
				if len(td.sdtp) > 0 {
					if _, err := output.Write(td.sdtp); err != nil {
						return 0, err
					}
					newSize += uint64(len(td.sdtp))
				}

			case fourCC("stsc"):
				if err := binary.Write(output, binary.BigEndian, uint32(len(td.stsc))); err != nil {
					return 0, err
				}
				newSize += 4
				for _, entry := range td.stsc {
					if err := binary.Write(output, binary.BigEndian, entry[0]); err != nil {
						return 0, err
					}
					if err := binary.Write(output, binary.BigEndian, entry[1]); err != nil {
						return 0, err
					}
					if err := binary.Write(output, binary.BigEndian, entry[2]); err != nil {
						return 0, err
					}
					newSize += 12
				}
			}

			if err := patchUint32(output, outPos, uint32(newSize)); err != nil {
				return 0, err
			}

		default:
			if _, err := first.Seek(-headerSize, io.SeekCurrent); err != nil {
				return 0, err
			}
			if _, err := io.CopyN(output, first, int64(size)); err != nil {
				return 0, err
			}
		}

		totalNew += newSize
		if totalRead >= maxRead {
			break
		}
	}

	return totalNew, nil
}

func patchUint32(w io.WriteSeeker, position int64, value uint32) error {
	current, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := w.Seek(position, io.SeekStart); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, value); err != nil {
		return err
	}
	_, err = w.Seek(current, io.SeekStart)
	return err
}

func patchUint64(w io.WriteSeeker, position int64, value uint64) error {
	current, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := w.Seek(position, io.SeekStart); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, value); err != nil {
		return err
	}
	_, err = w.Seek(current, io.SeekStart)
	return err
}

func readUint8(r io.ReadSeeker) (uint8, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}
