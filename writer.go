package mp4merge

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// rewrite writes the first file's boxes, substituting the merged movie
// description and replacing the selected mdat with all media payloads.
func rewrite(files []Stream, out io.WriteSeeker, d *desc) error {
	if len(files) == 0 || len(d.parts) == 0 {
		return mp4Error("invalid data", "no parsed files to write")
	}
	if _, err := files[0].Reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := rewriteBoxes(files, out, d, 0, d.parts[0].walkEnd, 0, 0); err != nil {
		return err
	}
	if d.finalMdatPosition == nil {
		return mp4Error("invalid data", "file 1 has no mdat box to write")
	}
	mdatPos := *d.finalMdatPosition
	for i := range d.tracks {
		t := &d.tracks[i]
		if len(t.stco) == 0 {
			continue
		}
		if t.co64Position == nil {
			return mp4Error("invalid data", "file 1 track %d has no chunk offset table to write merged chunks in", i+1)
		}
		if _, err := out.Seek(int64(*t.co64Position), io.SeekStart); err != nil {
			return err
		}
		for _, off := range t.stco {
			if math.MaxUint64-off < mdatPos {
				return mp4Error("invalid data", "track %d chunk offset overflows", i+1)
			}
			if err := binary.Write(out, binary.BigEndian, off+mdatPos); err != nil {
				return err
			}
		}
	}
	return nil
}

func rewriteBoxes(files []Stream, out io.WriteSeeker, d *desc, track int, maxRead, parentEnd uint64, parent uint32) (uint64, error) {
	if track < 0 {
		track = 0
	}
	first := files[0].Reader
	start, err := first.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if maxRead > math.MaxUint64-uint64(start) {
		return 0, mp4Error("invalid data", "box list end overflows")
	}
	end := uint64(start) + maxRead
	if parentEnd != 0 && end > parentEnd {
		end = parentEnd
	}
	var total, written uint64
	sampleGroup, compactGroup := 0, 0
	for uint64(start)+total < end {
		pos := uint64(start) + total
		remaining := end - pos
		if remaining < 8 {
			if parent != 0 && remaining != 0 {
				return written, mp4Error("invalid data", "%s ends with %d bytes that are not a box", typeToString(parent), remaining)
			}
			if _, err := first.Seek(int64(end), io.SeekStart); err != nil {
				return written, err
			}
			break
		}
		h, err := readBoxWithin(first, d.parts[0].end, end)
		if err != nil {
			return written, inFile(0, err)
		}
		size := h.end - h.offset
		total += size
		newSize := size
		typ := h.typ
		switch {
		case hasChildren(typ):
			if _, err := first.Seek(int64(h.offset), io.SeekStart); err != nil {
				return written, err
			}
			outPos, err := out.Seek(0, io.SeekCurrent)
			if err != nil {
				return written, err
			}
			if err := copyExact(out, first, h.headerSize); err != nil {
				return written, inFile(0, err)
			}
			childSize, err := rewriteBoxes(files, out, d, track, size-h.headerSize, h.end, typ)
			if err != nil {
				return written, err
			}
			newSize = h.headerSize + childSize
			if typ == fourCC("trak") {
				track++
			}
			if newSize != size {
				if err := patchBoxSize(out, uint64(outPos), h.headerSize, newSize); err != nil {
					return written, err
				}
			}
		case typ == fourCC("udta") && parent == fourCC("moov"):
			body, err := readBody(first, size-h.headerSize, typ, h.offset)
			if err != nil {
				return written, err
			}
			kept, found := withoutSegmentTable(body)
			if found {
				if len(kept) == 0 {
					newSize = 0
				} else {
					newSize, err = writeBox(out, typ, h.headerSize, kept)
					if err != nil {
						return written, err
					}
				}
			} else {
				if _, err := first.Seek(int64(h.offset), io.SeekStart); err != nil {
					return written, err
				}
				if err := copyExact(out, first, size); err != nil {
					return written, err
				}
			}
		case typ == fourCC("mdat") && parent == 0:
			part := &d.parts[0]
			if len(d.mdatPositions) > 0 && h.offset+h.headerSize == d.mdatPositions[0].offset {
				// The selected mdat need not be the first one. Match its original data offset.
				selected := false
				for _, m := range part.mdats {
					if h.offset+h.headerSize == m[0] && m[1] == d.mdatPositions[0].size {
						selected = true
						break
					}
				}
				if selected {
					outPos, err := out.Seek(0, io.SeekCurrent)
					if err != nil {
						return written, err
					}
					if err := writeU32(out, 1); err != nil {
						return written, err
					}
					if err := writeU32(out, typ); err != nil {
						return written, err
					}
					sizePos, err := out.Seek(0, io.SeekCurrent)
					if err != nil {
						return written, err
					}
					if err := writeU64(out, 0); err != nil {
						return written, err
					}
					dataPos, err := out.Seek(0, io.SeekCurrent)
					if err != nil {
						return written, err
					}
					d.finalMdatPosition = ptrU64(uint64(dataPos))
					newSize = 16
					for i, m := range d.mdatPositions {
						if m.fileIndex < 0 {
							continue
						}
						if m.fileIndex >= len(files) {
							return written, mp4Error("invalid data", "mdat references missing file %d", m.fileIndex+1)
						}
						r := files[m.fileIndex].Reader
						prev, err := r.Seek(0, io.SeekCurrent)
						if err != nil {
							return written, err
						}
						if _, err := r.Seek(int64(m.offset), io.SeekStart); err != nil {
							return written, err
						}
						if err := copyExact(out, r, m.size); err != nil {
							return written, inFile(i, err)
						}
						if _, err := r.Seek(prev, io.SeekStart); err != nil {
							return written, err
						}
						newSize, err = add(newSize, m.size)
						if err != nil {
							return written, err
						}
					}
					for _, f := range d.mdatFiller {
						if err := writeRepeated(out, f.value, f.length); err != nil {
							return written, err
						}
						newSize, err = add(newSize, f.length)
						if err != nil {
							return written, err
						}
					}
					if err := patchU64(out, uint64(sizePos), newSize); err != nil {
						return written, err
					}
					_ = outPos
				} else {
					if _, err := first.Seek(int64(h.end), io.SeekStart); err != nil {
						return written, err
					}
					newSize = 0
				}
			} else {
				if _, err := first.Seek(int64(h.end), io.SeekStart); err != nil {
					return written, err
				}
				newSize = 0
			}
		case typ == fourCC("mvex") && parent == fourCC("moov"):
			if _, err := first.Seek(int64(h.end), io.SeekStart); err != nil {
				return written, err
			}
			newSize = 0
		case typ == fourCC("mvhd") || typ == fourCC("tkhd") || typ == fourCC("mdhd"):
			body, err := readBody(first, size-h.headerSize, typ, h.offset)
			if err != nil {
				return written, err
			}
			if typ == fourCC("mvhd") && parent == fourCC("moov") {
				err = setDuration(&body, typ, d.movieDuration)
			}
			if track < len(d.tracks) {
				t := &d.tracks[track]
				switch typ {
				case fourCC("tkhd"):
					err = setDuration(&body, typ, t.tkhdDuration)
				case fourCC("mdhd"):
					at := 12
					if len(body) > 0 && body[0] == 1 {
						at = 20
					}
					if len(body) < at+4 {
						err = mp4Error("invalid data", "mdhd box is too short")
					} else {
						binary.BigEndian.PutUint32(body[at:at+4], t.mdhdTimescale)
						err = setDuration(&body, typ, t.mdhdDuration)
					}
				}
			}
			if err != nil {
				return written, err
			}
			newSize, err = writeBox(out, typ, h.headerSize, body)
			if err != nil {
				return written, err
			}
			if typ == fourCC("tkhd") && track < len(d.tracks) && d.tracks[track].insertEdts && !d.parts[0].tracks[track].hasEdts {
				body, err := elstBody(d.tracks[track].elst)
				if err != nil {
					return written, err
				}
				elst, _ := writeBoxBytes(fourCC("elst"), body)
				edts, _ := writeBoxBytes(fourCC("edts"), elst)
				if _, err := out.Write(edts); err != nil {
					return written, err
				}
				newSize += uint64(len(edts))
			}
		case typ == fourCC("elst") && (parent == fourCC("edts") || parent == fourCC("trak")) && track < len(d.tracks) && (d.parts[0].tracks[track].hasElst || d.tracks[track].insertEdts):
			if _, err := first.Seek(int64(h.end), io.SeekStart); err != nil {
				return written, err
			}
			body, err := elstBody(d.tracks[track].elst)
			if err != nil {
				return written, err
			}
			newSize, err = writeBox(out, typ, h.headerSize, body)
			if err != nil {
				return written, err
			}
		case typ == fourCC("stsd") && parent == fourCC("stbl") && track < len(d.tracks) && d.tracks[track].hasSTSD:
			if _, err := first.Seek(int64(h.end), io.SeekStart); err != nil {
				return written, err
			}
			newSize, err = writeBox(out, typ, h.headerSize, d.tracks[track].stsd)
			if err != nil {
				return written, err
			}
		case parent == fourCC("stbl") && track < len(d.tracks) && rewriteTableType(typ):
			var original []byte
			if typ == fourCC("cslg") {
				original, err = readBody(first, size-h.headerSize, typ, h.offset)
			} else {
				_, err = first.Seek(int64(h.end), io.SeekStart)
			}
			if err != nil {
				return written, err
			}
			newSize, err = writeTable(out, typ, &d.tracks[track], original, sampleGroup)
			if err != nil {
				return written, err
			}
			if typ == fourCC("sbgp") {
				sampleGroup++
			}
		case typ == fourCC("csgm") && parent == fourCC("stbl") && track < len(d.tracks) && compactGroup < len(d.tracks[track].compactGroups):
			body, err := readBody(first, size-h.headerSize, typ, h.offset)
			if err != nil {
				return written, err
			}
			if _, ok := compactToGroupOf(body); ok {
				box, ok := compactGroupBox(d.tracks[track].compactGroups[compactGroup])
				if !ok {
					return written, mp4Error("unsupported input", "merged csgm doesn't fit its fields")
				}
				compactGroup++
				if _, err := out.Write(box); err != nil {
					return written, err
				}
				newSize = uint64(len(box))
			} else {
				newSize, err = writeBox(out, typ, h.headerSize, body)
				if err != nil {
					return written, err
				}
			}
		default:
			if _, err := first.Seek(int64(h.offset), io.SeekStart); err != nil {
				return written, err
			}
			if err := copyExact(out, first, size); err != nil {
				return written, inFile(0, err)
			}
		}
		if math.MaxUint64-written < newSize {
			return written, mp4Error("invalid data", "written box sizes overflow")
		}
		written += newSize
		if _, err := first.Seek(int64(h.end), io.SeekStart); err != nil {
			return written, err
		}
	}
	if parent == fourCC("edts") && track < len(d.tracks) && d.tracks[track].insertEdts {
		body, err := elstBody(d.tracks[track].elst)
		if err != nil {
			return written, err
		}
		size, err := writeBox(out, fourCC("elst"), 8, body)
		if err != nil {
			return written, err
		}
		written += size
	}
	if parent == fourCC("stbl") && track < len(d.tracks) {
		t := &d.tracks[track]
		for _, item := range []struct {
			typ uint32
			has bool
		}{{fourCC("ctts"), t.hasCTTS}, {fourCC("stss"), t.hasSTSS}, {fourCC("stps"), t.hasSTPS}, {fourCC("sdtp"), t.hasSDTP}} {
			if item.has && !t.written[item.typ] {
				size, err := writeTable(out, item.typ, t, nil, 0)
				if err != nil {
					return written, err
				}
				written += size
			}
		}
	}
	return written, nil
}

func rewriteTableType(t uint32) bool {
	switch t {
	case fourCC("stts"), fourCC("ctts"), fourCC("stsz"), fourCC("stz2"), fourCC("stss"), fourCC("stps"), fourCC("stco"), fourCC("co64"), fourCC("sdtp"), fourCC("stsc"), fourCC("sbgp"), fourCC("cslg"):
		return true
	}
	return false
}

func writeTable(out io.WriteSeeker, typ uint32, t *mergedTrack, original []byte, groupIndex int) (uint64, error) {
	body := []byte{0, 0, 0, 0}
	newType := typ
	if typ == fourCC("stco") {
		newType = fourCC("co64")
	}
	if typ == fourCC("stz2") {
		newType = fourCC("stsz")
	}
	var group *sampleToGroup
	if typ == fourCC("sbgp") && groupIndex < len(t.sampleGroups) {
		group = &t.sampleGroups[groupIndex]
		body[0] = group.version
	}
	if typ == fourCC("ctts") {
		body[0] = t.cttsVersion
	}
	if typ == fourCC("cslg") && t.cslg != nil {
		version := uint8(0)
		for _, n := range t.cslg {
			if n < math.MinInt32 || n > math.MaxInt32 {
				version = 1
			}
		}
		if len(original) > 0 && original[0] == 1 {
			version = 1
		}
		body[0] = version
		if len(original) >= 4 {
			copy(body[1:4], original[1:4])
		}
	} else if typ == fourCC("cslg") {
		return writeBox(out, typ, 8, original)
	}
	switch typ {
	case fourCC("stts"):
		entries := make([][2]uint32, 0, len(t.stts))
		for _, e := range t.stts {
			entries = appendRun(entries, e)
		}
		body = binary.BigEndian.AppendUint32(body, uint32(len(entries)))
		for _, e := range entries {
			body = binary.BigEndian.AppendUint32(body, e[0])
			body = binary.BigEndian.AppendUint32(body, e[1])
		}
	case fourCC("ctts"):
		body = binary.BigEndian.AppendUint32(body, uint32(len(t.ctts)))
		for _, e := range t.ctts {
			body = binary.BigEndian.AppendUint32(body, e.sampleCount)
			body = binary.BigEndian.AppendUint32(body, uint32(e.sampleOffset))
		}
	case fourCC("cslg"):
		for _, v := range t.cslg {
			if body[0] == 1 {
				body = binary.BigEndian.AppendUint64(body, uint64(v))
			} else {
				body = binary.BigEndian.AppendUint32(body, uint32(int32(v)))
			}
		}
	case fourCC("stsz"), fourCC("stz2"):
		body = binary.BigEndian.AppendUint32(body, t.stszSampleSize)
		body = binary.BigEndian.AppendUint32(body, t.stszCount)
		for _, v := range t.stsz {
			body = binary.BigEndian.AppendUint32(body, v)
		}
	case fourCC("stss"), fourCC("stps"):
		list := t.stss
		if typ == fourCC("stps") {
			list = t.stps
		}
		body = binary.BigEndian.AppendUint32(body, uint32(len(list)))
		for _, v := range list {
			body = binary.BigEndian.AppendUint32(body, v)
		}
	case fourCC("stco"), fourCC("co64"):
		body = binary.BigEndian.AppendUint32(body, uint32(len(t.stco)))
		pos, err := out.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		t.co64Position = ptrU64(uint64(pos) + 8 + uint64(len(body)))
		for _, v := range t.stco {
			body = binary.BigEndian.AppendUint64(body, v)
		}
	case fourCC("sdtp"):
		body = append(body, t.sdtp...)
	case fourCC("stsc"):
		body = binary.BigEndian.AppendUint32(body, uint32(len(t.stsc)))
		for _, e := range t.stsc {
			body = binary.BigEndian.AppendUint32(body, e[0])
			body = binary.BigEndian.AppendUint32(body, e[1])
			body = binary.BigEndian.AppendUint32(body, e[2])
		}
	case fourCC("sbgp"):
		if group != nil {
			body = binary.BigEndian.AppendUint32(body, group.groupingType)
			if group.groupingTypeParameter != nil {
				body = binary.BigEndian.AppendUint32(body, *group.groupingTypeParameter)
			}
			body = binary.BigEndian.AppendUint32(body, uint32(len(group.entries)))
			for _, e := range group.entries {
				body = binary.BigEndian.AppendUint32(body, e[0])
				body = binary.BigEndian.AppendUint32(body, e[1])
			}
		} else {
			body = binary.BigEndian.AppendUint32(body, 0)
			body = binary.BigEndian.AppendUint32(body, 0)
		}
	default:
		return 0, fmt.Errorf("unsupported table %s", typeToString(typ))
	}
	size, err := writeBox(out, newType, 8, body)
	if err == nil {
		t.written[newType] = true
	}
	return size, err
}

func writeBox(out io.Writer, typ uint32, header uint64, body []byte) (uint64, error) {
	size := header + uint64(len(body))
	if size > math.MaxUint32 && header == 8 {
		return 0, mp4Error("unsupported input", "rewritten %s box exceeds 32-bit size", typeToString(typ))
	}
	if header == 16 {
		if err := writeU32(out, 1); err != nil {
			return 0, err
		}
		if err := writeU32(out, typ); err != nil {
			return 0, err
		}
		if err := writeU64(out, size); err != nil {
			return 0, err
		}
	} else {
		if err := writeU32(out, uint32(size)); err != nil {
			return 0, err
		}
		if err := writeU32(out, typ); err != nil {
			return 0, err
		}
	}
	if _, err := out.Write(body); err != nil {
		return 0, err
	}
	return size, nil
}
func writeBoxBytes(typ uint32, body []byte) ([]byte, error) {
	var b bytesBuffer
	_, err := writeBox(&b, typ, 8, body)
	return b.data, err
}

// tiny append-only writer used to build one complete box.
type bytesBuffer struct{ data []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) { b.data = append(b.data, p...); return len(p), nil }

func patchBoxSize(out io.WriteSeeker, pos, header, size uint64) error {
	if header == 16 {
		return patchU64(out, pos+8, size)
	}
	if size > math.MaxUint32 {
		return mp4Error("unsupported input", "rewritten box exceeds 32-bit size")
	}
	return patchU32(out, pos, uint32(size))
}
func patchU32(out io.WriteSeeker, pos uint64, v uint32) error {
	return patch(out, pos, binary.BigEndian.AppendUint32(nil, v))
}
func patchU64(out io.WriteSeeker, pos uint64, v uint64) error {
	return patch(out, pos, binary.BigEndian.AppendUint64(nil, v))
}
func patch(out io.WriteSeeker, pos uint64, b []byte) error {
	at, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err = out.Seek(int64(pos), io.SeekStart); err != nil {
		return err
	}
	if _, err = out.Write(b); err != nil {
		return err
	}
	_, err = out.Seek(at, io.SeekStart)
	return err
}
func writeU32(w io.Writer, v uint32) error { return binary.Write(w, binary.BigEndian, v) }
func writeU64(w io.Writer, v uint64) error { return binary.Write(w, binary.BigEndian, v) }
func ptrU64(v uint64) *uint64              { return &v }
func writeRepeated(w io.Writer, v byte, n uint64) error {
	var buf [64 * 1024]byte
	for i := range buf {
		buf[i] = v
	}
	for n > 0 {
		m := uint64(len(buf))
		if n < m {
			m = n
		}
		if _, err := w.Write(buf[:m]); err != nil {
			return err
		}
		n -= m
	}
	return nil
}

func setDuration(body *[]byte, typ uint32, duration uint64) error {
	b := *body
	if len(b) < 4 {
		return mp4Error("invalid data", "%s is too short", typeToString(typ))
	}
	at0, at1 := 16, 24
	if typ == fourCC("tkhd") {
		at0, at1 = 20, 28
	}
	if b[0] == 1 {
		if at1+8 > len(b) {
			return mp4Error("invalid data", "%s is too short", typeToString(typ))
		}
		binary.BigEndian.PutUint64(b[at1:at1+8], duration)
		return nil
	}
	if duration < math.MaxUint32 {
		if at0+4 > len(b) {
			return mp4Error("invalid data", "%s is too short", typeToString(typ))
		}
		binary.BigEndian.PutUint32(b[at0:at0+4], uint32(duration))
		return nil
	}
	if at0+4 > len(b) {
		return mp4Error("invalid data", "%s is too short", typeToString(typ))
	}
	v := make([]byte, 0, len(b)+12)
	v = append(v, 1)
	v = append(v, b[1:4]...)
	v = binary.BigEndian.AppendUint64(v, uint64(binary.BigEndian.Uint32(b[4:8])))
	v = binary.BigEndian.AppendUint64(v, uint64(binary.BigEndian.Uint32(b[8:12])))
	v = append(v, b[12:at0]...)
	v = binary.BigEndian.AppendUint64(v, duration)
	v = append(v, b[at0+4:]...)
	*body = v
	return nil
}
func elstBody(edits []edit) ([]byte, error) {
	v := uint8(0)
	for _, e := range edits {
		if e.segmentDuration > math.MaxUint32 || e.mediaTime < math.MinInt32 || e.mediaTime > math.MaxInt32 {
			v = 1
		}
	}
	b := make([]byte, 4)
	b[0] = v
	b = binary.BigEndian.AppendUint32(b, uint32(len(edits)))
	for _, e := range edits {
		if v == 1 {
			b = binary.BigEndian.AppendUint64(b, e.segmentDuration)
			b = binary.BigEndian.AppendUint64(b, uint64(e.mediaTime))
		} else {
			b = binary.BigEndian.AppendUint32(b, uint32(e.segmentDuration))
			b = binary.BigEndian.AppendUint32(b, uint32(int32(e.mediaTime)))
		}
		b = binary.BigEndian.AppendUint32(b, uint32(e.mediaRate))
	}
	return b, nil
}

func withoutSegmentTable(data []byte) ([]byte, bool) {
	kept := make([]byte, 0, len(data))
	found := false
	pos := 0
	for pos+8 <= len(data) {
		size := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		header := uint64(8)
		if size == 1 {
			if pos+16 > len(data) {
				break
			}
			size = binary.BigEndian.Uint64(data[pos+8 : pos+16])
			header = 16
		} else if size == 0 {
			break
		}
		if size < header || size > uint64(len(data)-pos) {
			break
		}
		end := pos + int(size)
		if binary.BigEndian.Uint32(data[pos+4:pos+8]) == fourCC("MTRM") {
			found = true
		} else {
			kept = append(kept, data[pos:end]...)
		}
		pos = end
	}
	kept = append(kept, data[pos:]...)
	return kept, found
}

func compactGroupBox(g compactToGroup) ([]byte, bool) {
	body := make([]byte, 0, 20+len(g.patterns)*8)
	for _, v := range []uint32{0, g.groupingType, 0, 4, uint32(len(g.patterns))} {
		body = binary.BigEndian.AppendUint32(body, v)
	}
	var indices []byte
	for _, p := range g.patterns {
		if len(p.indices) > math.MaxUint32 {
			return nil, false
		}
		body = binary.BigEndian.AppendUint32(body, uint32(len(p.indices)))
		body = binary.BigEndian.AppendUint32(body, p.sampleCount)
		for _, i := range p.indices {
			if i >= 16 {
				return nil, false
			}
			indices = append(indices, byte(i))
		}
	}
	for i := 0; i < len(indices); i += 2 {
		v := indices[i] << 4
		if i+1 < len(indices) {
			v |= indices[i+1]
		}
		body = append(body, v)
	}
	b, err := writeBoxBytes(fourCC("csgm"), body)
	return b, err == nil
}
