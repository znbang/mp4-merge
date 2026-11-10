package mp4merge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

func readDescription(r io.ReadSeeker, d *desc, track int, maxRead uint64, fileIndex int, parent uint32) error {
	startPos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if startPos < 0 || uint64(startPos) > ^uint64(0)-maxRead {
		return mp4Error("invalid data", "container offset overflows")
	}
	parentEnd := uint64(startPos) + maxRead
	if len(d.parts) <= fileIndex {
		d.parts = append(d.parts, make([]partDesc, fileIndex-len(d.parts)+1)...)
	}
	part := &d.parts[fileIndex]
	once := make(map[uint32]bool)
	pos := uint64(startPos)
	for pos < parentEnd {
		if parentEnd-pos < 8 {
			if parent == 0 {
				break
			}
			rest := make([]byte, int(parentEnd-pos))
			if _, err := io.ReadFull(r, rest); err != nil {
				return inFile(fileIndex, err)
			}
			if bytes.Equal(rest, []byte{0, 0, 0, 0}) {
				break
			}
			return inFile(fileIndex, mp4Error("invalid data", "the %s box ends with %d bytes at %d that are not a box", typeToString(parent), len(rest), pos))
		}
		h, err := readBoxWithin(r, part.end, parentEnd)
		if err != nil {
			if parent == 0 && part.moov > 0 {
				break // trailing Insta360 metadata, padding or other non-box bytes
			}
			return inFile(fileIndex, err)
		}
		if parent == 0 && h.typ == 0 {
			break
		}
		pos = h.end
		if parent == 0 {
			part.walkEnd = h.end
		}
		if onlyOne(parent, h.typ) {
			key := h.typ
			if parent == fourCC("stbl") && (h.typ == fourCC("stsz") || h.typ == fourCC("stz2")) {
				key = fourCC("stsz")
			}
			if parent == fourCC("stbl") && (h.typ == fourCC("stco") || h.typ == fourCC("co64")) {
				key = fourCC("stco")
			}
			if once[key] {
				return inFile(fileIndex, mp4Error("invalid data", "the %s box at %d is a duplicate in %s", typeToString(h.typ), h.offset, typeToString(parent)))
			}
			once[key] = true
		}
		if h.typ == fourCC("moof") && parent == 0 {
			return mp4Error("unsupported input", "files can't be merged: file %d is fragmented MP4 (moof at %d)", fileIndex+1, h.offset)
		}
		if h.typ == fourCC("mvex") && parent == fourCC("moov") {
			part.mvex = true
		}
		if h.typ == fourCC("mdat") && parent == 0 {
			part.mdats = append(part.mdats, [2]uint64{h.offset + h.headerSize, h.end - h.offset - h.headerSize})
		}
		if hasChildren(h.typ) {
			if parentOf(h.typ) != parent {
				return inFile(fileIndex, mp4Error("invalid data", "the %s box at %d is in %s, but belongs in %s", typeToString(h.typ), h.offset, boxPlace(parent), boxPlace(parentOf(h.typ))))
			}
			tlTrack := track
			switch h.typ {
			case fourCC("moov"):
				part.moov++
			case fourCC("trak"):
				part.tracks = append(part.tracks, partTrack{})
				tlTrack = len(part.tracks) - 1
			case fourCC("edts"):
				if track >= 0 && track < len(part.tracks) {
					part.tracks[track].hasEdts = true
				}
			}
			if err := readDescription(r, d, tlTrack, h.end-h.offset-h.headerSize, fileIndex, h.typ); err != nil {
				return err
			}
			if _, err := r.Seek(int64(h.end), io.SeekStart); err != nil {
				return err
			}
		} else {
			bodySize := h.end - h.offset - h.headerSize
			readBody := func() ([]byte, error) { return readBody(r, bodySize, h.typ, h.offset) }
			if h.typ == fourCC("udta") && parent == fourCC("moov") && !part.hasSegmentTbl {
				body, err := readBody()
				if err != nil {
					return inFile(fileIndex, err)
				}
				part.segmentTables = segmentPayloads(body)
				part.hasSegmentTbl = true
			}
			if h.typ == fourCC("mvhd") && parent == fourCC("moov") {
				body, err := readBody()
				if err != nil {
					return inFile(fileIndex, err)
				}
				part.mvhdTimescale, part.mvhdDuration, err = headerFields(h.typ, body)
				if err != nil {
					return inFile(fileIndex, fieldError(err, h.typ, h.offset))
				}
				if len(body) > 0 && body[0] == 1 {
					part.mvhdCreation = binary.BigEndian.Uint64(body[4:12])
				} else if len(body) >= 8 {
					part.mvhdCreation = uint64(binary.BigEndian.Uint32(body[4:8]))
				}
			}
			if track >= 0 && track < len(part.tracks) {
				td := &part.tracks[track]
				switch {
				case h.typ == fourCC("tkhd") && parent == fourCC("trak"):
					body, err := readBody()
					if err != nil {
						return inFile(fileIndex, err)
					}
					_, td.tkhdDuration, err = headerFields(h.typ, body)
					if err != nil {
						return inFile(fileIndex, fieldError(err, h.typ, h.offset))
					}
				case h.typ == fourCC("mdhd") && parent == fourCC("mdia"):
					body, err := readBody()
					if err != nil {
						return inFile(fileIndex, err)
					}
					td.mdhdTimescale, td.mdhdDuration, err = headerFields(h.typ, body)
					if err != nil {
						return inFile(fileIndex, fieldError(err, h.typ, h.offset))
					}
				case h.typ == fourCC("hdlr") && parent == fourCC("mdia"):
					body, err := readBody()
					if err != nil {
						return inFile(fileIndex, err)
					}
					if len(body) < 12 {
						return inFile(fileIndex, mp4Error("invalid data", "the hdlr box at %d is too short", h.offset))
					}
					td.handler = binary.BigEndian.Uint32(body[8:12])
				case h.typ == fourCC("elst") && (parent == fourCC("edts") || parent == fourCC("trak")):
					if td.hasElst {
						return inFile(fileIndex, mp4Error("invalid data", "track %d has more than one elst box", track+1))
					}
					body, err := readBody()
					if err != nil {
						return inFile(fileIndex, err)
					}
					td.elst, err = editList(body)
					if err != nil {
						return inFile(fileIndex, fieldError(err, h.typ, h.offset))
					}
					td.hasElst = true
				case parent == fourCC("stbl"):
					if err := readSampleTable(r, h.typ, h.offset, bodySize, td); err != nil {
						return inFile(fileIndex, err)
					}
				}
			}
			if _, err := r.Seek(int64(h.end), io.SeekStart); err != nil {
				return err
			}
		}
		if h.size == 0 {
			break
		}
	}
	return nil
}

func boxPlace(parent uint32) string {
	if parent == 0 {
		return "at the top level"
	}
	return "in a " + typeToString(parent) + " box"
}

func fieldError(err error, typ uint32, offset uint64) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return mp4Error("invalid data", "the %s box at %d is too short for its fields", typeToString(typ), offset)
	}
	return fmt.Errorf("the %s box at %d: %w", typeToString(typ), offset, err)
}

func readU32(b []byte, at int) (uint32, bool) {
	if at < 0 || at+4 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[at : at+4]), true
}

func headerFields(typ uint32, body []byte) (uint32, uint64, error) {
	if len(body) < 4 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	v := body[0]
	if v > 1 {
		return 0, 0, fmt.Errorf("unknown version %d", v)
	}
	at := 4
	if v == 1 {
		at += 16
	} else {
		at += 8
	}
	field, ok := readU32(body, at)
	if !ok {
		return 0, 0, io.ErrUnexpectedEOF
	}
	at += 4
	if typ == fourCC("tkhd") {
		at += 4
	}
	if v == 1 {
		if at+8 > len(body) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		return field, binary.BigEndian.Uint64(body[at : at+8]), nil
	}
	if at+4 > len(body) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	return field, uint64(binary.BigEndian.Uint32(body[at : at+4])), nil
}

func editList(body []byte) ([]edit, error) {
	if len(body) < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	v := body[0]
	if v > 1 {
		return nil, fmt.Errorf("unknown version %d", v)
	}
	count := binary.BigEndian.Uint32(body[4:8])
	width := 12
	if v == 1 {
		width = 20
	}
	if uint64(count)*uint64(width) > uint64(len(body)-8) {
		return nil, fmt.Errorf("entry count exceeds box")
	}
	out := make([]edit, 0, count)
	at := 8
	for i := uint32(0); i < count; i++ {
		var e edit
		if v == 1 {
			e.segmentDuration = binary.BigEndian.Uint64(body[at : at+8])
			e.mediaTime = int64(binary.BigEndian.Uint64(body[at+8 : at+16]))
			at += 16
		} else {
			e.segmentDuration = uint64(binary.BigEndian.Uint32(body[at : at+4]))
			e.mediaTime = int64(int32(binary.BigEndian.Uint32(body[at+4 : at+8])))
			at += 8
		}
		e.mediaRate = int32(binary.BigEndian.Uint32(body[at : at+4]))
		at += 4
		out = append(out, e)
	}
	return out, nil
}

func readSampleTable(r io.Reader, typ uint32, offset, size uint64, t *partTrack) error {
	if typ == fourCC("stsd") {
		body, err := readBody(r, size, typ, offset)
		if err != nil {
			return err
		}
		count := uint32(0)
		if len(body) >= 8 {
			count = binary.BigEndian.Uint32(body[4:8])
		}
		pos := 8
		for i := uint32(0); i < count; i++ {
			if pos+8 > len(body) {
				break
			}
			entrySize := uint64(binary.BigEndian.Uint32(body[pos : pos+4]))
			if entrySize < 8 || entrySize > uint64(len(body)-pos) {
				break
			}
			entry := body[pos : pos+int(entrySize)]
			format := binary.BigEndian.Uint32(entry[4:8])
			if format == fourCC("tmcd") {
				t.skip = true
			}
			if format == fourCC("encv") || format == fourCC("enca") || format == fourCC("enct") || format == fourCC("encs") {
				t.encrypted = true
			}
			t.sampleEntries = append(t.sampleEntries, sampleEntryOf(t.handler, entry, pos))
			pos += int(entrySize)
		}
		t.stsd = body
		return nil
	}
	if typ == fourCC("cslg") {
		t.hasCSLG = true
	}
	if typ == fourCC("saiz") || typ == fourCC("saio") || typ == fourCC("senc") {
		t.encrypted = true
	}
	if typ == fourCC("csgm") {
		body, err := readBody(r, size, typ, offset)
		if err != nil {
			return err
		}
		if g, ok := compactToGroupOf(body); ok {
			t.csgm = append(t.csgm, g)
		} else {
			t.unmerged = append(t.unmerged, typ)
		}
		return nil
	}
	if typ == fourCC("csgp") || typ == fourCC("subs") || typ == fourCC("padb") || typ == fourCC("stsh") || typ == fourCC("stdp") {
		t.unmerged = append(t.unmerged, typ)
	}
	if typ == fourCC("sgpd") {
		body, err := readBody(r, size, typ, offset)
		if err != nil {
			return err
		}
		if len(body) >= 8 {
			t.sgpd = append(t.sgpd, sgpd{groupingType: binary.BigEndian.Uint32(body[4:8]), body: body})
		}
		return nil
	}
	switch typ {
	case 0:
		return nil
	case fourCC("stts"), fourCC("ctts"), fourCC("stsz"), fourCC("stz2"), fourCC("stss"), fourCC("stps"), fourCC("stco"), fourCC("co64"), fourCC("sdtp"), fourCC("stsc"), fourCC("sbgp"):
	default:
		return nil
	}
	body, err := readBody(r, size, typ, offset)
	if err != nil {
		return err
	}
	return sampleTable(typ, body, t)
}

func sampleTable(typ uint32, body []byte, t *partTrack) error {
	if len(body) < 4 {
		return io.ErrUnexpectedEOF
	}
	v := body[0]
	at := 4
	need := func(n uint64) error {
		if n > uint64(len(body)-at) {
			return mp4Error("invalid data", "%s table is too short", typeToString(typ))
		}
		return nil
	}
	switch typ {
	case fourCC("stsz"):
		if err := need(8); err != nil {
			return err
		}
		t.stszSampleSize = binary.BigEndian.Uint32(body[at : at+4])
		t.stszCount = binary.BigEndian.Uint32(body[at+4 : at+8])
		at += 8
		if t.stszSampleSize == 0 {
			if err := need(uint64(t.stszCount) * 4); err != nil {
				return err
			}
			if uint64(t.stszCount) > uint64(maxInt()) {
				return mp4Error("invalid data", "stsz count is too large")
			}
			t.stsz = make([]uint32, 0, int(t.stszCount))
			for i := uint32(0); i < t.stszCount; i++ {
				t.stsz = append(t.stsz, binary.BigEndian.Uint32(body[at:at+4]))
				at += 4
			}
		}
	case fourCC("stz2"):
		if err := need(8); err != nil {
			return err
		}
		fieldSize := uint32(body[at+3])
		t.stszCount = binary.BigEndian.Uint32(body[at+4 : at+8])
		at += 8
		if fieldSize != 4 && fieldSize != 8 && fieldSize != 16 {
			return mp4Error("invalid data", "stz2 has field_size %d", fieldSize)
		}
		if err := need((uint64(t.stszCount)*uint64(fieldSize) + 7) / 8); err != nil {
			return err
		}
		if uint64(t.stszCount) > uint64(maxInt()) {
			return mp4Error("invalid data", "stz2 count is too large")
		}
		for i := uint32(0); i < t.stszCount; i++ {
			switch fieldSize {
			case 4:
				b := body[at]
				if i%2 == 0 {
					t.stsz = append(t.stsz, uint32(b>>4))
				} else {
					t.stsz = append(t.stsz, uint32(b&15))
					at++
				}
			case 8:
				t.stsz = append(t.stsz, uint32(body[at]))
				at++
			case 16:
				t.stsz = append(t.stsz, uint32(binary.BigEndian.Uint16(body[at:at+2])))
				at += 2
			}
		}
	case fourCC("sdtp"):
		t.sdtp = append([]byte(nil), body[4:]...)
		t.hasSDTP = true
	case fourCC("sbgp"):
		if v > 1 {
			return mp4Error("invalid data", "sbgp has unknown version %d", v)
		}
		if err := need(8); err != nil {
			return err
		}
		g := sampleToGroup{version: v, groupingType: binary.BigEndian.Uint32(body[at : at+4])}
		at += 4
		if v == 1 {
			if err := need(4); err != nil {
				return err
			}
			x := binary.BigEndian.Uint32(body[at : at+4])
			g.groupingTypeParameter = &x
			at += 4
		}
		if err := need(4); err != nil {
			return err
		}
		count := binary.BigEndian.Uint32(body[at : at+4])
		at += 4
		if uint64(count)*8 > uint64(len(body)-at) {
			return mp4Error("invalid data", "sbgp entry count exceeds box")
		}
		g.entries = make([][2]uint32, 0, count)
		for i := uint32(0); i < count; i++ {
			g.entries = append(g.entries, [2]uint32{binary.BigEndian.Uint32(body[at : at+4]), binary.BigEndian.Uint32(body[at+4 : at+8])})
			at += 8
		}
		t.sbgp = append(t.sbgp, g)
	case fourCC("stss"), fourCC("stps"), fourCC("stco"), fourCC("co64"), fourCC("stts"), fourCC("ctts"), fourCC("stsc"):
		if typ == fourCC("ctts") && v > 1 {
			return mp4Error("invalid data", "ctts has unknown version %d", v)
		}
		if err := need(4); err != nil {
			return err
		}
		count := binary.BigEndian.Uint32(body[at : at+4])
		at += 4
		entrySize := uint64(8)
		switch typ {
		case fourCC("stss"), fourCC("stps"), fourCC("stco"):
			entrySize = 4
		case fourCC("co64"):
			entrySize = 8
		case fourCC("stsc"):
			entrySize = 12
		}
		if uint64(count)*entrySize > uint64(len(body)-at) {
			return mp4Error("invalid data", "%s entry count exceeds box", typeToString(typ))
		}
		switch typ {
		case fourCC("stss"):
			t.stss = make([]uint32, 0, count)
			t.hasSTSS = true
		case fourCC("stps"):
			t.stps = make([]uint32, 0, count)
			t.hasSTPS = true
		case fourCC("ctts"):
			t.ctts = make([]cttsEntry, 0, count)
			t.cttsVersion = v
			t.hasCTTS = true
		case fourCC("stco"), fourCC("co64"):
			t.stco = make([]uint64, 0, count)
		case fourCC("stts"):
			t.stts = make([][2]uint32, 0, count)
		case fourCC("stsc"):
			t.stsc = make([][3]uint32, 0, count)
		}
		for i := uint32(0); i < count; i++ {
			switch typ {
			case fourCC("stss"):
				t.stss = append(t.stss, binary.BigEndian.Uint32(body[at:at+4]))
				at += 4
			case fourCC("stps"):
				t.stps = append(t.stps, binary.BigEndian.Uint32(body[at:at+4]))
				at += 4
			case fourCC("stco"):
				t.stco = append(t.stco, uint64(binary.BigEndian.Uint32(body[at:at+4])))
				at += 4
			case fourCC("co64"):
				t.stco = append(t.stco, binary.BigEndian.Uint64(body[at:at+8]))
				at += 8
			case fourCC("stts"):
				t.stts = append(t.stts, [2]uint32{binary.BigEndian.Uint32(body[at : at+4]), binary.BigEndian.Uint32(body[at+4 : at+8])})
				at += 8
			case fourCC("ctts"):
				t.ctts = append(t.ctts, cttsEntry{binary.BigEndian.Uint32(body[at : at+4]), int32(binary.BigEndian.Uint32(body[at+4 : at+8]))})
				at += 8
			case fourCC("stsc"):
				t.stsc = append(t.stsc, [3]uint32{binary.BigEndian.Uint32(body[at : at+4]), binary.BigEndian.Uint32(body[at+4 : at+8]), binary.BigEndian.Uint32(body[at+8 : at+12])})
				at += 12
			}
		}
	}
	return nil
}

func sampleEntryOf(handler uint32, entry []byte, at int) sampleEntry {
	format := binary.BigEndian.Uint32(entry[4:8])
	e := append([]byte(nil), entry...)
	var config *decoderConfig
	var boxesAt int
	switch handler {
	case fourCC("soun"):
		boxesAt = 36
		if len(e) >= 18 {
			switch binary.BigEndian.Uint16(e[16:18]) {
			case 1:
				boxesAt = 52
			case 2:
				boxesAt = 72
			}
		}
	case fourCC("vide"):
		boxesAt = 86
	default:
		return sampleEntry{format: format, at: at, comparable: e}
	}
	withoutBitrates(e, boxesAt, len(e), &config)
	if config != nil && config.at[0] >= 0 && config.at[1] <= len(e) {
		e = append(e[:config.at[0]], e[config.at[1]:]...)
		if len(e) >= 4 {
			for i := 0; i < 4; i++ {
				e[i] = 0
			}
		}
	}
	return sampleEntry{format: format, at: at, comparable: e, config: config}
}

func withoutBitrates(b []byte, pos, end int, config **decoderConfig) {
	for pos+8 <= end {
		size := int(binary.BigEndian.Uint32(b[pos : pos+4]))
		if size < 8 || size > end-pos {
			break
		}
		typ := string(b[pos+4 : pos+8])
		switch typ {
		case "btrt":
			for i := pos + 8; i < pos+size; i++ {
				b[i] = 0
			}
		case "esds":
			if size > 12 {
				esdsWithoutBitrates(b[pos+12 : pos+size])
			}
		case "wave":
			withoutBitrates(b, pos+8, pos+size, config)
		case "avcC", "hvcC":
			if *config == nil {
				*config = decoderConfigOf(b[pos:pos+size], pos)
			}
		}
		pos += size
	}
}

func decoderConfigOf(b []byte, at int) *decoderConfig {
	if len(b) < 8 {
		return nil
	}
	typ := binary.BigEndian.Uint32(b[4:8])
	c := b[8:]
	pos := 0
	out := &decoderConfig{typ: typ, at: [2]int{at, at + len(b)}}
	readNALs := func(count int) ([][]byte, bool) {
		if count < 0 {
			return nil, false
		}
		list := make([][]byte, 0, count)
		for i := 0; i < count; i++ {
			if pos+2 > len(c) {
				return nil, false
			}
			n := int(binary.BigEndian.Uint16(c[pos : pos+2]))
			pos += 2
			if n > len(c)-pos {
				return nil, false
			}
			list = append(list, append([]byte(nil), c[pos:pos+n]...))
			pos += n
		}
		return list, true
	}
	if typ == fourCC("avcC") {
		pos = 5
		if pos >= len(c) {
			return nil
		}
		sps := c[pos]
		pos++
		nals, ok := readNALs(int(sps & 31))
		if !ok {
			return nil
		}
		out.arrays = append(out.arrays, nalArray{7, sps & 0xe0, nals})
		if pos >= len(c) {
			return nil
		}
		pps := int(c[pos])
		pos++
		nals, ok = readNALs(pps)
		if !ok {
			return nil
		}
		out.arrays = append(out.arrays, nalArray{8, 0, nals})
		out.header = append([]byte(nil), c[:5]...)
	} else if typ == fourCC("hvcC") {
		pos = 22
		if pos >= len(c) {
			return nil
		}
		count := int(c[pos])
		pos++
		for i := 0; i < count; i++ {
			if pos+3 > len(c) {
				return nil
			}
			by := c[pos]
			n := int(binary.BigEndian.Uint16(c[pos+1 : pos+3]))
			pos += 3
			nals, ok := readNALs(n)
			if !ok {
				return nil
			}
			out.arrays = append(out.arrays, nalArray{by & 0x3f, by, nals})
		}
		if len(c) < 22 {
			return nil
		}
		out.header = append([]byte(nil), c[:22]...)
	} else {
		return nil
	}
	out.trailing = append([]byte(nil), c[pos:]...)
	return out
}

func esdsWithoutBitrates(es []byte) {
	header := func(pos int) (tag byte, body, size int, ok bool) {
		if pos >= len(es) {
			return
		}
		tag = es[pos]
		n := 0
		for i := 1; i <= 4; i++ {
			if pos+i >= len(es) {
				return
			}
			x := es[pos+i]
			n = (n << 7) | int(x&0x7f)
			if x&0x80 == 0 {
				return tag, pos + i + 1, n, true
			}
		}
		return
	}
	tag, pos, _, ok := header(0)
	if !ok || tag != 3 || pos+3 > len(es) {
		return
	}
	flags := es[pos+2]
	pos += 3
	if flags&0x80 != 0 {
		pos += 2
	}
	if flags&0x40 != 0 && pos < len(es) {
		pos += 1 + int(es[pos])
	}
	if flags&0x20 != 0 {
		pos += 2
	}
	tag, body, size, ok := header(pos)
	if ok && tag == 4 && size >= 13 && body+13 <= len(es) {
		for i := body + 2; i < body+13; i++ {
			es[i] = 0
		}
	}
}

func compactToGroupOf(body []byte) (compactToGroup, bool) {
	field := func(at int) (uint32, bool) {
		if at < 0 || at+4 > len(body) {
			return 0, false
		}
		return binary.BigEndian.Uint32(body[at : at+4]), true
	}
	version, ok := field(0)
	if !ok || version != 0 {
		return compactToGroup{}, false
	}
	group, ok := field(4)
	if !ok {
		return compactToGroup{}, false
	}
	reserved, ok := field(8)
	if !ok || reserved != 0 {
		return compactToGroup{}, false
	}
	bits, ok := field(12)
	if !ok || bits != 4 {
		return compactToGroup{}, false
	}
	count, ok := field(16)
	if !ok || uint64(count) > uint64((len(body)-20)/8) {
		return compactToGroup{}, false
	}
	type lens struct{ n, s uint32 }
	ls := make([]lens, 0, count)
	var total uint64
	for i := uint32(0); i < count; i++ {
		n, _ := field(20 + int(i)*8)
		s, _ := field(24 + int(i)*8)
		if n == 0 || n > s {
			return compactToGroup{}, false
		}
		ls = append(ls, lens{n, s})
		total += uint64(n)
	}
	start := 20 + int(count)*8
	if uint64(len(body)-start) != (total+1)/2 || total%2 != 0 && body[len(body)-1]&15 != 0 {
		return compactToGroup{}, false
	}
	get := func(k uint64) uint32 {
		b := body[start+int(k/2)]
		if k%2 == 0 {
			return uint32(b >> 4)
		}
		return uint32(b & 15)
	}
	var patterns []groupPattern
	var at uint64
	for _, x := range ls {
		ids := make([]uint32, x.n)
		for i := range ids {
			ids[i] = get(at)
			at++
		}
		patterns = append(patterns, groupPattern{sampleCount: x.s, indices: ids})
	}
	return compactToGroup{groupingType: group, patterns: patterns}, true
}

func decoderPPSID(typ uint32, nalu []byte) (uint32, bool) {
	header := 1
	if typ == fourCC("hvcC") {
		header = 2
	}
	if len(nalu) < header {
		return 0, false
	}
	rbsp := make([]byte, 0, 8)
	zeros := 0
	for _, x := range nalu[header:] {
		if len(rbsp) >= 16 {
			break
		}
		if zeros >= 2 && x == 3 {
			zeros = 0
			continue
		}
		if x == 0 {
			zeros++
		} else {
			zeros = 0
		}
		rbsp = append(rbsp, x)
	}
	bit := func(i int) (uint32, bool) {
		if i < 0 || i/8 >= len(rbsp) {
			return 0, false
		}
		return uint32(rbsp[i/8] >> (7 - i%8) & 1), true
	}
	i := 0
	for {
		b, ok := bit(i)
		if !ok {
			return 0, false
		}
		if b == 1 {
			break
		}
		i++
		if i > 31 {
			return 0, false
		}
	}
	leading := i
	var v uint32
	for k := 0; k < leading; k++ {
		b, ok := bit(leading + 1 + k)
		if !ok {
			return 0, false
		}
		v = v<<1 | b
	}
	return (uint32(1) << leading) - 1 + v, true
}
