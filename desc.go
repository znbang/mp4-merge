package mp4merge

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type trackDesc struct {
	tkhdDuration        uint64
	elstSegmentDuration uint64
	mdhdTimescale       uint32
	mdhdDuration        uint64
	stts                [][2]uint32
	stsz                []uint32
	stco                []uint64
	stss                []uint32
	sdtp                []byte
	sampleOffset        uint32
	chunkOffset         uint32
	stszSampleSize      uint32
	stszCount           uint32
	stsc                [][3]uint32
	co64FinalPosition   int64
	skip                bool
}

type mdatPosition struct {
	fileIndex int
	offset    uint64
	size      uint64
}

type desc struct {
	mdatPosition         []mdatPosition
	mvhdTimescalePerFile []uint32
	moovMvhdTimescale    uint32
	moovMvhdDuration     uint64
	moovTracks           []*trackDesc
	mdatOffset           uint64
	mdatFinalPosition    uint64
}

func newDesc() *desc {
	return &desc{
		mdatPosition:         make([]mdatPosition, 0),
		mvhdTimescalePerFile: make([]uint32, 0),
		moovTracks:           make([]*trackDesc, 0),
	}
}

func (d *desc) ensureTrack(index int) *trackDesc {
	for len(d.moovTracks) <= index {
		d.moovTracks = append(d.moovTracks, &trackDesc{co64FinalPosition: -1})
	}
	return d.moovTracks[index]
}

func (d *desc) ensureTimescaleIndex(index int) {
	for len(d.mvhdTimescalePerFile) <= index {
		d.mvhdTimescalePerFile = append(d.mvhdTimescalePerFile, 0)
	}
}

func readDesc(r io.ReadSeeker, d *desc, track int, maxRead uint64, fileIndex int) error {
	startOffs, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}

	d.ensureTimescaleIndex(fileIndex)

	tlTrack := track
	for {
		typ, _, size, headerSize, err := readBox(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if size == 0 || typ == 0 {
			continue
		}

		if hasChildren(typ, true) {
			payload := uint64(0)
			if size > uint64(headerSize) {
				payload = size - uint64(headerSize)
			}
			if err := readDesc(r, d, tlTrack, payload, fileIndex); err != nil {
				return err
			}
			if typ == fourCC("trak") {
				tlTrack++
			}
		} else {
			orgPos, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}

			switch typ {
			case fourCC("mvhd"), fourCC("tkhd"), fourCC("mdhd"):
				var version uint8
				if err := binary.Read(r, binary.BigEndian, &version); err != nil {
					return err
				}
				var flags [3]byte
				if _, err := io.ReadFull(r, flags[:]); err != nil {
					return err
				}
				_ = flags

				switch typ {
				case fourCC("mvhd"):
					var timescale uint32
					var duration uint64
					if version == 1 {
						if _, err := r.Seek(16, io.SeekCurrent); err != nil {
							return err
						}
						if err := binary.Read(r, binary.BigEndian, &timescale); err != nil {
							return err
						}
						if err := binary.Read(r, binary.BigEndian, &duration); err != nil {
							return err
						}
					} else {
						if _, err := r.Seek(8, io.SeekCurrent); err != nil {
							return err
						}
						if err := binary.Read(r, binary.BigEndian, &timescale); err != nil {
							return err
						}
						var dur32 uint32
						if err := binary.Read(r, binary.BigEndian, &dur32); err != nil {
							return err
						}
						duration = uint64(dur32)
					}
					if d.moovMvhdTimescale == 0 {
						d.moovMvhdTimescale = timescale
					}
					d.mvhdTimescalePerFile[fileIndex] = timescale
					if timescale != 0 && d.moovMvhdTimescale != 0 {
						delta := scaleDuration(duration, timescale, d.moovMvhdTimescale)
						d.moovMvhdDuration += delta
					}
				case fourCC("tkhd"):
					td := d.ensureTrack(tlTrack)
					var duration uint64
					if version == 1 {
						if _, err := r.Seek(20, io.SeekCurrent); err != nil {
							return err
						}
						if err := binary.Read(r, binary.BigEndian, &duration); err != nil {
							return err
						}
					} else {
						if _, err := r.Seek(16, io.SeekCurrent); err != nil {
							return err
						}
						var dur32 uint32
						if err := binary.Read(r, binary.BigEndian, &dur32); err != nil {
							return err
						}
						duration = uint64(dur32)
					}
					timescale := d.mvhdTimescalePerFile[fileIndex]
					if timescale != 0 && d.moovMvhdTimescale != 0 {
						td.tkhdDuration += scaleDuration(duration, timescale, d.moovMvhdTimescale)
					}
				case fourCC("mdhd"):
					td := d.ensureTrack(tlTrack)
					var timescale uint32
					var duration uint64
					if version == 1 {
						if _, err := r.Seek(16, io.SeekCurrent); err != nil {
							return err
						}
						if err := binary.Read(r, binary.BigEndian, &timescale); err != nil {
							return err
						}
						if err := binary.Read(r, binary.BigEndian, &duration); err != nil {
							return err
						}
					} else {
						if _, err := r.Seek(8, io.SeekCurrent); err != nil {
							return err
						}
						if err := binary.Read(r, binary.BigEndian, &timescale); err != nil {
							return err
						}
						var dur32 uint32
						if err := binary.Read(r, binary.BigEndian, &dur32); err != nil {
							return err
						}
						duration = uint64(dur32)
					}
					if td.mdhdTimescale == 0 {
						td.mdhdTimescale = timescale
					}
					if timescale != 0 && td.mdhdTimescale != 0 {
						td.mdhdDuration += scaleDuration(duration, timescale, td.mdhdTimescale)
					}
				}
			case fourCC("elst"), fourCC("stts"), fourCC("stsz"), fourCC("stss"),
				fourCC("stco"), fourCC("co64"), fourCC("sdtp"), fourCC("stsc"):
				td := d.ensureTrack(tlTrack)
				if !(td.skip && fileIndex > 0) {
					var version uint8
					if err := binary.Read(r, binary.BigEndian, &version); err != nil {
						return err
					}
					var flags [3]byte
					if _, err := io.ReadFull(r, flags[:]); err != nil {
						return err
					}
					_ = flags

					switch typ {
					case fourCC("elst"):
						var entryCount uint32
						if err := binary.Read(r, binary.BigEndian, &entryCount); err != nil {
							return err
						}
						for i := uint32(0); i < entryCount; i++ {
							var segmentDuration uint64
							var mediaTime int64
							if version == 1 {
								if err := binary.Read(r, binary.BigEndian, &segmentDuration); err != nil {
									return err
								}
								if err := binary.Read(r, binary.BigEndian, &mediaTime); err != nil {
									return err
								}
							} else {
								var dur32 uint32
								if err := binary.Read(r, binary.BigEndian, &dur32); err != nil {
									return err
								}
								segmentDuration = uint64(dur32)

								var media32 int32
								if err := binary.Read(r, binary.BigEndian, &media32); err != nil {
									return err
								}
								mediaTime = int64(media32)
							}
							if _, err := r.Seek(4, io.SeekCurrent); err != nil { // media rate
								return err
							}
							if mediaTime != -1 {
								td.elstSegmentDuration += segmentDuration
							}
						}
					case fourCC("stsz"):
						if err := binary.Read(r, binary.BigEndian, &td.stszSampleSize); err != nil {
							return err
						}
						var count uint32
						if err := binary.Read(r, binary.BigEndian, &count); err != nil {
							return err
						}
						if td.stszSampleSize == 0 {
							for i := uint32(0); i < count; i++ {
								var size uint32
								if err := binary.Read(r, binary.BigEndian, &size); err != nil {
									return err
								}
								td.stsz = append(td.stsz, size)
							}
						}
						td.stszCount += count
					case fourCC("sdtp"):
						payload := int(size) - int(headerSize) - 4
						if payload > 0 {
							buf := make([]byte, payload)
							if _, err := io.ReadFull(r, buf); err != nil {
								return err
							}
							td.sdtp = append(td.sdtp, buf...)
						}
					default:
						var count uint32
						if err := binary.Read(r, binary.BigEndian, &count); err != nil {
							return err
						}
						currentMdat := d.mdatPosition[len(d.mdatPosition)-1].offset
						mdatOffset := int64(d.mdatOffset) - int64(currentMdat)
						for i := uint32(0); i < count; i++ {
							switch typ {
							case fourCC("stss"):
								var sample uint32
								if err := binary.Read(r, binary.BigEndian, &sample); err != nil {
									return err
								}
								td.stss = append(td.stss, sample+td.sampleOffset)
							case fourCC("stco"):
								var value uint32
								if err := binary.Read(r, binary.BigEndian, &value); err != nil {
									return err
								}
								td.stco = append(td.stco, uint64(int64(value)+mdatOffset))
							case fourCC("co64"):
								var value uint64
								if err := binary.Read(r, binary.BigEndian, &value); err != nil {
									return err
								}
								td.stco = append(td.stco, uint64(int64(value)+mdatOffset))
							case fourCC("stts"):
								var countVal uint32
								var delta uint32
								if err := binary.Read(r, binary.BigEndian, &countVal); err != nil {
									return err
								}
								if err := binary.Read(r, binary.BigEndian, &delta); err != nil {
									return err
								}
								td.stts = append(td.stts, [2]uint32{countVal, delta})
							case fourCC("stsc"):
								var firstChunk uint32
								var samplesPerChunk uint32
								var sampleDescriptionIndex uint32
								if err := binary.Read(r, binary.BigEndian, &firstChunk); err != nil {
									return err
								}
								if err := binary.Read(r, binary.BigEndian, &samplesPerChunk); err != nil {
									return err
								}
								if err := binary.Read(r, binary.BigEndian, &sampleDescriptionIndex); err != nil {
									return err
								}
								td.stsc = append(td.stsc, [3]uint32{
									firstChunk + td.chunkOffset,
									samplesPerChunk,
									sampleDescriptionIndex,
								})
							default:
								return fmt.Errorf("unsupported box %s", typeToString(typ))
							}
						}
					}
				}
			case fourCC("tmcd"):
				td := d.ensureTrack(tlTrack)
				td.skip = true
			default:
				// ignore
			}

			target := orgPos + int64(size) - headerSize
			if target < 0 {
				target = orgPos
			}
			if _, err := r.Seek(target, io.SeekStart); err != nil {
				return err
			}
		}

		curPos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if uint64(curPos-startOffs) >= maxRead {
			break
		}
	}
	return nil
}

func scaleDuration(duration uint64, fromScale, toScale uint32) uint64 {
	if fromScale == 0 || toScale == 0 {
		return duration
	}
	value := (float64(duration) / float64(fromScale)) * float64(toScale)
	return uint64(math.Ceil(value))
}
