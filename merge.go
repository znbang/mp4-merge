package mp4merge

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"sort"
)

func merge(d *desc) error {
	if len(d.parts) == 0 {
		return nil
	}
	first := &d.parts[0]
	for i := range d.parts {
		p := &d.parts[i]
		if p.mvhdTimescale == 0 {
			return mp4Error("invalid data", "file %d has no movie timescale (mvhd)", i+1)
		}
		if len(p.tracks) != len(first.tracks) {
			return mp4Error("unsupported input", "files cannot be merged: file %d has %d tracks and file 1 has %d", i+1, len(p.tracks), len(first.tracks))
		}
		if p.mvex && allSamplesZero(p) {
			return mp4Error("unsupported input", "file %d is fragmented MP4 without samples in its sample tables", i+1)
		}
		for t := range first.tracks {
			a, b := &first.tracks[t], &p.tracks[t]
			if a.handler != b.handler {
				return mp4Error("unsupported input", "track %d has handler %q in file 1 and %q in file %d", t+1, typeToString(a.handler), typeToString(b.handler), i+1)
			}
			if b.mdhdTimescale == 0 {
				return mp4Error("invalid data", "track %d of file %d has no media timescale (mdhd)", t+1, i+1)
			}
		}
	}

	d.movieTimescale = first.mvhdTimescale
	ends := make([]uint64, len(d.parts))
	var movieEnd uint64
	for i := range d.parts {
		end, err := partMovieEnd(&d.parts[i], d.movieTimescale)
		if err != nil {
			return err
		}
		ends[i] = end
		movieEnd, err = add(movieEnd, end)
		if err != nil {
			return err
		}
	}
	d.continues = junctions(d.parts)
	d.partStarts = make([]uint64, len(d.parts))
	anchor := -1
	for t := range first.tracks {
		if first.tracks[t].skip {
			continue
		}
		if first.tracks[t].handler != fourCC("vide") {
			continue
		}
		for i := range d.parts {
			for _, e := range d.parts[i].tracks[t].stts {
				if e[0] > 0 && e[1] > 0 {
					anchor = t
					break
				}
			}
			if anchor == t {
				break
			}
		}
		if anchor >= 0 {
			break
		}
	}
	if anchor < 0 {
		var at uint64
		for i := range d.parts {
			d.partStarts[i] = at
			var err error
			at, err = add(at, ends[i])
			if err != nil {
				return err
			}
		}
	} else {
		anchorDuration := make([]uint64, len(d.parts))
		anchorPresents := make([]bool, len(d.parts))
		for i := range d.parts {
			track := &d.parts[i].tracks[anchor]
			v, err := presentedDuration(track, track.mdhdTimescale, d.parts[i].mvhdTimescale)
			if err != nil {
				return err
			}
			anchorDuration[i], err = rescaleUp(v, d.parts[i].mvhdTimescale, d.movieTimescale)
			if err != nil {
				return err
			}
			anchorPresents[i] = track.stszCount > 0 && (!track.hasElst || hasMediaEdit(track.elst))
		}
		for i := 1; i < len(d.parts); i++ {
			prevEnd, err := add(d.partStarts[i-1], anchorDuration[i-1])
			if err != nil {
				return err
			}
			if d.continues[i-1] {
				d.partStarts[i] = prevEnd
				continue
			}
			if !anchorPresents[i-1] {
				prevEnd, err = add(d.partStarts[i-1], ends[i-1])
				if err != nil {
					return err
				}
			}
			d.partStarts[i] = prevEnd
		}
	}

	d.tracks = make([]mergedTrack, len(first.tracks))
	var presents uint64
	for t := range first.tracks {
		merged, end, err := mergeTrack(d, t, d.movieTimescale, d.partStarts, d.continues)
		if err != nil {
			return err
		}
		d.tracks[t] = merged
		if end > presents {
			presents = end
		}
	}
	if presents > movieEnd {
		movieEnd = presents
	}
	if len(d.parts) > 0 {
		movieEnd = 0
		for i, end := range ends {
			if i+1 < len(ends) && !d.continues[i] {
				movieEnd = d.partStarts[i+1]
			} else {
				var err error
				movieEnd, err = add(movieEnd, end)
				if err != nil {
					return err
				}
			}
		}
		if presents > movieEnd {
			movieEnd = presents
		}
	}
	d.movieDuration = movieEnd
	return nil
}

func allSamplesZero(p *partDesc) bool {
	for i := range p.tracks {
		if p.tracks[i].stszCount != 0 {
			return false
		}
	}
	return true
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func rescaleExact(value uint64, from, to uint32) (uint64, bool) {
	if from == 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(value, uint64(to))
	if hi >= uint64(from) {
		return 0, false
	}
	q, r := bits.Div64(hi, lo, uint64(from))
	return q, r == 0
}

func rescaleUp(value uint64, from, to uint32) (uint64, error) {
	if from == 0 {
		return 0, mp4Error("invalid data", "cannot rescale a duration from timescale 0")
	}
	hi, lo := bits.Mul64(value, uint64(to))
	if hi >= uint64(from) {
		return 0, mp4Error("invalid data", "rescaled duration exceeds 64 bits")
	}
	q, r := bits.Div64(hi, lo, uint64(from))
	if r != 0 {
		if q == math.MaxUint64 {
			return 0, mp4Error("invalid data", "rescaled duration exceeds 64 bits")
		}
		q++
	}
	return q, nil
}

func mediaTimescale(tracks []*partTrack, name string) (uint32, error) {
	first := uint64(tracks[0].mdhdTimescale)
	scale := first
	for i, x := range tracks {
		ts := uint64(x.mdhdTimescale)
		g := ts
		for _, e := range x.stts {
			g = gcd(g, uint64(e[1]))
		}
		for _, e := range x.ctts {
			a := uint64(e.sampleOffset)
			if e.sampleOffset < 0 {
				a = uint64(-int64(e.sampleOffset))
			}
			g = gcd(g, a)
		}
		for _, e := range x.elst {
			if e.mediaTime > 0 {
				g = gcd(g, uint64(e.mediaTime))
			}
		}
		if g == 0 {
			g = ts
		}
		coarsest := ts / g
		div := gcd(scale, coarsest)
		if scale/div > math.MaxUint64/coarsest {
			return 0, mp4Error("unsupported input", "%s requires a media timescale above 32 bits", name)
		}
		scale = scale / div * coarsest
		if scale > math.MaxUint32 {
			return 0, mp4Error("unsupported input", "%s has no 32-bit media timescale that preserves file %d exactly", name, i+1)
		}
	}
	return uint32(scale), nil
}

func mediaDuration(x *partTrack) (uint64, error) {
	var sum uint64
	for _, e := range x.stts {
		prod := uint64(e[0]) * uint64(e[1])
		var err error
		sum, err = add(sum, prod)
		if err != nil {
			return 0, err
		}
	}
	return sum, nil
}

func compositionEnd(x *partTrack) (int64, error) {
	dts := int64(0)
	end := int64(0)
	if !x.hasCTTS {
		n, err := mediaDuration(x)
		if err != nil || n > math.MaxInt64 {
			return 0, mp4Error("invalid data", "composition time exceeds signed range")
		}
		return int64(n), nil
	}
	cttsIndex := 0
	var cttsLeft uint32
	var offset int32
	for _, run := range x.stts {
		for j := uint32(0); j < run[0]; j++ {
			for cttsLeft == 0 && cttsIndex < len(x.ctts) {
				offset = x.ctts[cttsIndex].sampleOffset
				cttsLeft = x.ctts[cttsIndex].sampleCount
				cttsIndex++
			}
			v := dts + int64(offset) + int64(run[1])
			if v > end {
				end = v
			}
			dts += int64(run[1])
			if cttsLeft > 0 {
				cttsLeft--
			} else {
				offset = 0
			}
			if dts < 0 {
				return 0, mp4Error("invalid data", "decode time overflows")
			}
		}
	}
	return end, nil
}

func presentedDuration(x *partTrack, mediaScale, movieScale uint32) (uint64, error) {
	end, err := compositionEnd(x)
	if err != nil {
		return 0, err
	}
	if end < 0 {
		return 0, nil
	}
	if !x.hasElst {
		return rescaleUp(uint64(end), mediaScale, movieScale)
	}
	var total uint64
	for _, e := range x.elst {
		if e.mediaTime < 0 || e.mediaRate != mediaRateOne {
			total, err = add(total, e.segmentDuration)
			if err != nil {
				return 0, err
			}
			continue
		}
		remaining := int64(end) - e.mediaTime
		if remaining < 0 {
			remaining = 0
		}
		mediaDuration, er := rescaleUp(uint64(remaining), mediaScale, movieScale)
		if er != nil {
			return 0, er
		}
		if mediaDuration > e.segmentDuration {
			mediaDuration = e.segmentDuration
		}
		total, err = add(total, mediaDuration)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func partMovieEnd(p *partDesc, movieScale uint32) (uint64, error) {
	var end uint64
	for i := range p.tracks {
		t := &p.tracks[i]
		if t.skip || t.stszCount == 0 {
			continue
		}
		v, err := presentedDuration(t, t.mdhdTimescale, p.mvhdTimescale)
		if err != nil {
			return 0, err
		}
		if v > end {
			end = v
		}
	}
	if end > p.mvhdDuration {
		end = p.mvhdDuration
	}
	return rescaleUp(end, p.mvhdTimescale, movieScale)
}

func mergeTrack(d *desc, index int, movieScale uint32, starts []uint64, continues []bool) (mergedTrack, uint64, error) {
	parts := d.parts
	inputs := make([]*partTrack, len(parts))
	for i := range parts {
		inputs[i] = &parts[i].tracks[index]
	}
	first := inputs[0]
	name := fmt.Sprintf("track %d (%q)", index+1, typeToString(first.handler))
	out := mergedTrack{written: make(map[uint32]bool)}
	for i, x := range inputs {
		duration, err := rescaleUp(x.tkhdDuration, parts[i].mvhdTimescale, movieScale)
		if err != nil {
			return out, 0, err
		}
		out.tkhdDuration, err = add(out.tkhdDuration, duration)
		if err != nil {
			return out, 0, err
		}
	}
	if first.skip {
		if err := checkChunks(parts[0].mdat, first, name, 0); err != nil {
			return out, 0, err
		}
		return mergeTimecodeTrack(parts, inputs, movieScale, out)
	}
	if first.encrypted {
		return out, 0, mp4Error("unsupported input", "%s is encrypted", name)
	}
	for i, x := range inputs {
		var samples uint64
		for _, e := range x.stts {
			samples += uint64(e[0])
		}
		if samples != uint64(x.stszCount) {
			return out, 0, mp4Error("invalid data", "%s file %d has %d stts samples and %d stsz samples", name, i+1, samples, x.stszCount)
		}
		if err := checkChunks(parts[i].mdat, x, name, i); err != nil {
			return out, 0, err
		}
	}
	stsd, changed, err := mergeSampleDescriptions(inputs, name)
	if err != nil {
		return out, 0, err
	}
	if changed {
		out.stsd = stsd
		out.hasSTSD = true
	}
	timescale, err := mediaTimescale(inputs, name)
	if err != nil {
		return out, 0, err
	}
	out.mdhdTimescale = timescale
	scaled := make([][][2]uint32, len(inputs))
	for i, x := range inputs {
		for _, e := range x.stts {
			delta, ok := rescaleExact(uint64(e[1]), x.mdhdTimescale, timescale)
			if !ok || delta > math.MaxUint32 {
				return out, 0, mp4Error("unsupported input", "%s file %d timing does not fit in timescale %d", name, i+1, timescale)
			}
			scaled[i] = append(scaled[i], [2]uint32{e[0], uint32(delta)})
		}
	}
	align := first.handler == fourCC("vide") || first.handler == fourCC("soun")
	if align {
		align = false
		for _, runs := range scaled {
			for _, run := range runs {
				if run[0] > 0 && run[1] > 0 {
					align = true
					break
				}
			}
			if align {
				break
			}
		}
	}
	pcm := pcmTrackFrames(inputs, scaled)
	var mediaEnd uint64
	for _, x := range inputs {
		partEnd, err := rescaleUp(x.mdhdDuration, x.mdhdTimescale, timescale)
		if err != nil {
			return out, 0, err
		}
		mediaEnd, err = add(mediaEnd, partEnd)
		if err != nil {
			return out, 0, err
		}
	}
	edits, whole, trackPresented, fills, lengthens, keptSamples, err := mergeEdits(inputs, parts, timescale, movieScale, mediaEnd, starts, continues, align, pcm)
	if err != nil {
		return out, 0, err
	}
	for i, x := range inputs {
		if i < len(keptSamples) && keptSamples[i] < x.stszCount || i < len(fills) && fills[i] != 0 || i < len(lengthens) && lengthens[i] != 0 {
			out.tkhdDuration = trackPresented
			break
		}
	}
	tableInputs := make([]*partTrack, len(inputs))
	tableScaled := make([][][2]uint32, len(inputs))
	for i, x := range inputs {
		tableInputs[i] = x
		tableScaled[i] = scaled[i]
		if i < len(keptSamples) && keptSamples[i] < x.stszCount {
			var err error
			tableInputs[i], err = headPartTrack(x, keptSamples[i])
			if err != nil {
				return out, 0, err
			}
			tableScaled[i] = trimSTTS(scaled[i], keptSamples[i])
		}
	}
	constant := true
	for _, x := range tableInputs {
		if x.stszSampleSize == 0 || x.stszSampleSize != first.stszSampleSize {
			constant = false
		}
	}
	if constant {
		out.stszSampleSize = first.stszSampleSize
	}
	anyCTTS, anySTSS, anySTPS, anySDTP := false, false, false, false
	cttsVersion := uint8(0)
	for _, x := range tableInputs {
		anyCTTS = anyCTTS || x.hasCTTS
		anySTSS = anySTSS || x.hasSTSS
		anySTPS = anySTPS || x.hasSTPS
		anySDTP = anySDTP || x.hasSDTP
		if x.hasCTTS && x.cttsVersion > cttsVersion {
			cttsVersion = x.cttsVersion
		}
	}
	out.hasSDTP = anySDTP
	var ctts []cttsEntry
	var stss, stps []uint32
	var sampleOff, chunkOff uint32
	for i, x := range tableInputs {
		runs := tableScaled[i]
		if i < len(lengthens) && lengthens[i] != 0 {
			runs, err = lengthenLastRun(runs, lengthens[i])
			if err != nil {
				return out, 0, err
			}
		}
		for _, e := range runs {
			out.stts = appendRun(out.stts, e)
		}
		md, err := rescaleUp(x.mdhdDuration, x.mdhdTimescale, timescale)
		if err != nil {
			return out, 0, err
		}
		out.mdhdDuration, err = add(out.mdhdDuration, md)
		if err != nil {
			return out, 0, err
		}
		if i < len(lengthens) && lengthens[i] != 0 {
			out.mdhdDuration, err = add(out.mdhdDuration, uint64(lengthens[i]))
			if err != nil {
				return out, 0, err
			}
		}
		if !constant {
			if x.stszSampleSize != 0 {
				for k := uint32(0); k < x.stszCount; k++ {
					out.stsz = append(out.stsz, x.stszSampleSize)
				}
			} else {
				out.stsz = append(out.stsz, x.stsz...)
			}
		}
		if anyCTTS {
			left := x.stszCount
			if x.hasCTTS {
				for _, c := range x.ctts {
					n := c.sampleCount
					if n > left {
						n = left
					}
					if n == 0 {
						continue
					}
					absolute := uint64(c.sampleOffset)
					if c.sampleOffset < 0 {
						absolute = uint64(-int64(c.sampleOffset))
					}
					v, ok := rescaleExact(absolute, x.mdhdTimescale, timescale)
					if !ok || v > math.MaxInt32+uint64(1) {
						return out, 0, mp4Error("unsupported input", "%s composition offset does not fit 32 bits", name)
					}
					offset := int32(v)
					if c.sampleOffset < 0 {
						offset = -offset
					}
					ctts = appendCTTS(ctts, cttsEntry{n, offset})
					left -= n
				}
			}
			if left > 0 {
				ctts = appendCTTS(ctts, cttsEntry{left, 0})
			}
		}
		if anySTSS {
			if x.hasSTSS {
				for _, v := range x.stss {
					if math.MaxUint32-v < sampleOff {
						return out, 0, mp4Error("invalid data", "sample number overflows")
					}
					stss = append(stss, v+sampleOff)
				}
			} else {
				for k := uint32(0); k < x.stszCount; k++ {
					stss = append(stss, sampleOff+k+1)
				}
			}
		}
		if anySTPS {
			for _, v := range x.stps {
				if math.MaxUint32-v < sampleOff {
					return out, 0, mp4Error("invalid data", "sample number overflows")
				}
				stps = append(stps, v+sampleOff)
			}
		}
		for _, c := range x.stsc {
			if math.MaxUint32-c[0] < chunkOff {
				return out, 0, mp4Error("invalid data", "chunk number overflows")
			}
			c[0] += chunkOff
			out.stsc = append(out.stsc, c)
		}
		out.stco = append(out.stco, x.stco...)
		if anySDTP {
			if x.hasSDTP {
				entries := x.sdtp
				if uint64(len(entries)) > uint64(x.stszCount) {
					entries = entries[:x.stszCount]
				}
				out.sdtp = append(out.sdtp, entries...)
			}
			for uint64(len(out.sdtp)) < uint64(sampleOff)+uint64(x.stszCount) {
				out.sdtp = append(out.sdtp, 0)
			}
		}
		if math.MaxUint32-sampleOff < x.stszCount {
			return out, 0, mp4Error("invalid data", "track has more than 2^32 samples")
		}
		sampleOff += x.stszCount
		if uint64(chunkOff)+uint64(len(x.stco)) > math.MaxUint32 {
			return out, 0, mp4Error("invalid data", "track has more than 2^32 chunks")
		}
		chunkOff += uint32(len(x.stco))
		if i < len(fills) && fills[i] != 0 {
			count := fills[i]
			if pcm == nil || math.MaxUint32-sampleOff < count || uint64(chunkOff)+1 > math.MaxUint32 {
				return out, 0, mp4Error("invalid data", "%s has more than 2^32 samples or chunks with PCM silence", name)
			}
			out.stts = appendRun(out.stts, [2]uint32{count, 1})
			out.mdhdDuration, err = add(out.mdhdDuration, uint64(count))
			if err != nil {
				return out, 0, err
			}
			if !constant {
				for n := uint32(0); n < count; n++ {
					out.stsz = append(out.stsz, pcm.frame)
				}
			}
			if anySTSS {
				for n := uint32(0); n < count; n++ {
					stss = append(stss, sampleOff+1+n)
				}
			}
			if anySDTP {
				for n := uint32(0); n < count; n++ {
					out.sdtp = append(out.sdtp, 0)
				}
			}
			description := uint32(1)
			if len(x.stsc) > 0 {
				description = x.stsc[len(x.stsc)-1][2]
			}
			firstChunk := uint64(chunkOff) + 1
			if firstChunk > math.MaxUint32 {
				return out, 0, mp4Error("invalid data", "%s has more than 2^32 chunks with PCM silence", name)
			}
			out.stsc = append(out.stsc, [3]uint32{uint32(firstChunk), count, description})
			fillerBytes := uint64(count) * uint64(pcm.frame)
			if pcm.frame != 0 && fillerBytes/uint64(pcm.frame) != uint64(count) {
				return out, 0, mp4Error("invalid data", "%s PCM silence size overflows", name)
			}
			fillerOffset := d.mdatOffset
			for _, f := range d.mdatFiller {
				fillerOffset, err = add(fillerOffset, f.length)
				if err != nil {
					return out, 0, err
				}
			}
			out.stco = append(out.stco, fillerOffset)
			if len(d.mdatFiller) > 0 && d.mdatFiller[len(d.mdatFiller)-1].value == pcm.byte {
				d.mdatFiller[len(d.mdatFiller)-1].length, err = add(d.mdatFiller[len(d.mdatFiller)-1].length, fillerBytes)
				if err != nil {
					return out, 0, err
				}
			} else {
				d.mdatFiller = append(d.mdatFiller, fill{length: fillerBytes, value: pcm.byte})
			}
			sampleOff += count
			chunkOff++
		}
	}
	out.stszCount = sampleOff
	out.hasCTTS = anyCTTS
	out.cttsVersion = cttsVersion
	out.ctts = ctts
	out.hasSTSS = anySTSS
	out.stss = stss
	out.hasSTPS = anySTPS
	out.stps = stps
	if first.hasCSLG {
		bounds, err := compositionBounds(out.stts, out.ctts, out.hasCTTS)
		if err != nil {
			return out, 0, err
		}
		out.cslg = &bounds
	}
	groups, compact, err := mergeSampleGroups(tableInputs, fills, name)
	if err != nil {
		return out, 0, err
	}
	out.sampleGroups = groups
	out.compactGroups = compact
	if first.hasElst || !whole {
		out.elst = edits
		out.insertEdts = !first.hasEdts
	}
	out.tkhdDuration = trackPresented
	return out, trackPresented, nil
}

func mergeTimecodeTrack(parts []partDesc, inputs []*partTrack, movieScale uint32, out mergedTrack) (mergedTrack, uint64, error) {
	first := inputs[0]
	for i, x := range inputs {
		if !x.skip {
			return out, 0, mp4Error("unsupported input", "timecode track differs in file %d", i+1)
		}
	}
	out.mdhdTimescale = first.mdhdTimescale
	for i, x := range inputs {
		v, err := rescaleUp(x.mdhdDuration, x.mdhdTimescale, first.mdhdTimescale)
		if err != nil {
			return out, 0, err
		}
		out.mdhdDuration, err = add(out.mdhdDuration, v)
		if err != nil {
			return out, 0, err
		}
		_ = i
	}
	out.stts = append(out.stts, first.stts...)
	out.ctts = append(out.ctts, first.ctts...)
	out.hasCTTS = first.hasCTTS
	out.cttsVersion = first.cttsVersion
	out.stss = append(out.stss, first.stss...)
	out.hasSTSS = first.hasSTSS
	out.stps = append(out.stps, first.stps...)
	out.hasSTPS = first.hasSTPS
	out.stszSampleSize = first.stszSampleSize
	out.stszCount = first.stszCount
	out.stsz = append(out.stsz, first.stsz...)
	out.stsc = append(out.stsc, first.stsc...)
	out.stco = append(out.stco, first.stco...)
	if first.hasSDTP {
		out.sdtp = append(out.sdtp, first.sdtp...)
	}
	out.sampleGroups = append(out.sampleGroups, first.sbgp...)
	if first.hasElst {
		out.elst = append(out.elst, first.elst...)
		for i := 1; i < len(inputs); i++ {
			var presented uint64
			if inputs[i].hasElst {
				for _, e := range inputs[i].elst {
					if e.mediaTime < 0 {
						continue
					}
					v, err := rescaleUp(e.segmentDuration, parts[i].mvhdTimescale, movieScale)
					if err != nil {
						return out, 0, err
					}
					presented, err = add(presented, v)
					if err != nil {
						return out, 0, err
					}
				}
			} else {
				v, err := mediaDuration(inputs[i])
				if err != nil {
					return out, 0, err
				}
				presented, err = rescaleUp(v, inputs[i].mdhdTimescale, movieScale)
				if err != nil {
					return out, 0, err
				}
			}
			for j := len(out.elst) - 1; j >= 0; j-- {
				if out.elst[j].mediaTime == -1 {
					continue
				}
				var err error
				out.elst[j].segmentDuration, err = add(out.elst[j].segmentDuration, presented)
				if err != nil {
					return out, 0, err
				}
				break
			}
		}
	}
	return out, 0, nil
}

func appendRun(dst [][2]uint32, e [2]uint32) [][2]uint32 {
	if e[0] == 0 {
		return dst
	}
	if len(dst) > 0 && dst[len(dst)-1][1] == e[1] && math.MaxUint32-dst[len(dst)-1][0] >= e[0] {
		dst[len(dst)-1][0] += e[0]
		return dst
	}
	return append(dst, e)
}
func appendCTTS(dst []cttsEntry, e cttsEntry) []cttsEntry {
	if e.sampleCount == 0 {
		return dst
	}
	if len(dst) > 0 && dst[len(dst)-1].sampleOffset == e.sampleOffset && math.MaxUint32-dst[len(dst)-1].sampleCount >= e.sampleCount {
		dst[len(dst)-1].sampleCount += e.sampleCount
		return dst
	}
	return append(dst, e)
}

func checkChunks(mdat [2]uint64, x *partTrack, name string, i int) error {
	bad := func(s string) error { return mp4Error("invalid data", "%s file %d stsc %s", name, i+1, s) }
	chunks := len(x.stco)
	descriptions := uint32(0)
	if len(x.stsd) >= 8 {
		descriptions = binary.BigEndian.Uint32(x.stsd[4:8])
	}
	if len(x.stsc) == 0 && chunks > 0 || len(x.stsc) > 0 && x.stsc[0][0] != 1 {
		return bad("does not start at chunk 1")
	}
	var sampleCount uint64
	for k, e := range x.stsc {
		first := uint64(e[0])
		next := uint64(chunks + 1)
		if k+1 < len(x.stsc) {
			next = uint64(x.stsc[k+1][0])
		}
		if first > uint64(chunks) || next <= first {
			return bad("has invalid chunk range")
		}
		if e[2] == 0 || e[2] > descriptions {
			return bad("references a missing sample description")
		}
		sampleCount += (next - first) * uint64(e[1])
	}
	if sampleCount != uint64(x.stszCount) {
		return bad(fmt.Sprintf("maps %d samples, stsz has %d", sampleCount, x.stszCount))
	}
	if x.stszSampleSize == 0 && len(x.stsz) != int(x.stszCount) {
		return bad("has incomplete sample sizes")
	}
	end := mdat[0] + mdat[1]
	sample, run := 0, 0
	for k, off := range x.stco {
		for run+1 < len(x.stsc) && int(x.stsc[run+1][0]) <= k+1 {
			run++
		}
		count := int(x.stsc[run][1])
		if sample+count > int(x.stszCount) {
			return bad("chunk overruns samples")
		}
		var size uint64
		if x.stszSampleSize != 0 {
			size = uint64(x.stszSampleSize) * uint64(count)
		} else {
			for _, v := range x.stsz[sample : sample+count] {
				size += uint64(v)
			}
		}
		sample += count
		if off < mdat[0] || off > end || size > end-off {
			return mp4Error("unsupported input", "%s file %d has samples outside its mdat box", name, i+1)
		}
	}
	return nil
}

func mergeSampleDescriptions(tracks []*partTrack, name string) ([]byte, bool, error) {
	first := tracks[0]
	configs := make([]*decoderConfig, len(first.sampleEntries))
	for i, e := range first.sampleEntries {
		if e.config != nil {
			configs[i] = cloneDecoderConfig(e.config)
		}
	}
	differs := func(i int) error {
		return mp4Error("unsupported input", "%s has a different sample description in file %d", name, i+1)
	}
	for i, x := range tracks[1:] {
		if len(x.sampleEntries) != len(first.sampleEntries) {
			return nil, false, differs(i + 1)
		}
		for j := range first.sampleEntries {
			a, b := first.sampleEntries[j], x.sampleEntries[j]
			if a.format != b.format || !bytesEqual(a.comparable, b.comparable) {
				return nil, false, differs(i + 1)
			}
			m, c := configs[j], b.config
			if m == nil && c == nil {
				continue
			}
			if m == nil || c == nil || m.typ != c.typ || !bytesEqual(m.header, c.header) || !bytesEqual(m.trailing, c.trailing) || len(m.arrays) != len(c.arrays) {
				return nil, false, differs(i + 1)
			}
			pps := uint8(8)
			maxID := uint32(255)
			completeness := uint8(0)
			if m.typ == fourCC("hvcC") {
				pps = 34
				maxID = 63
				completeness = 0x80
			}
			for a := range m.arrays {
				ma, ca := &m.arrays[a], c.arrays[a]
				if ma.typ != ca.typ || (ma.byte|completeness) != (ca.byte|completeness) || ma.typ != pps && !nalListEqual(ma.nalus, ca.nalus) {
					return nil, false, differs(i + 1)
				}
				ma.byte &= ca.byte | ^completeness
				for _, nalu := range ca.nalus {
					if ma.typ != pps || nalContains(ma.nalus, nalu) {
						continue
					}
					id, ok := decoderPPSID(m.typ, nalu)
					if !ok {
						return nil, false, differs(i + 1)
					}
					if id > maxID {
						return nil, false, mp4Error("invalid data", "%s file %d PPS id %d is out of range", name, i+1, id)
					}
					for _, old := range ma.nalus {
						if oldID, ok := decoderPPSID(m.typ, old); ok && oldID == id {
							return nil, false, differs(i + 1)
						}
					}
					ma.nalus = append(ma.nalus, append([]byte(nil), nalu...))
				}
			}
		}
	}
	stsd := append([]byte(nil), first.stsd...)
	changed := false
	for i := len(first.sampleEntries) - 1; i >= 0; i-- {
		entry := first.sampleEntries[i]
		base := first.sampleEntries[i].config
		merged := configs[i]
		if base == nil || merged == nil || decoderConfigEqual(base, merged) {
			continue
		}
		box, ok := decoderConfigBox(merged)
		if !ok {
			return nil, false, mp4Error("unsupported input", "%s has too many decoder parameter sets", name)
		}
		a, b := entry.at+base.at[0], entry.at+base.at[1]
		if a < 0 || b > len(stsd) || a > b {
			return nil, false, mp4Error("invalid data", "sample entry decoder config offset is invalid")
		}
		stsd = append(stsd[:a], append(box, stsd[b:]...)...)
		size := int(binary.BigEndian.Uint32(stsd[entry.at:entry.at+4])) - (base.at[1] - base.at[0]) + len(box)
		if size > math.MaxUint32 {
			return nil, false, mp4Error("unsupported input", "merged sample entry is too large")
		}
		binary.BigEndian.PutUint32(stsd[entry.at:entry.at+4], uint32(size))
		changed = true
	}
	return stsd, changed, nil
}

func cloneDecoderConfig(c *decoderConfig) *decoderConfig {
	if c == nil {
		return nil
	}
	o := *c
	o.header = append([]byte(nil), c.header...)
	o.trailing = append([]byte(nil), c.trailing...)
	o.arrays = make([]nalArray, len(c.arrays))
	for i, a := range c.arrays {
		o.arrays[i] = a
		o.arrays[i].nalus = make([][]byte, len(a.nalus))
		for j, n := range a.nalus {
			o.arrays[i].nalus[j] = append([]byte(nil), n...)
		}
	}
	return &o
}
func decoderConfigEqual(a, b *decoderConfig) bool {
	if a.typ != b.typ || a.at != b.at || !bytesEqual(a.header, b.header) || !bytesEqual(a.trailing, b.trailing) || len(a.arrays) != len(b.arrays) {
		return false
	}
	for i := range a.arrays {
		if a.arrays[i].typ != b.arrays[i].typ || a.arrays[i].byte != b.arrays[i].byte || !nalListEqual(a.arrays[i].nalus, b.arrays[i].nalus) {
			return false
		}
	}
	return true
}
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func nalListEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}
func nalContains(a [][]byte, b []byte) bool {
	for _, x := range a {
		if bytesEqual(x, b) {
			return true
		}
	}
	return false
}

func decoderConfigBox(c *decoderConfig) ([]byte, bool) {
	body := append([]byte(nil), c.header...)
	push := func(nalus [][]byte) bool {
		for _, n := range nalus {
			if len(n) > math.MaxUint16 {
				return false
			}
			body = binary.BigEndian.AppendUint16(body, uint16(len(n)))
			body = append(body, n...)
		}
		return true
	}
	if c.typ == fourCC("avcC") {
		for _, a := range c.arrays {
			limit := 255
			if a.typ == 7 {
				limit = 31
			}
			if len(a.nalus) > limit {
				return nil, false
			}
			body = append(body, a.byte|byte(len(a.nalus)))
			if !push(a.nalus) {
				return nil, false
			}
		}
	} else if c.typ == fourCC("hvcC") {
		if len(c.arrays) > math.MaxUint8 {
			return nil, false
		}
		body = append(body, byte(len(c.arrays)))
		for _, a := range c.arrays {
			body = append(body, a.byte)
			if len(a.nalus) > math.MaxUint16 {
				return nil, false
			}
			body = binary.BigEndian.AppendUint16(body, uint16(len(a.nalus)))
			if !push(a.nalus) {
				return nil, false
			}
		}
	} else {
		return nil, false
	}
	body = append(body, c.trailing...)
	if len(body) > math.MaxUint32-8 {
		return nil, false
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)+8))
	binary.BigEndian.PutUint32(out[4:], c.typ)
	return append(out, body...), true
}

func mergeEdits(tracks []*partTrack, parts []partDesc, mediaScale, movieScale uint32, mediaEnd uint64, starts []uint64, continues []bool, align bool, pcm *pcmFrames) ([]edit, bool, uint64, []uint32, []uint32, []uint32, error) {
	var edits []edit
	var mediaOffset, timeline uint64
	var end *uint64
	fills := make([]uint32, len(tracks))
	lengthens := make([]uint32, len(tracks))
	keptSamples := make([]uint32, len(tracks))
	presentationStarts := make([]uint64, len(tracks))
	for i, track := range tracks {
		keptSamples[i] = track.stszCount
	}
	oneTickSound := allSoundSamplesOneTick(tracks, mediaScale)
	for i, x := range tracks {
		if align && i > 0 && i-1 < len(continues) && !continues[i-1] {
			target := starts[i]
			if timeline > target {
				// A separate recording starts on the anchor track's timeline. If this
				// track runs past that point, discard the tail of its previous
				// presentation instead of moving the new recording later. The mdat
				// bytes stay in place; the sample tables stop referring to the cut tail.
				edits = trimEditsTo(edits, target)
				timeline = target
				end = nil
				if i > 0 && tracks[i-1].handler == fourCC("soun") {
					lead, eligible, leadErr := soundLeadingEdit(tracks[i-1], parts[i-1].mvhdTimescale, movieScale)
					if leadErr != nil {
						return nil, false, 0, nil, nil, nil, leadErr
					}
					if eligible && target > presentationStarts[i-1] && target-presentationStarts[i-1] > lead {
						allowed := target - presentationStarts[i-1] - lead
						cutAt, ok := rescaleNearest(allowed, movieScale, mediaScale)
						if ok {
							kept, keptDuration, dropped, cut := soundCutPrefix(tracks[i-1], mediaScale, cutAt)
							if cut && kept < keptSamples[i-1] {
								keptSamples[i-1] = kept
								if dropped > mediaOffset {
									return nil, false, 0, nil, nil, nil, mp4Error("invalid data", "track %d sample cut exceeds its merged media offset", i)
								}
								mediaOffset -= dropped
								if dropped >= mediaEnd {
									mediaEnd = 0
								} else {
									mediaEnd -= dropped
								}
								keptMovieDuration, ok := rescaleDown(keptDuration, mediaScale, movieScale)
								if !ok {
									return nil, false, 0, nil, nil, nil, mp4Error("unsupported input", "track %d kept sound duration cannot be represented", i)
								}
								cutDuration, err := add(lead, keptMovieDuration)
								if err != nil {
									return nil, false, 0, nil, nil, nil, err
								}
								cutTimeline, err := add(presentationStarts[i-1], cutDuration)
								if err != nil {
									return nil, false, 0, nil, nil, nil, err
								}
								if cutTimeline < timeline {
									edits = trimEditsTo(edits, cutTimeline)
									timeline = cutTimeline
								}
								mediaEnd := mediaOffset
								end = &mediaEnd
							}
						}
					}
				}
			}
			gap := target - timeline
			if gap > 0 {
				filled := false
				fillLength := uint64(0)
				if pcm != nil && i > 0 && end != nil && *end == mediaOffset && len(edits) > 0 && edits[len(edits)-1].mediaTime >= 0 && edits[len(edits)-1].mediaRate == mediaRateOne {
					count, countOK := rescaleNearest(gap, movieScale, mediaScale)
					length, lengthOK := rescaleNearest(count, mediaScale, movieScale)
					if countOK && lengthOK && count > 0 && count <= math.MaxUint32 && fills[i-1] <= math.MaxUint32-uint32(count) {
						var err error
						edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, length)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						mediaOffset, err = add(mediaOffset, count)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						*end, err = add(*end, count)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						mediaEnd, err = add(mediaEnd, count)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						fills[i-1] += uint32(count)
						fillLength = length
						filled = true
					}
				}
				if !filled && !oneTickSound && i > 0 && end != nil && *end == mediaOffset && len(edits) > 0 && edits[len(edits)-1].mediaTime >= 0 && edits[len(edits)-1].mediaRate == mediaRateOne {
					count, countOK := rescaleNearest(gap, movieScale, mediaScale)
					length, lengthOK := rescaleNearest(count, mediaScale, movieScale)
					if countOK && lengthOK && count > 0 && count <= math.MaxUint32 && uint64(lengthens[i-1])+count <= math.MaxUint32 && canLengthenLastSample(tracks[i-1], mediaScale, uint32(count)) {
						var err error
						edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, length)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						mediaOffset, err = add(mediaOffset, count)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						*end, err = add(*end, count)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						mediaEnd, err = add(mediaEnd, count)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
						lengthens[i-1] += uint32(count)
						fillLength = length
						filled = true
					}
				}
				if !filled && !hasMediaEdit(edits) {
					if len(edits) > 0 && edits[len(edits)-1].mediaTime == -1 {
						var err error
						edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, gap)
						if err != nil {
							return nil, false, 0, nil, nil, nil, err
						}
					} else {
						edits = append(edits, edit{segmentDuration: gap, mediaTime: -1, mediaRate: mediaRateOne})
					}
				}
				if filled {
					nextTimeline, err := add(timeline, fillLength)
					if err != nil {
						return nil, false, 0, nil, nil, nil, err
					}
					timeline = nextTimeline
				} else if !hasMediaEdit(edits) {
					timeline = target
					end = nil
				}
			}
		}
		presentationStarts[i] = timeline
		fileEdits := x.elst
		if !x.hasElst {
			mediaDuration, err := mediaDuration(x)
			if err != nil {
				return nil, false, 0, nil, nil, nil, err
			}
			fileEdits = []edit{{segmentDuration: mediaDuration, mediaTime: 0, mediaRate: mediaRateOne}}
		}
		for _, source := range fileEdits {
			copy := source
			var duration uint64
			var err error
			if x.hasElst {
				duration, err = rescaleUp(source.segmentDuration, parts[i].mvhdTimescale, movieScale)
			} else {
				duration, err = rescaleUp(source.segmentDuration, x.mdhdTimescale, movieScale)
			}
			if err != nil {
				return nil, false, 0, nil, nil, nil, err
			}
			copy.segmentDuration = duration
			if source.mediaTime >= 0 {
				m, ok := rescaleExact(uint64(source.mediaTime), x.mdhdTimescale, mediaScale)
				if !ok || m > math.MaxInt64 || mediaOffset > math.MaxInt64-m {
					return nil, false, 0, nil, nil, nil, mp4Error("unsupported input", "track %d edit media time cannot be represented exactly", i+1)
				}
				copy.mediaTime = int64(mediaOffset + m)
			}
			if copy.mediaTime < 0 {
				if copy.mediaTime == -1 && len(edits) > 0 && edits[len(edits)-1].mediaTime == -1 {
					edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, copy.segmentDuration)
					if err != nil {
						return nil, false, 0, nil, nil, nil, err
					}
				} else {
					edits = append(edits, copy)
				}
				end = nil
			} else {
				merge := copy.mediaRate == mediaRateOne && end != nil && absU64(*end, uint64(copy.mediaTime)) < uint64(mediaScale)
				if merge && len(edits) > 0 {
					edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, copy.segmentDuration)
					if err != nil {
						return nil, false, 0, nil, nil, nil, err
					}
				} else {
					edits = append(edits, copy)
				}
				if copy.mediaRate == mediaRateOne {
					ticks, er := rescaleUp(copy.segmentDuration, movieScale, mediaScale)
					if er != nil {
						return nil, false, 0, nil, nil, nil, er
					}
					v, er := add(uint64(copy.mediaTime), ticks)
					if er != nil {
						return nil, false, 0, nil, nil, nil, er
					}
					end = &v
				} else {
					end = nil
				}
			}
			timeline, err = add(timeline, copy.segmentDuration)
			if err != nil {
				return nil, false, 0, nil, nil, nil, err
			}
		}
		md, err := mediaDuration(x)
		if err != nil {
			return nil, false, 0, nil, nil, nil, err
		}
		scaled, err := rescaleUp(md, x.mdhdTimescale, mediaScale)
		if err != nil {
			return nil, false, 0, nil, nil, nil, err
		}
		mediaOffset, err = add(mediaOffset, scaled)
		if err != nil {
			return nil, false, 0, nil, nil, nil, err
		}
	}
	whole := len(edits) == 1 && edits[0].mediaTime == 0 && edits[0].mediaRate == mediaRateOne && end != nil && absU64(*end, mediaEnd) < uint64(mediaScale)
	return edits, whole, timeline, fills, lengthens, keptSamples, nil
}

func rescaleNearest(value uint64, from, to uint32) (uint64, bool) {
	if from == 0 {
		return 0, false
	}
	n := new(big.Int).Mul(new(big.Int).SetUint64(value), new(big.Int).SetUint64(uint64(to)))
	n.Add(n, new(big.Int).SetUint64(uint64(from)/2))
	n.Div(n, new(big.Int).SetUint64(uint64(from)))
	if !n.IsUint64() {
		return 0, false
	}
	return n.Uint64(), true
}

func allSoundSamplesOneTick(tracks []*partTrack, mediaScale uint32) bool {
	if len(tracks) == 0 || tracks[0].handler != fourCC("soun") {
		return false
	}
	any := false
	for _, track := range tracks {
		for _, run := range track.stts {
			if run[0] == 0 {
				continue
			}
			any = true
			delta, ok := rescaleExact(uint64(run[1]), track.mdhdTimescale, mediaScale)
			if !ok || delta != 1 {
				return false
			}
		}
	}
	return any
}

// soundCutPrefix returns the last complete sound sample boundary at or
// before cutAt. Rust uses this for a simple, continuous edit when a recording
// boundary cuts across the previous sound part.
func soundCutPrefix(track *partTrack, mediaScale uint32, cutAt uint64) (uint32, uint64, uint64, bool) {
	if track.handler != fourCC("soun") || track.hasCTTS || len(track.unmerged) != 0 || track.stszCount == 0 {
		return track.stszCount, 0, 0, false
	}
	if _, ok := soundLeadingEditShape(track); !ok {
		return track.stszCount, 0, 0, false
	}
	var elapsed uint64
	var sample uint32
	var kept uint32
	var keptDuration uint64
	for _, run := range track.stts {
		if run[0] == 0 {
			continue
		}
		delta, ok := rescaleExact(uint64(run[1]), track.mdhdTimescale, mediaScale)
		if !ok || delta == 0 {
			return track.stszCount, 0, 0, false
		}
		remaining := uint64(0)
		if cutAt > elapsed {
			remaining = cutAt - elapsed
		}
		count := remaining / delta
		if count > uint64(run[0]) {
			count = uint64(run[0])
		}
		if count > 0 {
			kept = sample + uint32(count)
			keptDuration = elapsed + count*delta
		}
		if count < uint64(run[0]) {
			break
		}
		elapsed += uint64(run[0]) * delta
		sample += run[0]
	}
	if kept == 0 || kept >= track.stszCount {
		return track.stszCount, 0, 0, false
	}
	var total uint64
	for _, run := range track.stts {
		delta, ok := rescaleExact(uint64(run[1]), track.mdhdTimescale, mediaScale)
		if !ok || (run[0] != 0 && delta == 0) || uint64(run[0]) > (math.MaxUint64-total)/max(uint64(delta), 1) {
			return track.stszCount, 0, 0, false
		}
		total += uint64(run[0]) * delta
	}
	if keptDuration > total {
		return track.stszCount, 0, 0, false
	}
	return kept, keptDuration, total - keptDuration, true
}

func soundLeadingEdit(track *partTrack, partScale, movieScale uint32) (uint64, bool, error) {
	if _, ok := soundLeadingEditShape(track); !ok {
		return 0, false, nil
	}
	var lead uint64
	if track.hasElst {
		for _, edit := range track.elst {
			if edit.mediaTime != -1 {
				break
			}
			duration, err := rescaleUp(edit.segmentDuration, partScale, movieScale)
			if err != nil {
				return 0, false, err
			}
			lead, err = add(lead, duration)
			if err != nil {
				return 0, false, err
			}
		}
	}
	return lead, true, nil
}

func soundLeadingEditShape(track *partTrack) (uint64, bool) {
	if !track.hasElst {
		return 0, true
	}
	media := false
	for _, edit := range track.elst {
		if edit.mediaTime == -1 && !media {
			continue
		}
		if media || edit.mediaTime != 0 || edit.mediaRate != mediaRateOne {
			return 0, false
		}
		media = true
	}
	return 0, media
}

func rescaleDown(value uint64, from, to uint32) (uint64, bool) {
	if from == 0 {
		return 0, false
	}
	n := new(big.Int).Mul(new(big.Int).SetUint64(value), new(big.Int).SetUint64(uint64(to)))
	n.Div(n, new(big.Int).SetUint64(uint64(from)))
	if !n.IsUint64() {
		return 0, false
	}
	return n.Uint64(), true
}

func trimSTTS(runs [][2]uint32, kept uint32) [][2]uint32 {
	out := make([][2]uint32, 0, len(runs))
	left := kept
	for _, run := range runs {
		count := run[0]
		if count > left {
			count = left
		}
		if count != 0 {
			out = append(out, [2]uint32{count, run[1]})
			left -= count
		}
		if left == 0 {
			break
		}
	}
	return out
}

// headPartTrack mirrors Rust's head(): keep the first kept samples and
// leave any unreferenced bytes in the original mdat payload.
func headPartTrack(track *partTrack, kept uint32) (*partTrack, error) {
	if kept >= track.stszCount {
		return track, nil
	}
	out := *track
	out.sbgp = append([]sampleToGroup(nil), track.sbgp...)
	out.csgm = append([]compactToGroup(nil), track.csgm...)
	out.stts = trimSTTS(track.stts, kept)
	out.ctts = trimCTTS(track.ctts, kept)
	out.stss = make([]uint32, 0, len(track.stss))
	for _, sample := range track.stss {
		if sample <= kept {
			out.stss = append(out.stss, sample)
		}
	}
	out.stps = make([]uint32, 0, len(track.stps))
	for _, sample := range track.stps {
		if sample <= kept {
			out.stps = append(out.stps, sample)
		}
	}
	if track.stszSampleSize == 0 && int(kept) < len(track.stsz) {
		out.stsz = append([]uint32(nil), track.stsz[:kept]...)
	}
	out.stszCount = kept
	if len(track.sdtp) > 0 {
		if int(kept) < len(track.sdtp) {
			out.sdtp = append([]byte(nil), track.sdtp[:kept]...)
		}
	}
	for i, group := range track.sbgp {
		out.sbgp[i].entries = trimSTTS(group.entries, kept)
	}
	for i, group := range track.csgm {
		left := kept
		patterns := make([]groupPattern, 0, len(group.patterns))
		for _, pattern := range group.patterns {
			count := pattern.sampleCount
			if count > left {
				count = left
			}
			if count > 0 {
				patterns = append(patterns, groupPattern{sampleCount: count, indices: append([]uint32(nil), pattern.indices...)})
				left -= count
			}
			if left == 0 {
				break
			}
		}
		out.csgm[i].patterns = patterns
	}
	remaining := uint64(kept)
	sample, run := uint64(0), 0
	out.stsc = nil
	out.stco = nil
	for chunk, offset := range track.stco {
		if sample >= uint64(kept) {
			break
		}
		for run+1 < len(track.stsc) && track.stsc[run+1][0] <= uint32(chunk+1) {
			run++
		}
		if run >= len(track.stsc) {
			break
		}
		perChunk, description := track.stsc[run][1], track.stsc[run][2]
		n := uint64(perChunk)
		if n > remaining {
			n = remaining
		}
		firstChunk := uint32(chunk + 1)
		if len(out.stsc) == 0 || out.stsc[len(out.stsc)-1][1] != uint32(n) || out.stsc[len(out.stsc)-1][2] != description {
			out.stsc = append(out.stsc, [3]uint32{firstChunk, uint32(n), description})
		}
		out.stco = append(out.stco, offset)
		used := uint64(perChunk)
		if used > remaining {
			used = remaining
		}
		sample += used
		remaining -= used
	}
	var dropped uint64
	left := kept
	for _, entry := range track.stts {
		used := entry[0]
		if used > left {
			used = left
		}
		left -= used
		tail := entry[0] - used
		term := uint64(tail) * uint64(entry[1])
		var err error
		dropped, err = add(dropped, term)
		if err != nil {
			return nil, err
		}
	}
	if dropped > out.mdhdDuration {
		out.mdhdDuration = 0
	} else {
		out.mdhdDuration -= dropped
	}
	return &out, nil
}

func trimCTTS(runs []cttsEntry, kept uint32) []cttsEntry {
	out := make([]cttsEntry, 0, len(runs))
	left := kept
	for _, run := range runs {
		count := run.sampleCount
		if count > left {
			count = left
		}
		if count != 0 {
			out = append(out, cttsEntry{sampleCount: count, sampleOffset: run.sampleOffset})
			left -= count
		}
		if left == 0 {
			break
		}
	}
	return out
}

func hasMediaEdit(edits []edit) bool {
	for _, edit := range edits {
		if edit.mediaTime >= 0 {
			return true
		}
	}
	return false
}

func canLengthenLastSample(track *partTrack, mediaScale uint32, ticks uint32) bool {
	if track.hasCTTS || len(track.unmerged) != 0 || track.stszCount == 0 {
		return false
	}
	if track.hasElst && (len(track.elst) != 1 || track.elst[0].mediaTime != 0 || track.elst[0].mediaRate != mediaRateOne) {
		return false
	}
	for i := len(track.stts) - 1; i >= 0; i-- {
		if track.stts[i][0] == 0 {
			continue
		}
		delta, ok := rescaleExact(uint64(track.stts[i][1]), track.mdhdTimescale, mediaScale)
		return ok && delta <= math.MaxUint32-uint64(ticks)
	}
	return false
}

func lengthenLastRun(runs [][2]uint32, ticks uint32) ([][2]uint32, error) {
	out := append([][2]uint32(nil), runs...)
	for i := len(out) - 1; i >= 0; i-- {
		count, delta := out[i][0], out[i][1]
		if count == 0 {
			continue
		}
		if math.MaxUint32-delta < ticks {
			return nil, mp4Error("unsupported input", "a sample duration doesn't fit 32 bits after filling a gap")
		}
		out = out[:i]
		if count > 1 {
			out = appendRun(out, [2]uint32{count - 1, delta})
		}
		out = appendRun(out, [2]uint32{1, delta + ticks})
		out = append(out, runs[i+1:]...)
		return out, nil
	}
	return out, nil
}

// trimEditsTo keeps the first duration ticks of a presentation timeline.
// It clips the edit that crosses the boundary and drops edits after it.
func trimEditsTo(edits []edit, duration uint64) []edit {
	kept := edits[:0]
	left := duration
	for _, edit := range edits {
		if left == 0 {
			break
		}
		if edit.segmentDuration > left {
			edit.segmentDuration = left
		}
		if edit.segmentDuration != 0 {
			kept = append(kept, edit)
			left -= edit.segmentDuration
		}
	}
	return kept
}

func absU64(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

func compositionBounds(stts [][2]uint32, ctts []cttsEntry, has bool) ([5]int64, error) {
	var out [5]int64
	if !has || len(ctts) == 0 {
		return out, nil
	}
	least, greatest := int64(math.MaxInt64), int64(math.MinInt64)
	start, end := int64(math.MaxInt64), int64(math.MinInt64)
	var dts int64
	si, ci := 0, 0
	var cLeft uint32
	for _, run := range stts {
		for j := uint32(0); j < run[0]; j++ {
			for ci < len(ctts) && cLeft == 0 {
				cLeft = ctts[ci].sampleCount
				if cLeft == 0 {
					ci++
				}
			}
			offset := int32(0)
			if ci < len(ctts) {
				offset = ctts[ci].sampleOffset
				cLeft--
				if cLeft == 0 {
					ci++
				}
			}
			_ = si
			v := int64(offset)
			if v < least {
				least = v
			}
			if v > greatest {
				greatest = v
			}
			if dts+v < start {
				start = dts + v
			}
			if dts+v+int64(run[1]) > end {
				end = dts + v + int64(run[1])
			}
			dts += int64(run[1])
		}
	}
	values := []int64{-least, least, greatest, start, end}
	for i, v := range values {
		if v < 0 && i == 0 {
			v = 0
		}
		out[i] = v
	}
	return out, nil
}

func pushGroup(entries *[][2]uint32, count, index uint32) {
	if count == 0 {
		return
	}
	e := *entries
	if len(e) > 0 && e[len(e)-1][1] == index && math.MaxUint32-e[len(e)-1][0] >= count {
		e[len(e)-1][0] += count
		*entries = e
		return
	}
	*entries = append(e, [2]uint32{count, index})
}
func optionalU32Equal(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func junctions(parts []partDesc) []bool {
	out := make([]bool, max(0, len(parts)-1))
	tables := make([]*segmentTable, len(parts))
	for i := range parts {
		t, err := parseSegmentTable(parts[i].segmentTables, &parts[i])
		if err != nil && len(parts[i].segmentTables) > 0 {
			logWarning("invalid segment table in file %d: %v", i+1, err)
		} else {
			tables[i] = t
		}
	}
	for i := 0; i+1 < len(parts); i++ {
		out[i] = continues(&parts[i], tables[i], &parts[i+1], tables[i+1])
	}
	return out
}
func continues(a *partDesc, ta *segmentTable, b *partDesc, tb *segmentTable) bool {
	if ta != nil && tb != nil {
		x, y := ta.last(), tb.first()
		sa, sb := ta.sources[x.source], tb.sources[y.source]
		if sa.name == sb.name && sa.size == sb.size && frameEndIs(x.srcFirstFrame, x.frameCount, y.srcFirstFrame) {
			return true
		}
	}
	type recording struct {
		creation uint64
		duration uint64
		scale    uint32
		source   *source
		part     *partDesc
	}
	var end, start *recording
	if ta == nil {
		end = &recording{a.mvhdCreation, a.mvhdDuration, a.mvhdTimescale, nil, a}
	} else {
		x := ta.last()
		s := &ta.sources[x.source]
		if frameEndIs(x.srcFirstFrame, x.frameCount, s.frames) {
			end = &recording{creation: s.creation, source: s}
		}
	}
	if tb == nil {
		start = &recording{b.mvhdCreation, b.mvhdDuration, b.mvhdTimescale, nil, b}
	} else {
		x := tb.first()
		if x.srcFirstFrame == 0 {
			s := &tb.sources[x.source]
			start = &recording{creation: s.creation, source: s}
		}
	}
	if end == nil || start == nil {
		return false
	}
	delta := new(big.Int).SetUint64(start.creation)
	delta.Sub(delta, new(big.Int).SetUint64(end.creation))
	if delta.Sign() == 0 {
		if end.part != nil && start.part != nil {
			return !sameRecording(end.part, start.part)
		}
		if end.source != nil && start.source != nil {
			return end.source.name != start.source.name || end.source.size != start.source.size
		}
		if end.part != nil && start.source != nil {
			return end.part.size != start.source.size || presentedFrames(end.part) != start.source.frames
		}
		if end.source != nil && start.part != nil {
			return start.part.size != end.source.size || presentedFrames(start.part) != end.source.frames
		}
		return false
	}
	if end.part != nil {
		return endsNear(delta, uint64(end.scale), end.duration)
	}
	return framesEndNear(delta, end.source.fps[0], end.source.frames, end.source.fps[1])
}

func frameEndIs(first, count, expected uint64) bool {
	return count <= math.MaxUint64-first && first+count == expected
}

func endsNear(delta *big.Int, scale, duration uint64) bool {
	difference := new(big.Int).Mul(new(big.Int).Set(delta), new(big.Int).SetUint64(scale))
	difference.Sub(difference, new(big.Int).SetUint64(duration))
	difference.Abs(difference)
	limit := new(big.Int).SetUint64(scale)
	limit.Lsh(limit, 1)
	return difference.Cmp(limit) <= 0
}

func framesEndNear(delta *big.Int, fpsNumerator, frames, fpsDenominator uint64) bool {
	difference := new(big.Int).Mul(new(big.Int).Set(delta), new(big.Int).SetUint64(fpsNumerator))
	frameDuration := new(big.Int).Mul(new(big.Int).SetUint64(frames), new(big.Int).SetUint64(fpsDenominator))
	difference.Sub(difference, frameDuration)
	difference.Abs(difference)
	limit := new(big.Int).SetUint64(fpsNumerator)
	limit.Lsh(limit, 1)
	return difference.Cmp(limit) <= 0
}

func presentedFrames(p *partDesc) uint64 {
	v := videoFramesOf(p)
	if v == nil {
		return 0
	}
	return uint64(len(v.starts))
}

func sameRecording(a, b *partDesc) bool {
	if a.mvhdDuration != b.mvhdDuration || a.mvhdTimescale != b.mvhdTimescale || len(a.tracks) != len(b.tracks) {
		return false
	}
	for i := range a.tracks {
		x, y := &a.tracks[i], &b.tracks[i]
		if x.mdhdTimescale != y.mdhdTimescale || len(x.stts) != len(y.stts) || x.stszSampleSize != y.stszSampleSize || len(x.stsz) != len(y.stsz) || x.stszCount != y.stszCount {
			return false
		}
		for j := range x.stts {
			if x.stts[j] != y.stts[j] {
				return false
			}
		}
		if x.hasCTTS != y.hasCTTS || x.cttsVersion != y.cttsVersion || len(x.ctts) != len(y.ctts) {
			return false
		}
		for j := range x.ctts {
			if x.ctts[j] != y.ctts[j] {
				return false
			}
		}
		for j := range x.stsz {
			if x.stsz[j] != y.stsz[j] {
				return false
			}
		}
	}
	return true
}

func logWarning(format string, args ...any) { /* wired to the package logger when one is configured */ }

// These types mirror the current upstream mp4-merge description model. The
// parser keeps per-file data separate until merge has checked and combined it.
type edit struct {
	segmentDuration uint64
	mediaTime       int64
	mediaRate       int32
}

const mediaRateOne int32 = 1 << 16

type sampleToGroup struct {
	version               uint8
	groupingType          uint32
	groupingTypeParameter *uint32
	entries               [][2]uint32
}

type compactToGroup struct {
	groupingType uint32
	patterns     []groupPattern
}

type groupPattern struct {
	sampleCount uint32
	indices     []uint32
}

type sampleEntry struct {
	format     uint32
	at         int
	comparable []byte
	config     *decoderConfig
}

type decoderConfig struct {
	typ      uint32
	at       [2]int
	header   []byte
	arrays   []nalArray
	trailing []byte
}

type nalArray struct {
	typ   uint8
	byte  uint8
	nalus [][]byte
}

type partTrack struct {
	handler        uint32
	tkhdDuration   uint64
	hasEdts        bool
	elst           []edit
	hasElst        bool
	mdhdTimescale  uint32
	mdhdDuration   uint64
	stsd           []byte
	sampleEntries  []sampleEntry
	stts           [][2]uint32
	ctts           []cttsEntry
	cttsVersion    uint8
	hasCTTS        bool
	stss           []uint32
	hasSTSS        bool
	stps           []uint32
	hasSTPS        bool
	stszSampleSize uint32
	stszCount      uint32
	stsz           []uint32
	stsc           [][3]uint32
	stco           []uint64
	sdtp           []byte
	hasSDTP        bool
	sbgp           []sampleToGroup
	csgm           []compactToGroup
	sgpd           []sgpd
	hasCSLG        bool
	encrypted      bool
	unmerged       []uint32
	skip           bool
}

type cttsEntry struct {
	sampleCount  uint32
	sampleOffset int32
}

type sgpd struct {
	groupingType uint32
	body         []byte
}

type partDesc struct {
	mvhdTimescale uint32
	mvhdDuration  uint64
	mvhdCreation  uint64
	mvex          bool
	tracks        []partTrack
	mdat          [2]uint64
	mdats         [][2]uint64
	end           uint64
	walkEnd       uint64
	moov          uint32
	size          uint64
	segmentTables [][]byte
	hasSegmentTbl bool
}

type mdatPosition struct {
	fileIndex int
	offset    uint64
	size      uint64
}

type mergedTrack struct {
	tkhdDuration   uint64
	elst           []edit
	insertEdts     bool
	mdhdTimescale  uint32
	mdhdDuration   uint64
	stts           [][2]uint32
	ctts           []cttsEntry
	cttsVersion    uint8
	hasCTTS        bool
	cslg           *[5]int64
	stsz           []uint32
	stco           []uint64
	stss           []uint32
	hasSTSS        bool
	stps           []uint32
	hasSTPS        bool
	sdtp           []byte
	hasSDTP        bool
	sampleGroups   []sampleToGroup
	compactGroups  []compactToGroup
	stszSampleSize uint32
	stszCount      uint32
	stsc           [][3]uint32
	co64Position   *uint64
	stsd           []byte
	hasSTSD        bool
	written        map[uint32]bool
}

type fill struct {
	length uint64
	value  byte
}

type desc struct {
	mdatPositions     []mdatPosition
	parts             []partDesc
	partStarts        []uint64
	continues         []bool
	movieTimescale    uint32
	movieDuration     uint64
	tracks            []mergedTrack
	mdatOffset        uint64
	finalMdatPosition *uint64
	mdatFiller        []fill
}

func boxType(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func mp4Error(kind string, format string, args ...any) error {
	return fmt.Errorf("mp4merge: %s: %s", kind, fmt.Sprintf(format, args...))
}

func add(a, b uint64) (uint64, error) {
	if math.MaxUint64-a < b {
		return 0, mp4Error("invalid data", "duration overflows 64 bits")
	}
	return a + b, nil
}

func mergeSampleGroups(tracks []*partTrack, silence []uint32, name string) ([]sampleToGroup, []compactToGroup, error) {
	if len(tracks) == 0 {
		return nil, nil, nil
	}
	first := tracks[0]
	refused := func(format string, args ...any) error {
		return mp4Error("unsupported input", name+": "+fmt.Sprintf(format, args...))
	}
	invalid := func(format string, args ...any) error {
		return mp4Error("invalid data", name+": "+fmt.Sprintf(format, args...))
	}
	for i, x := range tracks[1:] {
		if groupCompactUnmerged(x) && hasEssentialGroupDescription(x) {
			return nil, nil, refused("file %d maps samples to groups in an unmerged compact mapping and has essential sample groups (sgpd version 3)", i+2)
		}
	}
	var groups []sampleToGroup
	for _, base := range first.sbgp {
		out := sampleToGroup{version: base.version, groupingType: base.groupingType, groupingTypeParameter: base.groupingTypeParameter}
		for i, x := range tracks {
			own, has := groupMapping(x, base.groupingType, base.groupingTypeParameter)
			left := x.stszCount
			grouped := false
			for _, e := range own {
				n := e[0]
				if n > left {
					n = left
				}
				if n > 0 && e[1] != 0 {
					grouped = true
				}
				pushGroup(&out.entries, n, e[1])
				left -= n
			}
			index, ok := defaultGroup(x, base.groupingType)
			if !ok {
				return nil, nil, invalid("the default entry for sample group %q in file %d cannot be read", typeToString(base.groupingType), i+1)
			}
			if left > 0 {
				if index != 0 && !has && groupCompactUnmerged(x) {
					return nil, nil, refused("file %d may map samples to sample group %q in an unmerged compact mapping and its description has a default entry", i+1, typeToString(base.groupingType))
				}
				grouped = grouped || index != 0
				pushGroup(&out.entries, left, index)
			}
			if i < len(silence) && silence[i] > 0 {
				pushGroup(&out.entries, silence[i], 0)
			}
			if grouped && !sameGroupDescription(first, x, base.groupingType) {
				return nil, nil, refused("sample group descriptions of type %q differ in file %d", typeToString(base.groupingType), i+1)
			}
		}
		groups = append(groups, out)
	}
	var compact []compactToGroup
	for _, base := range first.csgm {
		out := compactToGroup{groupingType: base.groupingType}
		for i, x := range tracks {
			own, has := groupPatterns(x, base.groupingType)
			left := x.stszCount
			grouped := false
			for _, p := range own {
				n := p.sampleCount
				if n > left {
					n = left
				}
				if n == 0 {
					continue
				}
				ids := append([]uint32(nil), p.indices...)
				if uint64(len(ids)) > uint64(n) {
					ids = ids[:n]
				}
				if len(ids) == 0 {
					return nil, nil, invalid("compact sample group %q has an empty index pattern", typeToString(base.groupingType))
				}
				for _, id := range ids {
					if id > 15 {
						return nil, nil, refused("file %d maps sample group %q to index %d, which doesn't fit csgm's 4 bits", i+1, typeToString(base.groupingType), id)
					}
					grouped = grouped || id != 0
				}
				out.patterns = append(out.patterns, groupPattern{sampleCount: n, indices: ids})
				left -= n
			}
			index, ok := defaultGroup(x, base.groupingType)
			if !ok {
				return nil, nil, invalid("the default entry for compact sample group %q in file %d cannot be read", typeToString(base.groupingType), i+1)
			}
			if left > 0 {
				if index > 15 {
					return nil, nil, refused("file %d's default index %d for sample group %q doesn't fit csgm's 4 bits", i+1, index, typeToString(base.groupingType))
				}
				if index != 0 && !has && groupCompactUnmerged(x) {
					return nil, nil, refused("file %d may map samples to compact sample group %q in an unmerged compact mapping and its description has a default entry", i+1, typeToString(base.groupingType))
				}
				grouped = grouped || index != 0
				if len(out.patterns) > 0 && len(out.patterns[len(out.patterns)-1].indices) == 1 && out.patterns[len(out.patterns)-1].indices[0] == index {
					out.patterns[len(out.patterns)-1].sampleCount += left
				} else {
					out.patterns = append(out.patterns, groupPattern{sampleCount: left, indices: []uint32{index}})
				}
			}
			if i < len(silence) && silence[i] > 0 {
				out.patterns = append(out.patterns, groupPattern{sampleCount: silence[i], indices: []uint32{0}})
			}
			if grouped && !sameGroupDescription(first, x, base.groupingType) {
				return nil, nil, refused("sample group descriptions of type %q differ in file %d", typeToString(base.groupingType), i+1)
			}
		}
		compact = append(compact, out)
	}
	// Validate groupings present only in later files. The writer preserves the
	// first file's descriptions, so later samples may only use compatible defaults.
	types := allGroupTypes(tracks)
	for _, typ := range types {
		mapped := func(parameter *uint32) bool { return firstMaps(first, typ, parameter) }
		var parameters []*uint32
		for _, x := range tracks {
			for i := range x.sbgp {
				g := &x.sbgp[i]
				if g.groupingType == typ && !mapped(g.groupingTypeParameter) {
					parameters = append(parameters, g.groupingTypeParameter)
				}
			}
			for _, g := range x.csgm {
				if g.groupingType == typ && !mapped(nil) {
					parameters = append(parameters, nil)
				}
			}
		}
		if !mapped(nil) {
			hasType := false
			for _, g := range first.sbgp {
				if g.groupingType == typ {
					hasType = true
					break
				}
			}
			if !hasType {
				parameters = append(parameters, nil)
			}
		}
		sort.SliceStable(parameters, func(i, j int) bool {
			if parameters[i] == nil {
				return parameters[j] != nil
			}
			if parameters[j] == nil {
				return false
			}
			return *parameters[i] < *parameters[j]
		})
		unique := parameters[:0]
		for _, p := range parameters {
			if len(unique) == 0 || !optionalU32Equal(unique[len(unique)-1], p) {
				unique = append(unique, p)
			}
		}
		for _, parameter := range unique {
			for i, x := range tracks[1:] {
				own, has := groupMapping(x, typ, parameter)
				if !has && !groupCompactUnmerged(x) && sameGroupDescription(first, x, typ) {
					continue
				}
				merged, ok := defaultGroup(first, typ)
				if !ok {
					return nil, nil, invalid("the default entry for sample group %q in file 1 cannot be read", typeToString(typ))
				}
				other, grouped := false, false
				left := x.stszCount
				for _, e := range own {
					n := e[0]
					if n > left {
						n = left
					}
					if n > 0 {
						other = other || e[1] != merged
						grouped = grouped || e[1] != 0
					}
					left -= n
				}
				index, ok := defaultGroup(x, typ)
				if !ok {
					return nil, nil, invalid("the default entry for sample group %q in file %d cannot be read", typeToString(typ), i+2)
				}
				if left > 0 {
					other = other || index != merged
					grouped = grouped || index != 0
				}
				if merged != 0 {
					if other || groupCompactUnmerged(x) || !sameGroupDescription(first, x, typ) {
						return nil, nil, refused("file %d samples would use default entry %d of file 1's %q descriptions, but they do not", i+2, merged, typeToString(typ))
					}
				} else if grouped && groupDescriptionVersion(x, typ) == 3 {
					return nil, nil, refused("file %d samples use essential %q groups that file 1 doesn't map", i+2, typeToString(typ))
				}
			}
		}
	}
	return groups, compact, nil
}

func groupMapping(t *partTrack, typ uint32, parameter *uint32) ([][2]uint32, bool) {
	for _, g := range t.sbgp {
		if g.groupingType == typ && optionalU32Equal(g.groupingTypeParameter, parameter) {
			return g.entries, true
		}
	}
	if parameter != nil {
		return nil, false
	}
	for _, g := range t.csgm {
		if g.groupingType != typ {
			continue
		}
		var out [][2]uint32
		left := t.stszCount
		for _, p := range g.patterns {
			n := p.sampleCount
			if n > left {
				n = left
			}
			if len(p.indices) == 0 {
				continue
			}
			for k := uint32(0); k < n; k++ {
				pushGroup(&out, 1, p.indices[k%uint32(len(p.indices))])
			}
			left -= n
		}
		return out, true
	}
	return nil, false
}

func groupPatterns(t *partTrack, typ uint32) ([]groupPattern, bool) {
	for _, g := range t.csgm {
		if g.groupingType == typ {
			return g.patterns, true
		}
	}
	entries, ok := groupMapping(t, typ, nil)
	if !ok {
		return nil, false
	}
	out := make([]groupPattern, 0, len(entries))
	for _, e := range entries {
		out = append(out, groupPattern{sampleCount: e[0], indices: []uint32{e[1]}})
	}
	return out, true
}
func defaultGroup(t *partTrack, typ uint32) (uint32, bool) {
	for _, g := range t.sgpd {
		if g.groupingType == typ {
			return sgpdDefault(g.body)
		}
	}
	return 0, true
}
func sgpdDefault(b []byte) (uint32, bool) {
	if len(b) < 4 {
		return 0, false
	}
	v := b[0]
	if v <= 1 {
		return 0, true
	}
	if v > 3 {
		return 0, false
	}
	field := func(at int) (uint32, bool) {
		if at < 0 || at+4 > len(b) {
			return 0, false
		}
		return binary.BigEndian.Uint32(b[at : at+4]), true
	}
	length, ok := field(8)
	if !ok {
		return 0, false
	}
	def, ok := field(12)
	if !ok {
		return 0, false
	}
	count, ok := field(16)
	if !ok {
		return 0, false
	}
	end := 20
	if length == 0 {
		for i := uint32(0); i < count; i++ {
			n, ok := field(end)
			if !ok || uint64(n) > uint64(len(b)-min(end+4, len(b))) {
				return 0, false
			}
			end += 4 + int(n)
		}
	} else {
		if uint64(count)*uint64(length) > uint64(len(b)-20) {
			return 0, false
		}
		end += int(uint64(count) * uint64(length))
	}
	if def <= count && end == len(b) {
		return def, true
	}
	if v == 2 && len(b) >= 16 {
		size := uint32(0)
		switch string(b[4:8]) {
		case "roll", "prol":
			size = 2
		case "sync":
			size = 1
		}
		if size > 0 {
			old, ok := field(8)
			cnt, cok := field(12)
			if ok && cok && old <= cnt && uint64(cnt)*uint64(size)+16 == uint64(len(b)) {
				return old, true
			}
		}
	}
	if def <= count && end <= len(b) {
		return def, true
	}
	return 0, false
}
func groupDescription(t *partTrack, typ uint32) []byte {
	for _, g := range t.sgpd {
		if g.groupingType == typ {
			return g.body
		}
	}
	return nil
}
func sameGroupDescription(a, b *partTrack, typ uint32) bool {
	return bytesEqual(groupDescription(a, typ), groupDescription(b, typ))
}
func groupDescriptionVersion(t *partTrack, typ uint32) uint8 {
	for _, g := range t.sgpd {
		if g.groupingType == typ && len(g.body) > 0 {
			return g.body[0]
		}
	}
	return 0
}
func groupCompactUnmerged(t *partTrack) bool {
	for _, x := range t.unmerged {
		if x == fourCC("csgp") || x == fourCC("csgm") {
			return true
		}
	}
	return false
}
func hasEssentialGroupDescription(t *partTrack) bool {
	for _, g := range t.sgpd {
		if len(g.body) > 0 && g.body[0] == 3 {
			return true
		}
	}
	return false
}
func allGroupTypes(tracks []*partTrack) []uint32 {
	var out []uint32
	seen := map[uint32]bool{}
	for _, t := range tracks {
		for _, g := range t.sgpd {
			if !seen[g.groupingType] {
				seen[g.groupingType] = true
				out = append(out, g.groupingType)
			}
		}
		for _, g := range t.sbgp {
			if !seen[g.groupingType] {
				seen[g.groupingType] = true
				out = append(out, g.groupingType)
			}
		}
		for _, g := range t.csgm {
			if !seen[g.groupingType] {
				seen[g.groupingType] = true
				out = append(out, g.groupingType)
			}
		}
	}
	return out
}
func firstMaps(t *partTrack, typ uint32, p *uint32) bool {
	for _, g := range t.sbgp {
		if g.groupingType == typ && optionalU32Equal(g.groupingTypeParameter, p) {
			return true
		}
	}
	if p == nil {
		for _, g := range t.csgm {
			if g.groupingType == typ {
				return true
			}
		}
	}
	return false
}

type pcmFrames struct {
	frame   uint32
	byte    byte
	byEntry bool
}

// pcmTrackFrames recognizes the PCM sample entries whose frame size and
// silence byte can be inferred without decoding media. It deliberately
// requires each sample to be exactly one media tick; the merge uses this to
// insert a chunk of silence without changing encoded audio.
func pcmTrackFrames(tracks []*partTrack, scaled [][][2]uint32) *pcmFrames {
	if len(tracks) == 0 || len(scaled) != len(tracks) || tracks[0].handler != fourCC("soun") {
		return nil
	}
	any := false
	for _, runs := range scaled {
		for _, run := range runs {
			if run[0] == 0 {
				continue
			}
			any = true
			if run[1] != 1 {
				return nil
			}
		}
	}
	if !any {
		return nil
	}
	var common *pcmFrames
	for _, track := range tracks {
		if track.hasCTTS || len(track.unmerged) != 0 || track.stszSampleSize == 0 || track.stszSampleSize != tracks[0].stszSampleSize || len(track.sampleEntries) == 0 {
			return nil
		}
		for _, entry := range track.sampleEntries {
			pcm, ok := pcmEntry(track.stsd, entry)
			if !ok || !pcm.byEntry && pcm.frame != track.stszSampleSize {
				return nil
			}
			if common == nil {
				copy := pcm
				common = &copy
			} else if common.frame != pcm.frame || common.byte != pcm.byte || common.byEntry != pcm.byEntry {
				return nil
			}
		}
	}
	if common != nil && !common.byEntry && tracks[0].stszSampleSize != common.frame {
		return nil
	}
	return common
}

func pcmEntry(stsd []byte, entry sampleEntry) (pcmFrames, bool) {
	var out pcmFrames
	if entry.at < 0 || entry.at+8 > len(stsd) {
		return out, false
	}
	box := stsd[entry.at:]
	size := uint64(binary.BigEndian.Uint32(box[:4]))
	if size < 36 || size > uint64(len(box)) {
		return out, false
	}
	box = box[:int(size)]
	format := entry.format
	name := string([]byte{byte(format >> 24), byte(format >> 16), byte(format >> 8), byte(format)})
	if name != "twos" && name != "sowt" && name != "lpcm" && name != "NONE" && name != "raw " && name != "in24" && name != "in32" && name != "fl32" && name != "fl64" && name != "ipcm" && name != "fpcm" {
		return out, false
	}
	version := uint32(binary.BigEndian.Uint16(box[16:18]))
	channels := uint32(binary.BigEndian.Uint16(box[24:26]))
	entryBits := uint32(binary.BigEndian.Uint16(box[26:28]))
	bits := entryBits
	var packetFrames, packetBytes, flags *uint32
	boxesAt := 36
	switch version {
	case 0:
	case 1:
		if len(stsd) == 0 || stsd[0] != 0 || len(box) < 52 {
			return out, false
		}
		a, b := binary.BigEndian.Uint32(box[36:40]), binary.BigEndian.Uint32(box[44:48])
		packetFrames, packetBytes = &a, &b
		boxesAt = 52
	case 2:
		if len(stsd) == 0 || stsd[0] != 0 || len(box) < 72 {
			return out, false
		}
		channels = binary.BigEndian.Uint32(box[48:52])
		bits = binary.BigEndian.Uint32(box[56:60])
		entryBits = bits
		f, bf, bp := binary.BigEndian.Uint32(box[60:64]), binary.BigEndian.Uint32(box[64:68]), binary.BigEndian.Uint32(box[68:72])
		flags, packetBytes, packetFrames = &f, &bf, &bp
		boxesAt = 72
	default:
		return out, false
	}
	switch format {
	case fourCC("twos"), fourCC("sowt"), fourCC("lpcm"):
		if flags == nil && bits != 8 && bits != 24 && bits != 32 {
			bits = 16
		}
	case fourCC("NONE"):
		if bits != 16 {
			bits = 8
		}
	case fourCC("raw "):
		bits = 8
		out.byte = 0x80
	case fourCC("in24"):
		bits = 24
	case fourCC("in32"), fourCC("fl32"):
		bits = 32
	case fourCC("fl64"):
		bits = 64
	case fourCC("ipcm"), fourCC("fpcm"):
		var config []byte
		for at := boxesAt; at+8 <= len(box); {
			childSize := uint64(binary.BigEndian.Uint32(box[at : at+4]))
			if childSize < 8 || childSize > uint64(len(box)-at) {
				return out, false
			}
			if binary.BigEndian.Uint32(box[at+4:at+8]) == fourCC("pcmC") {
				config = box[at+8 : at+int(childSize)]
				break
			}
			at += int(childSize)
		}
		if len(config) < 6 || config[0] != 0 || config[1] != 0 || config[2] != 0 || config[3] != 0 {
			return out, false
		}
		flag := uint32(config[4])
		flags = &flag
		bits = uint32(config[5])
		if format == fourCC("ipcm") && bits != 16 && bits != 24 && bits != 32 || format == fourCC("fpcm") && bits != 32 && bits != 64 {
			return out, false
		}
	default:
		return out, false
	}
	if channels == 0 || bits == 0 || bits%8 != 0 || channels > math.MaxUint32/(bits/8) {
		return out, false
	}
	out.frame = channels * (bits / 8)
	if packetFrames != nil && *packetFrames != 1 || packetBytes != nil && *packetBytes != out.frame {
		return pcmFrames{}, false
	}
	var ffmpegBits *uint32
	switch format {
	case fourCC("twos"), fourCC("sowt"):
		v := entryBits
		if v != 8 && v != 24 && v != 32 {
			v = 16
		}
		ffmpegBits = &v
	case fourCC("lpcm"):
		if flags == nil {
			v := entryBits
			if v != 8 && v != 24 && v != 32 {
				v = 16
			}
			ffmpegBits = &v
		} else {
			flag := *flags
			bytesPerSample := (entryBits + 7) / 8
			if flag&1 != 0 {
				if entryBits == 32 || entryBits == 64 {
					v := entryBits
					ffmpegBits = &v
				}
			} else if flag&4 != 0 {
				if (bytesPerSample >= 1 && bytesPerSample <= 4 || bytesPerSample == 8) && entryBits <= 64 {
					v := bytesPerSample * 8
					ffmpegBits = &v
				}
			} else if bytesPerSample >= 1 && bytesPerSample <= 4 && entryBits <= 64 {
				v := bytesPerSample * 8
				ffmpegBits = &v
			}
		}
	case fourCC("NONE"), fourCC("raw "):
		v := uint32(8)
		if format == fourCC("NONE") && entryBits == 16 {
			v = 16
		}
		ffmpegBits = &v
	case fourCC("ipcm"), fourCC("fpcm"):
	case fourCC("in24"):
		v := uint32(24)
		ffmpegBits = &v
	case fourCC("in32"), fourCC("fl32"):
		v := uint32(32)
		ffmpegBits = &v
	case fourCC("fl64"):
		v := uint32(64)
		ffmpegBits = &v
	}
	if ffmpegBits != nil && *ffmpegBits != bits {
		return pcmFrames{}, false
	}
	out.byEntry = ffmpegBits != nil && out.frame <= math.MaxInt32
	if format == fourCC("NONE") && bits == 8 || format == fourCC("lpcm") && flags != nil && *flags&1 == 0 && *flags&4 == 0 && bits == 8 {
		out.byte = 0x80
	}
	if format == fourCC("lpcm") && flags != nil && *flags&1 == 0 && *flags&4 == 0 && bits != 8 {
		return pcmFrames{}, false
	}
	return out, true
}
