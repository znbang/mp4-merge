package mp4merge

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"sort"
)

// big.Int helpers mirroring Rust's u128/i128 arithmetic cleanly without overflow.
var (
	biZero = big.NewInt(0)
	biOne  = big.NewInt(1)
)

func biU64(v uint64) *big.Int      { return new(big.Int).SetUint64(v) }
func biU32(v uint32) *big.Int      { return new(big.Int).SetUint64(uint64(v)) }
func biI64(v int64) *big.Int       { return big.NewInt(v) }
func biClone(a *big.Int) *big.Int  { return new(big.Int).Set(a) }
func biAdd(a, b *big.Int) *big.Int { return new(big.Int).Add(a, b) }
func biMul(a, b *big.Int) *big.Int { return new(big.Int).Mul(a, b) }
func biDiv(a, b *big.Int) *big.Int { return new(big.Int).Quo(a, b) }
func biMulU32(a *big.Int, b uint32) *big.Int {
	return new(big.Int).Mul(a, biU32(b))
}
func biMulU64(a *big.Int, b uint64) *big.Int {
	return new(big.Int).Mul(a, biU64(b))
}
func biSubSat(a, b *big.Int) *big.Int {
	if a.Cmp(b) <= 0 {
		return biU64(0)
	}
	return new(big.Int).Sub(a, b)
}
func biAbsDiff(a, b *big.Int) *big.Int {
	d := new(big.Int).Sub(a, b)
	return d.Abs(d)
}
func biMin(a, b *big.Int) *big.Int {
	if a.Cmp(b) <= 0 {
		return biClone(a)
	}
	return biClone(b)
}
func biMax(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return biClone(a)
	}
	return biClone(b)
}
func biToU64(a *big.Int) (uint64, bool) {
	if a.Sign() < 0 || !a.IsUint64() {
		return 0, false
	}
	return a.Uint64(), true
}
func biToU64Sat(a *big.Int) uint64 {
	if a.Sign() < 0 {
		return 0
	}
	if !a.IsUint64() {
		return math.MaxUint64
	}
	return a.Uint64()
}
func biToU32(a *big.Int) (uint32, bool) {
	u, ok := biToU64(a)
	if !ok || u > math.MaxUint32 {
		return 0, false
	}
	return uint32(u), true
}
func biToU32Sat(a *big.Int) uint32 {
	u, ok := biToU64(a)
	if !ok || u > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(u)
}

type movieInfo struct {
	parts     []partDesc
	timescale uint32
	continues []bool
	ends      []uint64
}

type trackStarts struct {
	ticks     []uint64
	exact     []*big.Int
	timescale uint32
}

type trackRoleKind int

const (
	roleAnchor trackRoleKind = iota
	roleAligned
	roleContinuous
)

type trackRole struct {
	kind   trackRoleKind
	starts *trackStarts
}

type mdatFillerState struct {
	at   uint64
	runs []fill
}

func (f *mdatFillerState) add(length uint64, b byte) (uint64, error) {
	at := f.at
	next, err := add(f.at, length)
	if err != nil {
		return 0, err
	}
	f.at = next
	if len(f.runs) > 0 && f.runs[len(f.runs)-1].value == b {
		f.runs[len(f.runs)-1].length, err = add(f.runs[len(f.runs)-1].length, length)
		if err != nil {
			return 0, err
		}
	} else {
		f.runs = append(f.runs, fill{length: length, value: b})
	}
	return at, nil
}

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
	for i := range d.parts {
		p := &d.parts[i]
		if p.mvex && allSamplesZero(p) {
			return mp4Error("unsupported input", "file %d is fragmented MP4 without samples in its sample tables", i+1)
		}
	}

	ends := make([]uint64, len(d.parts))
	for i := range d.parts {
		end, err := partMovieEnd(&d.parts[i], first.mvhdTimescale)
		if err != nil {
			return err
		}
		ends[i] = end
	}
	movie := &movieInfo{
		parts:     d.parts,
		timescale: first.mvhdTimescale,
		continues: junctions(d.parts),
		ends:      ends,
	}
	filler := &mdatFillerState{at: d.mdatOffset}
	tracks := make([]*mergedTrack, len(first.tracks))
	var presents uint64

	anchor := -1
	for t := range first.tracks {
		if anchors(d.parts, t) {
			anchor = t
			break
		}
	}

	var starts trackStarts
	if anchor >= 0 {
		merged, st, end, err := mergeTrack(movie, anchor, trackRole{kind: roleAnchor}, filler)
		if err != nil {
			return err
		}
		tracks[anchor] = &merged
		presents = end
		starts = st
	} else {
		ticks := make([]uint64, len(d.parts))
		exact := make([]*big.Int, len(d.parts))
		var at uint64
		for i, end := range movie.ends {
			ticks[i] = at
			exact[i] = biU64(at)
			var err error
			at, err = add(at, end)
			if err != nil {
				return err
			}
		}
		starts = trackStarts{ticks: ticks, exact: exact, timescale: 1}
	}

	for t := range tracks {
		if tracks[t] != nil {
			continue
		}
		merged, _, end, err := mergeTrack(movie, t, trackRole{kind: roleAligned, starts: &starts}, filler)
		if err != nil {
			return err
		}
		tracks[t] = &merged
		if end > presents {
			presents = end
		}
	}

	var duration uint64
	for i, end := range movie.ends {
		if i < len(movie.continues) && !movie.continues[i] {
			duration = starts.ticks[i+1]
		} else {
			var err error
			duration, err = add(duration, end)
			if err != nil {
				return err
			}
		}
	}
	if presents > duration {
		duration = presents
	}

	d.movieTimescale = movie.timescale
	d.movieDuration = duration
	d.continues = movie.continues
	d.partStarts = starts.ticks
	d.tracks = make([]mergedTrack, len(tracks))
	for i, tr := range tracks {
		d.tracks[i] = *tr
	}
	d.mdatFiller = filler.runs
	return nil
}

func anchors(parts []partDesc, t int) bool {
	isAnchor := func(u int) bool {
		if parts[0].tracks[u].skip {
			return false
		}
		for i := range parts {
			x := &parts[i].tracks[u]
			if x.handler == fourCC("vide") && !x.skip {
				for _, e := range x.stts {
					if e[0] > 0 && e[1] > 0 {
						return true
					}
				}
			}
		}
		return false
	}
	if !isAnchor(t) {
		return false
	}
	for u := 0; u < t; u++ {
		if isAnchor(u) {
			return false
		}
	}
	return true
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

func compositionEnd(x *partTrack) (*big.Int, error) {
	if !x.hasCTTS {
		n, err := mediaDuration(x)
		if err != nil {
			return nil, err
		}
		return biU64(n), nil
	}
	dts := biI64(0)
	end := biI64(0)
	cttsIndex := 0
	var cttsLeft uint32
	var offset int32
	for _, run := range x.stts {
		d := biU32(run[1])
		for j := uint32(0); j < run[0]; j++ {
			for cttsLeft == 0 && cttsIndex < len(x.ctts) {
				offset = x.ctts[cttsIndex].sampleOffset
				cttsLeft = x.ctts[cttsIndex].sampleCount
				cttsIndex++
			}
			var o int32
			if cttsLeft > 0 {
				o = offset
				cttsLeft--
			}
			v := new(big.Int).Add(dts, biI64(int64(o)))
			v.Add(v, d)
			if v.Cmp(end) > 0 {
				end = v
			}
			dts.Add(dts, d)
		}
	}
	return end, nil
}

func presentedDuration(x *partTrack, mediaScale, movieScale uint32) (uint64, error) {
	endBig, err := compositionEnd(x)
	if err != nil {
		return 0, err
	}
	media, ok := biToU64(endBig)
	if !ok || media == 0 {
		return 0, nil
	}
	if !x.hasElst {
		return rescaleUp(media, mediaScale, movieScale)
	}
	var total uint64
	for _, e := range x.elst {
		presents := e.segmentDuration
		if e.mediaTime >= 0 && e.mediaRate == mediaRateOne {
			rem := uint64(0)
			if media > uint64(e.mediaTime) {
				rem = media - uint64(e.mediaTime)
			}
			mDur, er := rescaleUp(rem, mediaScale, movieScale)
			if er != nil {
				return 0, er
			}
			if mDur < presents {
				presents = mDur
			}
		}
		total, err = add(total, presents)
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

func mergeTrack(movie *movieInfo, index int, role trackRole, filler *mdatFillerState) (mergedTrack, trackStarts, uint64, error) {
	parts := movie.parts
	movieScale := movie.timescale
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
			return out, trackStarts{}, 0, err
		}
		out.tkhdDuration, err = add(out.tkhdDuration, duration)
		if err != nil {
			return out, trackStarts{}, 0, err
		}
	}

	if first.skip {
		for i, x := range inputs {
			if !x.skip {
				return out, trackStarts{}, 0, mp4Error("unsupported input", "%s has another sample description in file %d than in file 1: file 1 is timecode", name, i+1)
			}
		}
		if !withinMdat(parts[0].mdat, first) {
			return out, trackStarts{}, 0, mp4Error("unsupported input", "%s of file 1 has samples outside its mdat box", name)
		}
		merged, end, err := mergeTimecodeTrack(parts, inputs, movieScale, out)
		return merged, trackStarts{timescale: 1}, end, err
	}

	if first.encrypted {
		return out, trackStarts{}, 0, mp4Error("unsupported input", "%s is encrypted", name)
	}
	for i, x := range inputs {
		var samples uint64
		for _, e := range x.stts {
			samples += uint64(e[0])
		}
		if samples != uint64(x.stszCount) {
			return out, trackStarts{}, 0, mp4Error("invalid data", "%s file %d has %d stts samples and %d stsz samples", name, i+1, samples, x.stszCount)
		}
		if err := checkChunks(parts[i].mdat, x, name, i); err != nil {
			return out, trackStarts{}, 0, err
		}
		for _, typ := range x.unmerged {
			logWarning("%s: the %q box of file %d isn't merged, so the merged file keeps the first file's", name, typeToString(typ), i+1)
		}
	}

	stsd, changed, err := mergeSampleDescriptions(inputs, name)
	if err != nil {
		return out, trackStarts{}, 0, err
	}
	if changed {
		out.stsd = stsd
		out.hasSTSD = true
	}

	timescale, err := mediaTimescale(inputs, name)
	if err != nil {
		return out, trackStarts{}, 0, err
	}
	out.mdhdTimescale = timescale

	scaled := make([][][2]uint32, len(inputs))
	for i, x := range inputs {
		for _, e := range x.stts {
			delta, ok := rescaleExact(uint64(e[1]), x.mdhdTimescale, timescale)
			if !ok || delta > math.MaxUint32 {
				return out, trackStarts{}, 0, mp4Error("unsupported input", "%s file %d timing does not fit in timescale %d", name, i+1, timescale)
			}
			scaled[i] = append(scaled[i], [2]uint32{e[0], uint32(delta)})
		}
	}

	pictureOrSound := false
	if first.handler == fourCC("vide") || first.handler == fourCC("soun") {
		for _, runs := range scaled {
			for _, run := range runs {
				if run[0] > 0 && run[1] > 0 {
					pictureOrSound = true
					break
				}
			}
			if pictureOrSound {
				break
			}
		}
	}
	if role.kind == roleAligned && !pictureOrSound {
		role = trackRole{kind: roleContinuous}
	}

	pcm := pcmTrack(inputs, scaled)
	tl := &timeline{
		movie:     movie,
		tracks:    append([]*partTrack(nil), inputs...),
		scaled:    scaled,
		timescale: timescale,
		role:      role,
		pcm:       pcm,
		name:      name,
	}
	plan, err := tl.plan()
	if err != nil {
		return out, trackStarts{}, 0, err
	}

	keptTracks := tl.tracks
	keptScaled := tl.scaled

	constant := true
	for _, x := range keptTracks {
		if x.stszSampleSize == 0 || x.stszSampleSize != first.stszSampleSize {
			constant = false
		}
	}
	if constant {
		out.stszSampleSize = first.stszSampleSize
	}

	anyCTTS, anySTSS, anySTPS, anySDTP := false, false, false, false
	cttsVersion := uint8(0)
	for _, x := range keptTracks {
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
	var mediaOffset uint64
	offsets := make([]uint64, len(keptTracks))
	var sampleOff, chunkOff uint32

	for i, x := range keptTracks {
		offsets[i] = mediaOffset
		fl := plan.fills[i]
		var sample uint32
		for _, run := range keptScaled[i] {
			count, delta := run[0], run[1]
			if fl.hasLengthen && fl.lengthenSample >= sample && fl.lengthenSample < sample+count {
				longer := uint64(delta) + fl.lengthenTicks
				if longer > math.MaxUint32 {
					return out, trackStarts{}, 0, mp4Error("unsupported input", "%s: a gap of %d ticks after file %d doesn't fit a sample's 32-bit duration", name, fl.lengthenTicks, i+1)
				}
				before := fl.lengthenSample - sample
				after := sample + count - fl.lengthenSample - 1
				if before > 0 {
					out.stts = appendRun(out.stts, [2]uint32{before, delta})
				}
				out.stts = appendRun(out.stts, [2]uint32{1, uint32(longer)})
				if after > 0 {
					out.stts = appendRun(out.stts, [2]uint32{after, delta})
				}
			} else {
				out.stts = appendRun(out.stts, run)
			}
			sample += count
			mediaOffset, err = add(mediaOffset, uint64(count)*uint64(delta))
			if err != nil {
				return out, trackStarts{}, 0, err
			}
		}
		var lengthenTicks uint64
		if fl.hasLengthen {
			lengthenTicks = fl.lengthenTicks
		}
		gap, err := add(lengthenTicks, fl.silence)
		if err != nil {
			return out, trackStarts{}, 0, err
		}
		mediaOffset, err = add(mediaOffset, gap)
		if err != nil {
			return out, trackStarts{}, 0, err
		}

		md, err := rescaleUp(x.mdhdDuration, x.mdhdTimescale, timescale)
		if err != nil {
			return out, trackStarts{}, 0, err
		}
		if plan.shortened[i] >= md {
			md = 0
		} else {
			md -= plan.shortened[i]
		}
		mdWithGap, err := add(md, gap)
		if err != nil {
			return out, trackStarts{}, 0, err
		}
		out.mdhdDuration, err = add(out.mdhdDuration, mdWithGap)
		if err != nil {
			return out, trackStarts{}, 0, err
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
					if !ok || v > math.MaxInt32 {
						return out, trackStarts{}, 0, mp4Error("unsupported input", "%s composition offset does not fit 32 bits", name)
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

		if math.MaxUint32-sampleOff < x.stszCount {
			return out, trackStarts{}, 0, mp4Error("invalid data", "%s has more than 2^32 samples", name)
		}
		nextSampleOff := sampleOff + x.stszCount

		if !constant {
			if x.stszSampleSize != 0 {
				for k := uint32(0); k < x.stszCount; k++ {
					out.stsz = append(out.stsz, x.stszSampleSize)
				}
			} else {
				out.stsz = append(out.stsz, x.stsz...)
			}
		}
		if anySTSS {
			if x.hasSTSS {
				for _, v := range x.stss {
					if math.MaxUint32-v < sampleOff {
						return out, trackStarts{}, 0, mp4Error("invalid data", "%s file %d sample number overflows", name, i+1)
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
					return out, trackStarts{}, 0, mp4Error("invalid data", "%s file %d sample number overflows", name, i+1)
				}
				stps = append(stps, v+sampleOff)
			}
		}
		for _, c := range x.stsc {
			if math.MaxUint32-c[0] < chunkOff {
				return out, trackStarts{}, 0, mp4Error("invalid data", "%s file %d chunk number overflows", name, i+1)
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
			for uint64(len(out.sdtp)) < uint64(nextSampleOff) {
				out.sdtp = append(out.sdtp, 0)
			}
		}

		sampleOff = nextSampleOff
		if uint64(chunkOff)+uint64(len(x.stco)) > math.MaxUint32 {
			return out, trackStarts{}, 0, mp4Error("invalid data", "%s has more than 2^32 chunks", name)
		}
		chunkOff += uint32(len(x.stco))

		if pcm.kind == pcmKindSilence && fl.silence > 0 {
			if fl.silence > math.MaxUint32 || math.MaxUint32-sampleOff < uint32(fl.silence) {
				return out, trackStarts{}, 0, mp4Error("invalid data", "%s has more than 2^32 samples", name)
			}
			count := uint32(fl.silence)
			if uint64(chunkOff)+1 > math.MaxUint32 {
				return out, trackStarts{}, 0, mp4Error("invalid data", "%s has more than 2^32 chunks", name)
			}
			description := uint32(1)
			if len(x.stsc) > 0 {
				description = x.stsc[len(x.stsc)-1][2]
			}
			out.stts = appendRun(out.stts, [2]uint32{count, 1})
			if !constant {
				for k := uint32(0); k < count; k++ {
					out.stsz = append(out.stsz, pcm.frame)
				}
			}
			if anySTSS {
				for k := uint32(0); k < count; k++ {
					stss = append(stss, sampleOff+k+1)
				}
			}
			if anySDTP {
				for k := uint32(0); k < count; k++ {
					out.sdtp = append(out.sdtp, 0)
				}
			}
			out.stsc = append(out.stsc, [3]uint32{chunkOff + 1, count, description})
			fillerBytes := fl.silence * uint64(pcm.frame)
			if pcm.frame != 0 && fillerBytes/uint64(pcm.frame) != fl.silence {
				return out, trackStarts{}, 0, mp4Error("invalid data", "%s PCM silence size overflows", name)
			}
			off, err := filler.add(fillerBytes, pcm.byte)
			if err != nil {
				return out, trackStarts{}, 0, err
			}
			out.stco = append(out.stco, off)
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
		bounds, err := compositionBounds(out.stts, out.ctts, out.hasCTTS, name)
		if err != nil {
			return out, trackStarts{}, 0, err
		}
		out.cslg = &bounds
	}

	silence := make([]uint32, len(plan.fills))
	for i, fl := range plan.fills {
		if fl.silence > math.MaxUint32 {
			return out, trackStarts{}, 0, mp4Error("invalid data", "%s has more than 2^32 samples", name)
		}
		silence[i] = uint32(fl.silence)
	}
	groups, compact, err := mergeSampleGroups(keptTracks, silence, name)
	if err != nil {
		return out, trackStarts{}, 0, err
	}
	out.sampleGroups = groups
	out.compactGroups = compact

	edits, whole, err := mergedEdits(plan, offsets, mediaOffset, timescale, name)
	if err != nil {
		return out, trackStarts{}, 0, err
	}
	out.tkhdDuration = plan.tkhd
	if first.hasElst || !whole {
		out.hasElst = true
		out.insertEdts = !first.hasElst
		out.elst = edits
	}
	return out, trackStarts{ticks: plan.starts, exact: plan.exact, timescale: timescale}, plan.end, nil
}

func mergeTimecodeTrack(parts []partDesc, inputs []*partTrack, movieScale uint32, out mergedTrack) (mergedTrack, uint64, error) {
	first := inputs[0]
	out.mdhdTimescale = first.mdhdTimescale
	for _, x := range inputs {
		v, err := rescaleUp(x.mdhdDuration, x.mdhdTimescale, first.mdhdTimescale)
		if err != nil {
			return out, 0, err
		}
		out.mdhdDuration, err = add(out.mdhdDuration, v)
		if err != nil {
			return out, 0, err
		}
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
		out.hasSDTP = true
		out.sdtp = append(out.sdtp, first.sdtp...)
	}
	out.sampleGroups = append(out.sampleGroups, first.sbgp...)
	if first.hasElst {
		out.hasElst = true
		out.elst = append(out.elst, first.elst...)
		for i := 1; i < len(inputs); i++ {
			var presented uint64
			if inputs[i].hasElst {
				for _, e := range inputs[i].elst {
					if e.mediaTime == -1 {
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

// withinMdat checks that every chunk of a file's track falls within its mdat.
func withinMdat(mdat [2]uint64, x *partTrack) bool {
	end := mdat[0] + mdat[1]
	sample, run := uint64(0), 0
	for k, off := range x.stco {
		for run+1 < len(x.stsc) && int(x.stsc[run+1][0]) <= k+1 {
			run++
		}
		var count uint64
		if run < len(x.stsc) && int(x.stsc[run][0]) <= k+1 {
			rem := uint64(0)
			if uint64(x.stszCount) > sample {
				rem = uint64(x.stszCount) - sample
			}
			count = uint64(x.stsc[run][1])
			if count > rem {
				count = rem
			}
		}
		if off < mdat[0] || off > end {
			return false
		}
		if count == 0 {
			continue
		}
		var size uint64
		if x.stszSampleSize != 0 {
			size = uint64(x.stszSampleSize) * count
		} else {
			if sample+count > uint64(len(x.stsz)) {
				return false
			}
			for _, v := range x.stsz[sample : sample+count] {
				size += uint64(v)
			}
		}
		sample += count
		if size > end-off {
			return false
		}
	}
	return true
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

// --- Timeline planning (exact port of merge.rs Timeline, Arrival, Piece, Plan) ---

type pieceEnd struct {
	val *big.Int
	den uint32
}

type piece struct {
	duration  uint64
	mediaTime int64
	mediaRate int32
	end       *pieceEnd
}

func (p piece) presentsAfter(at *big.Int) bool {
	if p.mediaTime == -1 {
		return false
	}
	if p.end != nil {
		return p.end.val.Cmp(biMulU32(at, p.end.den)) > 0
	}
	return p.mediaRate != 0 || biU64(uint64(p.mediaTime)).Cmp(at) >= 0
}

func anyPresentsAfter(pieces []piece, at *big.Int) bool {
	for _, p := range pieces {
		if p.presentsAfter(at) {
			return true
		}
	}
	return false
}

func endsPast(end pieceEnd, point *big.Int, movieTimescale uint32) bool {
	lhs := biMulU32(end.val, movieTimescale)
	rhs := biMulU32(biAdd(point, biOne), end.den)
	return lhs.Cmp(rhs) > 0
}

type trackFill struct {
	hasLengthen    bool
	lengthenSample uint32
	lengthenTicks  uint64
	silence        uint64
}

type gapKind int

const (
	gapNone gapKind = iota
	gapEmpty
	gapFill
)

type trackGap struct {
	kind     gapKind
	duration uint64
	ticks    uint64
}

type trackPlan struct {
	pieces    [][]piece
	gaps      []trackGap
	fills     []trackFill
	shortened []uint64
	tkhd      uint64
	starts    []uint64
	exact     []*big.Int
	end       uint64
}

func (p *trackPlan) othersPresentAfter(j int, at *big.Int) bool {
	if len(p.pieces[j]) <= 1 {
		return false
	}
	return anyPresentsAfter(p.pieces[j][:len(p.pieces[j])-1], at)
}

type arrivalEdit struct {
	start *big.Int
	from  *big.Int
}

type arrival struct {
	pos      uint64
	offset   *big.Int
	hasOwner bool
	owner    int
	edit     *arrivalEdit
	end      *pieceEnd
}

func newArrival() arrival {
	return arrival{offset: biU64(0)}
}

func (a *arrival) presents(media *big.Int, timescale, movieTimescale uint32) *big.Int {
	if a.end == nil || a.edit == nil {
		return nil
	}
	scaledMedia := biMulU32(media, a.end.den)
	if biAbsDiff(a.end.val, scaledMedia).Cmp(biU32(timescale)) < 0 && media.Cmp(a.edit.from) >= 0 {
		diff := new(big.Int).Sub(media, a.edit.from)
		return biAdd(a.edit.start, biMulU32(diff, movieTimescale))
	}
	return nil
}

func (a *arrival) push(p piece, pieceAt uint64, timescale, movieTimescale uint32) {
	if p.mediaTime == -1 || p.mediaRate != mediaRateOne {
		a.edit = nil
		a.end = nil
		return
	}
	media := biAdd(a.offset, biU64(uint64(p.mediaTime)))
	if a.presents(media, timescale, movieTimescale) == nil {
		a.edit = &arrivalEdit{
			start: biMulU32(biU64(pieceAt), timescale),
			from:  media,
		}
	}
	if p.end != nil {
		a.end = &pieceEnd{
			val: biAdd(p.end.val, biMulU32(a.offset, p.end.den)),
			den: p.end.den,
		}
	} else {
		a.end = nil
	}
}

type endingKind int

const (
	endingTick endingKind = iota
	endingCut
	endingLast
)

type lastSample struct {
	kept     uint32
	ends     uint64
	hasLasts bool
	lasts    uint32
	atPoint  bool
}

type endingResult struct {
	kind    endingKind
	last    lastSample
	cutKept uint32
	cutEnd  uint64
	tick    *big.Int
}

type pcmKind int

const (
	pcmKindNo pcmKind = iota
	pcmKindSilence
	pcmKindUnfilled
)

type pcmMode struct {
	kind  pcmKind
	frame uint32
	byte  byte
	why   string
}

type timeline struct {
	movie     *movieInfo
	tracks    []*partTrack
	scaled    [][][2]uint32
	timescale uint32
	role      trackRole
	pcm       pcmMode
	name      string
}

func (tl *timeline) plan() (*trackPlan, error) {
	movie := tl.movie
	parts := movie.parts
	movieScale := movie.timescale
	timescale := tl.timescale
	n := len(parts)

	durations := make([]uint64, n)
	for i, s := range tl.scaled {
		var sum uint64
		for _, e := range s {
			var err error
			sum, err = add(sum, uint64(e[0])*uint64(e[1]))
			if err != nil {
				return nil, err
			}
		}
		durations[i] = sum
	}

	plan := &trackPlan{
		pieces:    make([][]piece, 0, n),
		gaps:      make([]trackGap, n),
		fills:     make([]trackFill, n),
		shortened: make([]uint64, n),
		starts:    make([]uint64, n),
		exact:     make([]*big.Int, n),
	}
	for i := range plan.exact {
		plan.exact[i] = biU64(0)
	}

	at := newArrival()
	presented := false

	target := func(starts *trackStarts, i int) *big.Int {
		if starts == nil || i >= len(starts.exact) || starts.exact[i] == nil {
			return nil
		}
		num := biAdd(biMulU32(starts.exact[i], timescale), biU32(starts.timescale/2))
		return biDiv(num, biU32(starts.timescale))
	}
	nearest := func(starts *trackStarts, i int) uint64 {
		if t := target(starts, i); t != nil {
			q := biDiv(biAdd(t, biU32(timescale/2)), biU32(timescale))
			if u, ok := biToU64(q); ok {
				return u
			}
		}
		return starts.ticks[i]
	}

	for i := range parts {
		p := &parts[i]
		x := tl.tracks[i]
		var fileEdits []edit
		var editTimescale uint32
		if x.hasElst {
			fileEdits = append([]edit(nil), x.elst...)
			editTimescale = p.mvhdTimescale
		} else {
			md, err := mediaDuration(x)
			if err != nil {
				return nil, err
			}
			fileEdits = []edit{{segmentDuration: md, mediaTime: 0, mediaRate: mediaRateOne}}
			editTimescale = x.mdhdTimescale
		}

		lead := 0
		if i > 0 {
			for lead < len(fileEdits) && fileEdits[lead].mediaTime == -1 {
				lead++
			}
		}
		var leadDuration uint64
		for _, e := range fileEdits[:lead] {
			d, err := rescaleUp(e.segmentDuration, editTimescale, movieScale)
			if err != nil {
				return nil, err
			}
			leadDuration, err = add(leadDuration, d)
			if err != nil {
				return nil, err
			}
		}

		var firstMedia *big.Int
		for _, e := range fileEdits[lead:] {
			if e.mediaTime != -1 {
				if e.mediaTime >= 0 {
					if m, ok := rescaleExact(uint64(e.mediaTime), x.mdhdTimescale, timescale); ok {
						firstMedia = biU64(m)
					}
				}
				break
			}
		}

		if i > 0 {
			separate := !movie.continues[i-1]
			start := at.pos
			switch tl.role.kind {
			case roleAnchor:
				if separate && !presented {
					prevEnd, err := add(plan.starts[i-1], movie.ends[i-1])
					if err != nil {
						return nil, err
					}
					if prevEnd > start {
						start = prevEnd
					}
				}
			case roleAligned:
				if separate {
					ns := nearest(tl.role.starts, i)
					if ns > start {
						start = ns
					}
				}
			}

			var exact *big.Int
			if tl.role.kind == roleAnchor && separate && !presented {
				a := biMulU32(biU64(start), timescale)
				b := biAdd(plan.exact[i-1], biMulU32(biU64(movie.ends[i-1]), timescale))
				exact = biMax(a, b)
			} else {
				if firstMedia != nil {
					exact = at.presents(biAdd(at.offset, firstMedia), timescale, movieScale)
				}
				if exact == nil {
					exact = biMulU32(biU64(at.pos), timescale)
				}
			}
			plan.starts[i] = start
			plan.exact[i] = exact

			effectiveLead := leadDuration
			if tl.role.kind != roleContinuous && effectiveLead > movie.ends[i] {
				effectiveLead = movie.ends[i]
			}
			startWithLead, err := add(start, effectiveLead)
			if err != nil {
				return nil, err
			}
			gap := startWithLead - at.pos
			leadDuration = effectiveLead

			var belongs *[2]*big.Int
			if tl.role.kind == roleAligned && separate {
				if t := target(tl.role.starts, i); t != nil && firstMedia != nil {
					belongs = &[2]*big.Int{
						biAdd(t, biMulU32(biU64(leadDuration), timescale)),
						biAdd(at.offset, firstMedia),
					}
				}
			}

			shown, length, err := tl.closeGap(plan, &at, gap, i, belongs)
			if err != nil {
				return nil, err
			}
			plan.gaps[i] = shown
			at.pos, err = add(at.pos, length)
			if err != nil {
				return nil, err
			}
			plan.tkhd, err = add(plan.tkhd, length)
			if err != nil {
				return nil, err
			}
		}

		x = tl.tracks[i]
		shownFrom := at.pos

		var hasNext bool
		var nextStartMedia *uint64
		if i+1 < n {
			hasNext = true
			y := tl.tracks[i+1]
			if !y.hasElst {
				v := durations[i]
				nextStartMedia = &v
			} else if len(y.elst) > 0 && y.elst[0].mediaTime >= 0 && y.elst[0].mediaRate == mediaRateOne {
				if m, ok := rescaleExact(uint64(y.elst[0].mediaTime), y.mdhdTimescale, timescale); ok {
					if m <= math.MaxUint64-durations[i] {
						v := m + durations[i]
						nextStartMedia = &v
					}
				}
			}
		}

		var ownEnd *uint64
		if hasNext {
			ce, err := compositionEnd(x)
			if err != nil {
				return nil, err
			}
			if c, ok := biToU64(ce); ok {
				if s, ok := rescaleExact(c, x.mdhdTimescale, timescale); ok {
					ownEnd = &s
				}
			}
		}

		nextFromStart := false
		if i+1 < n {
			y := tl.tracks[i+1]
			if !y.hasElst {
				nextFromStart = true
			} else {
				for _, e := range y.elst {
					if e.mediaTime != -1 {
						nextFromStart = e.mediaTime == 0 && e.mediaRate == mediaRateOne
						break
					}
				}
			}
		}

		type nextStartInfo struct {
			picture *big.Int
			tick    *big.Int
			sound   *big.Int
		}
		var room *uint64
		var nextStart *nextStartInfo
		if tl.role.kind == roleAligned && i+1 < n && !movie.continues[i] {
			tick := nearest(tl.role.starts, i+1)
			pic := target(tl.role.starts, i+1)
			tickExact := biMulU32(biU64(tick), timescale)
			snd := tickExact
			if nextFromStart {
				snd = pic
			}
			r := uint64(0)
			if tick > at.pos {
				r = tick - at.pos
			}
			room = &r
			nextStart = &nextStartInfo{picture: pic, tick: tickExact, sound: snd}
		}

		lastMediaIdx := -1
		for k := len(fileEdits) - 1; k >= 0; k-- {
			if fileEdits[k].mediaTime != -1 {
				lastMediaIdx = k
				break
			}
		}

		var pieces []piece
		changed := lead > 0
		var length uint64
		var cut *uint32
		var lasts *uint32
		var grown uint64
		hasMedia := false

		for k := lead; k < len(fileEdits); k++ {
			e := fileEdits[k]
			duration, err := rescaleUp(e.segmentDuration, editTimescale, movieScale)
			if err != nil {
				return nil, err
			}
			pc := piece{duration: duration, mediaTime: -1, mediaRate: e.mediaRate}
			pieceAt, err := add(at.pos, length)
			if err != nil {
				return nil, err
			}
			shownAt := biMulU32(biU64(pieceAt), timescale)

			if e.mediaTime != -1 {
				if e.mediaTime < 0 {
					return nil, mp4Error("invalid data", "%s of file %d has an edit with media time %d", tl.name, i+1, e.mediaTime)
				}
				m, ok := rescaleExact(uint64(e.mediaTime), x.mdhdTimescale, timescale)
				if !ok || m > math.MaxInt64 {
					return nil, mp4Error("invalid data", "%s of file %d has an edit with media time %d", tl.name, i+1, e.mediaTime)
				}
				pc.mediaTime = int64(m)
				if e.mediaRate == mediaRateOne {
					if sh := at.presents(biAdd(at.offset, biU64(m)), timescale, movieScale); sh != nil {
						shownAt = sh
					}
				}
				editEnd := biAdd(biMulU32(biU64(m), editTimescale), biMulU32(biU64(e.segmentDuration), timescale))
				if ownEnd != nil && e.mediaRate == mediaRateOne {
					continuesNext := k == lastMediaIdx && nextStartMedia != nil &&
						biAbsDiff(editEnd, biMulU32(biU64(*nextStartMedia), editTimescale)).Cmp(biU32(timescale)) < 0
					ownEndScaled := biMulU32(biU64(*ownEnd), editTimescale)
					if editEnd.Cmp(ownEndScaled) > 0 && !continuesNext {
						rem := uint64(0)
						if *ownEnd > uint64(pc.mediaTime) {
							rem = *ownEnd - uint64(pc.mediaTime)
						}
						kept := biToU64Sat(biDiv(biMulU32(biU64(rem), movieScale), biU32(timescale)))
						if kept > duration {
							kept = duration
						}
						changed = true
						pc.duration = kept
						editEnd = ownEndScaled
					}
				}
				if e.mediaRate == mediaRateOne {
					pc.end = &pieceEnd{val: editEnd, den: editTimescale}
				}
			}

			if room != nil {
				var media *big.Int
				if pc.mediaTime >= 0 {
					media = biMulU32(biU64(uint64(pc.mediaTime)), movieScale)
				}
				within := func(atPoint *big.Int) *big.Int {
					if media == nil {
						return nil
					}
					return biAdd(media, biSubSat(atPoint, shownAt))
				}

				var point *big.Int
				if nextStart != nil && k == lastMediaIdx {
					if nextStart.sound != nil {
						point = within(nextStart.sound)
					} else if media != nil {
						point = biAdd(media, biMulU32(biU64(*room), timescale))
					}
				}

				endsAfter := false
				if point != nil && pc.end != nil {
					endsAfter = endsPast(*pc.end, point, movieScale)
				}

				nextContinues := nextFromStart && point != nil && pc.end != nil && (k+1 == len(fileEdits) || endsAfter)
				past := endsAfter
				if !past && nextContinues && point != nil && ownEnd != nil {
					lhs := biMulU32(biU64(*ownEnd), movieScale)
					rhs := biAdd(point, biU32(movieScale/2))
					past = lhs.Cmp(rhs) > 0
				}

				if pc.duration > *room || past {
					var onTick *[2]*big.Int
					if nextStart != nil && nextStart.picture != nil {
						tWithin := within(nextStart.tick)
						pWithin := within(nextStart.picture)
						if tWithin != nil && pWithin != nil {
							onTick = &[2]*big.Int{tWithin, pWithin}
						}
					}
					before := pc.duration
					res := tl.ending(i, pc, pieces, point, nextContinues, onTick)
					switch res.kind {
					case endingLast:
						changed = true
						if res.last.kept+1 < x.stszCount {
							v := res.last.kept + 1
							cut = &v
						}
						if res.last.hasLasts {
							v := res.last.lasts
							lasts = &v
						}
						if res.last.atPoint {
							pc.duration = *room
						} else {
							diff := biSubSat(biU64(res.last.ends), biU64(uint64(pc.mediaTime)))
							pc.duration = biToU64Sat(biDiv(biMulU32(diff, movieScale), biU32(timescale)))
						}
						pc.end = &pieceEnd{val: biMulU32(biU64(res.last.ends), timescale), den: timescale}
						if pc.duration > before {
							grown = pc.duration - before
						} else {
							grown = 0
						}
					case endingCut:
						changed = true
						diff := biSubSat(biU64(res.cutEnd), biU64(uint64(pc.mediaTime)))
						pc.duration = biToU64Sat(biDiv(biMulU32(diff, movieScale), biU32(timescale)))
						pc.end = &pieceEnd{val: biMulU32(biU64(res.cutEnd), movieScale), den: movieScale}
						if res.cutKept < x.stszCount {
							v := res.cutKept
							cut = &v
						}
					case endingTick:
						if pc.duration > *room {
							changed = true
							pc.duration = *room
							if pc.end != nil {
								if res.tick != nil {
									val := biDiv(biAdd(biMulU32(res.tick, timescale), biU32(movieScale/2)), biU32(movieScale))
									pc.end = &pieceEnd{val: val, den: timescale}
								} else if media != nil {
									pc.end = &pieceEnd{val: biAdd(media, biMulU32(biU64(*room), timescale)), den: movieScale}
								} else {
									pc.end = nil
								}
							}
						}
					}
				}
				if endsAfter {
					*room = 0
				} else if pc.duration >= *room {
					*room = 0
				} else {
					*room -= pc.duration
				}
			}

			if pc.duration == 0 && duration > 0 {
				continue
			}
			length, err = add(length, pc.duration)
			if err != nil {
				return nil, err
			}
			if pc.mediaTime != -1 && x.stszCount > 0 {
				at.hasOwner = true
				at.owner = i
				hasMedia = true
			}
			at.push(pc, pieceAt, timescale, movieScale)
			pieces = append(pieces, pc)
		}

		var err error
		at.pos, err = add(at.pos, length)
		if err != nil {
			return nil, err
		}

		from := shownFrom
		if tl.role.kind == roleContinuous {
			from = plan.end
		}
		avail := uint64(0)
		if movie.ends[i] > leadDuration {
			avail = movie.ends[i] - leadDuration
		}
		shownLen := length
		if avail < shownLen {
			shownLen = avail
		}
		candEnd, err := add(from, shownLen)
		if err != nil {
			return nil, err
		}
		if candEnd > plan.end {
			plan.end = candEnd
		}

		ownTkhd, err := rescaleUp(x.tkhdDuration, p.mvhdTimescale, movieScale)
		if err != nil {
			return nil, err
		}
		if ownTkhd > leadDuration {
			ownTkhd -= leadDuration
		} else {
			ownTkhd = 0
		}
		addTkhd := ownTkhd
		if changed && i+1 < n {
			baseLen := uint64(0)
			if length > grown {
				baseLen = length - grown
			}
			if baseLen < addTkhd {
				addTkhd = baseLen
			}
			addTkhd, err = add(addTkhd, grown)
			if err != nil {
				return nil, err
			}
		}
		plan.tkhd, err = add(plan.tkhd, addTkhd)
		if err != nil {
			return nil, err
		}

		plan.pieces = append(plan.pieces, pieces)
		presented = hasMedia
		if cut != nil {
			if err := tl.truncate(i, *cut); err != nil {
				return nil, err
			}
		}
		if lasts != nil {
			plan.shortened[i] = tl.shorten(i, *lasts)
		}
		for _, e := range tl.scaled[i] {
			at.offset = biAdd(at.offset, biU64(uint64(e[0])*uint64(e[1])))
		}
	}

	return plan, nil
}

func (tl *timeline) closeGap(plan *trackPlan, at *arrival, gap uint64, file int, belongs *[2]*big.Int) (trackGap, uint64, error) {
	timescale := tl.timescale
	movieScale := tl.movie.timescale

	var exact *big.Int
	if belongs != nil {
		if shown := at.presents(belongs[1], timescale, movieScale); shown != nil {
			diff := biSubSat(belongs[0], shown)
			exact = biDiv(biAdd(diff, biU32(movieScale/2)), biU32(movieScale))
		}
	}
	if gap == 0 && (exact == nil || exact.Sign() == 0) {
		return trackGap{kind: gapNone}, 0, nil
	}
	if !at.hasOwner {
		if gap == 0 {
			return trackGap{kind: gapNone}, 0, nil
		}
		at.edit = nil
		at.end = nil
		return trackGap{kind: gapEmpty, duration: gap}, gap, nil
	}
	j := at.owner
	if tl.role.kind == roleContinuous {
		return trackGap{kind: gapNone}, 0, nil
	}
	leftOut := func(why string) (trackGap, uint64, error) {
		logWarning("%s: a gap of %.3f ms before file %d is left out: %s", tl.name, float64(gap)*1000.0/float64(movieScale), file+1, why)
		return trackGap{kind: gapNone}, 0, nil
	}

	var lastPiece *piece
	for pi := len(plan.pieces) - 1; pi >= 0; pi-- {
		if len(plan.pieces[pi]) > 0 {
			lp := plan.pieces[pi][len(plan.pieces[pi])-1]
			lastPiece = &lp
			break
		}
	}
	if lastPiece == nil || lastPiece.mediaRate != mediaRateOne || lastPiece.end == nil {
		return leftOut("the edit before it isn't one of media at the normal rate")
	}
	endVal := lastPiece.end.val
	den := lastPiece.end.den

	ticksBig := exact
	if ticksBig == nil {
		ticksBig = biDiv(biAdd(biMulU32(biU64(gap), timescale), biU32(movieScale/2)), biU32(movieScale))
	}
	ticks, ok := biToU64(ticksBig)
	if !ok || ticks == 0 {
		return trackGap{kind: gapNone}, 0, nil
	}
	lengthBig := biDiv(biAdd(biMulU32(biU64(ticks), movieScale), biU32(timescale/2)), biU32(timescale))
	length, ok := biToU64(lengthBig)
	if !ok {
		return trackGap{}, 0, mp4Error("invalid data", "%s: a gap of %d ticks doesn't fit 64 bits", tl.name, ticks)
	}

	mEnd, err := tl.mediaEnd(j)
	if err != nil {
		return trackGap{}, 0, err
	}
	toTheEnd := biAbsDiff(endVal, biMulU32(biU64(mEnd), den)).Cmp(biU32(timescale)) < 0

	if tl.pcm.kind == pcmKindUnfilled {
		return leftOut("one-tick sound track cannot be filled with silence: " + tl.pcm.why)
	}
	if tl.pcm.kind == pcmKindSilence {
		if !toTheEnd {
			keptBig := biDiv(biAdd(endVal, biU32(den/2)), biU32(den))
			kept := biToU32Sat(keptBig)
			if kept > tl.tracks[j].stszCount {
				kept = tl.tracks[j].stszCount
			}
			if plan.othersPresentAfter(j, biU32(kept)) {
				return leftOut(fmt.Sprintf("another edit of file %d presents its samples after where its last edit ends", j+1))
			}
			before := biU64(0)
			for _, e := range tl.scaled[j] {
				before = biAdd(before, biU64(uint64(e[0])*uint64(e[1])))
			}
			if err := tl.truncate(j, kept); err != nil {
				return trackGap{}, 0, err
			}
			after := biU64(0)
			for _, e := range tl.scaled[j] {
				after = biAdd(after, biU64(uint64(e[0])*uint64(e[1])))
			}
			at.offset = biSubSat(at.offset, biSubSat(before, after))
			for pi := len(plan.pieces) - 1; pi >= 0; pi-- {
				if len(plan.pieces[pi]) > 0 {
					plan.pieces[pi][len(plan.pieces[pi])-1].end = &pieceEnd{val: biMulU32(biU32(kept), den), den: den}
					break
				}
			}
			if at.end != nil {
				newEndVal := biAdd(new(big.Int).Sub(at.end.val, endVal), biMulU32(biU32(kept), den))
				at.end = &pieceEnd{val: newEndVal, den: at.end.den}
			}
		}
		var totalSamples, totalChunks uint64
		for _, x := range tl.tracks {
			totalSamples += uint64(x.stszCount)
			totalChunks += uint64(len(x.stco))
		}
		for _, f := range plan.fills {
			if f.silence > 0 {
				totalSamples += f.silence
				totalChunks++
			}
		}
		totalSamples += ticks
		totalChunks++
		if totalSamples > math.MaxUint32 || totalChunks > math.MaxUint32 {
			return leftOut("the track's samples or chunks wouldn't number in 32 bits with its silence")
		}
		plan.fills[j].silence += ticks
	} else {
		var sample uint32
		if toTheEnd {
			if tl.tracks[j].stszCount == 0 {
				return leftOut("the file has no sample to lengthen")
			}
			sample = tl.tracks[j].stszCount - 1
		} else if !tl.tracks[j].hasCTTS {
			dts := biU64(0)
			var first uint32
			found := false
			var foundSample uint32
			var foundEnds *big.Int
			for _, run := range tl.scaled[j] {
				count, delta := run[0], run[1]
				if count > 0 && biMulU32(dts, den).Cmp(endVal) < 0 {
					var k uint32
					if delta == 0 {
						k = count - 1
					} else {
						num := biSubSat(biSubSat(endVal, biMulU32(dts, den)), biOne)
						denom := biMulU32(biU32(delta), den)
						k = biToU32Sat(biDiv(num, denom))
						if k > count-1 {
							k = count - 1
						}
					}
					found = true
					foundSample = first + k
					foundEnds = biAdd(dts, biMulU32(biU64(uint64(k)+1), delta))
				}
				dts = biAdd(dts, biU64(uint64(count)*uint64(delta)))
				first += count
			}
			if !found {
				return leftOut("its edit ends before the file's first sample")
			}
			if plan.othersPresentAfter(j, foundEnds) {
				return leftOut(fmt.Sprintf("another edit of file %d presents its samples after the one that would last longer", j+1))
			}
			sample = foundSample
		} else {
			return leftOut("its edit hides the end of a track with composition offsets")
		}

		fl := &plan.fills[j]
		var longer uint64
		if !fl.hasLengthen {
			longer = ticks
		} else if fl.lengthenSample == sample {
			if math.MaxUint64-fl.lengthenTicks < ticks {
				longer = math.MaxUint64
			} else {
				longer = fl.lengthenTicks + ticks
			}
		} else {
			return leftOut("another sample of the file presents a gap already")
		}
		delta := tl.durationOf(j, sample)
		if uint64(delta) > math.MaxUint32-longer || longer > math.MaxUint32 {
			return leftOut("it doesn't fit a sample's 32-bit duration")
		}
		fl.hasLengthen = true
		fl.lengthenSample = sample
		fl.lengthenTicks = longer
	}

	if at.end != nil {
		at.end = &pieceEnd{
			val: biAdd(at.end.val, biMulU32(biU64(ticks), at.end.den)),
			den: at.end.den,
		}
	}
	at.offset = biAdd(at.offset, biU64(ticks))
	return trackGap{kind: gapFill, duration: length, ticks: ticks}, length, nil
}

func (tl *timeline) mediaEnd(j int) (uint64, error) {
	x := tl.tracks[j]
	if !x.hasCTTS {
		sum := biU64(0)
		for _, e := range tl.scaled[j] {
			sum = biAdd(sum, biU64(uint64(e[0])*uint64(e[1])))
		}
		return biToU64Sat(sum), nil
	}
	ce, err := compositionEnd(x)
	if err != nil {
		return 0, err
	}
	c, ok := biToU64(ce)
	if !ok {
		return math.MaxUint64, nil
	}
	s, ok := rescaleExact(c, x.mdhdTimescale, tl.timescale)
	if !ok {
		return math.MaxUint64, nil
	}
	return s, nil
}

func (tl *timeline) ending(i int, pc piece, pieces []piece, point *big.Int, nextContinues bool, onTick *[2]*big.Int) endingResult {
	if point == nil {
		return endingResult{kind: endingTick}
	}
	by := point
	if nextContinues && pc.end != nil {
		endAt := biDiv(biMulU32(pc.end.val, tl.movie.timescale), biU32(pc.end.den))
		by = biMin(point, endAt)
	}
	kept, end, ok := tl.soundCut(i, pc, by)
	if !ok || anyPresentsAfter(pieces, biU64(end)) {
		return endingResult{kind: endingTick}
	}
	if nextContinues {
		if last, ok := tl.keepsLast(i, pc, kept, end, point); ok {
			return endingResult{kind: endingLast, last: last}
		}
		return endingResult{kind: endingCut, cutKept: kept, cutEnd: end}
	}
	if onTick != nil && tl.keeps(i, pc, kept, end, point, onTick[0], onTick[1]) {
		return endingResult{kind: endingTick, tick: onTick[0]}
	}
	return endingResult{kind: endingCut, cutKept: kept, cutEnd: end}
}

func (tl *timeline) soundCut(i int, pc piece, point *big.Int) (uint32, uint64, bool) {
	x := tl.tracks[i]
	if pc.end == nil || tl.tracks[0].handler != fourCC("soun") || x.hasCTTS || pc.mediaTime < 0 {
		return 0, 0, false
	}
	for _, e := range tl.scaled[i] {
		if e[0] > 0 && e[1] == 0 {
			return 0, 0, false
		}
	}
	movie := biU32(tl.movie.timescale)
	from := biU64(uint64(pc.mediaTime))
	t := biU64(0)
	var s uint32
	found := false
	var cutKept uint32
	var cutEnd uint64
	for _, run := range tl.scaled[i] {
		count, delta := run[0], run[1]
		if count == 0 {
			continue
		}
		d := biU32(delta)
		rem := biSubSat(point, biMul(t, movie))
		m := biDiv(rem, biMul(d, movie))
		countBig := biU32(count)
		if m.Cmp(countBig) > 0 {
			m = countBig
		}
		b := biAdd(t, biMul(m, d))
		if m.Sign() > 0 && b.Cmp(from) > 0 && biMulU32(b, pc.end.den).Cmp(pc.end.val) <= 0 {
			if bu, ok := biToU64(b); ok {
				if mu, ok := biToU32(m); ok {
					found = true
					cutKept = s + mu
					cutEnd = bu
				}
			}
		}
		if m.Cmp(countBig) < 0 {
			break
		}
		t = biAdd(t, biMul(countBig, d))
		s += count
	}
	return cutKept, cutEnd, found
}

func (tl *timeline) keeps(i int, pc piece, kept uint32, start uint64, point, tick, picture *big.Int) bool {
	if tl.pcm.kind != pcmKindNo || pc.end == nil {
		return false
	}
	movie := biU32(tl.movie.timescale)
	from := biMul(biU64(start), movie)
	to := biMul(biAdd(biU64(start), biU32(tl.durationOf(i, kept))), movie)
	end := biDiv(biMul(pc.end.val, movie), biU32(pc.end.den))
	over := biSubSat(biMin(end, to), tick)
	lost := biSubSat(biMin(end, picture), from)
	return from.Cmp(point) < 0 && point.Cmp(to) < 0 && from.Cmp(tick) < 0 && over.Cmp(lost) <= 0
}

func (tl *timeline) keepsLast(i int, pc piece, kept uint32, start uint64, point *big.Int) (lastSample, bool) {
	if tl.pcm.kind != pcmKindNo || pc.end == nil {
		return lastSample{}, false
	}
	movie := biU32(tl.movie.timescale)
	dur := uint64(tl.durationOf(i, kept))
	if math.MaxUint64-start < dur {
		return lastSample{}, false
	}
	to := start + dur
	from := biMul(biU64(start), movie)
	end := biDiv(biMul(pc.end.val, movie), biU32(pc.end.den))
	toMovie := biMul(biU64(to), movie)
	over := biSubSat(biMin(end, toMovie), point)
	lost := biSubSat(biMin(end, point), from)
	nearest := biDiv(biAdd(point, biU32(tl.movie.timescale/2)), movie)
	var ends uint64
	if toMovie.Cmp(point) > 0 {
		u, ok := biToU64(nearest)
		if !ok {
			return lastSample{}, false
		}
		ends = u
	} else {
		ends = to
	}
	if lost.Sign() <= 0 || over.Cmp(lost) > 0 || ends <= start {
		return lastSample{}, false
	}
	res := lastSample{
		kept:    kept,
		ends:    ends,
		atPoint: biU64(ends).Cmp(nearest) == 0,
	}
	if ends < to {
		res.hasLasts = true
		res.lasts = uint32(ends - start)
	}
	return res, true
}

func (tl *timeline) durationOf(i int, s uint32) uint32 {
	var first uint32
	for _, run := range tl.scaled[i] {
		count, delta := run[0], run[1]
		end := uint64(first) + uint64(count)
		if uint64(s) >= uint64(first) && uint64(s) < end {
			return delta
		}
		if end > math.MaxUint32 {
			first = math.MaxUint32
		} else {
			first = uint32(end)
		}
	}
	return 0
}

func (tl *timeline) shorten(i int, duration uint32) uint64 {
	runs := tl.scaled[i]
	k := -1
	for idx := len(runs) - 1; idx >= 0; idx-- {
		if runs[idx][0] > 0 {
			k = idx
			break
		}
	}
	if k < 0 {
		return 0
	}
	count, delta := runs[k][0], runs[k][1]
	var replacement [][2]uint32
	if count > 1 {
		replacement = append(replacement, [2]uint32{count - 1, delta})
	}
	replacement = append(replacement, [2]uint32{1, duration})
	updated := make([][2]uint32, 0, len(runs)+1)
	updated = append(updated, runs[:k]...)
	updated = append(updated, replacement...)
	updated = append(updated, runs[k+1:]...)
	tl.scaled[i] = updated
	if delta > duration {
		return uint64(delta - duration)
	}
	return 0
}

func (tl *timeline) truncate(i int, kept uint32) error {
	trimmed, err := headPartTrack(tl.tracks[i], kept)
	if err != nil {
		return err
	}
	tl.tracks[i] = trimmed
	tl.scaled[i] = trimSTTS(tl.scaled[i], kept)
	return nil
}

func mergedEdits(plan *trackPlan, offsets []uint64, mediaEnd uint64, timescale uint32, name string) ([]edit, bool, error) {
	var edits []edit
	var end *pieceEnd
	pushEmpty := func(dur uint64, rate int32) error {
		if len(edits) > 0 && edits[len(edits)-1].mediaTime == -1 {
			var err error
			edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, dur)
			return err
		}
		edits = append(edits, edit{segmentDuration: dur, mediaTime: -1, mediaRate: rate})
		return nil
	}
	for i, pieces := range plan.pieces {
		switch plan.gaps[i].kind {
		case gapEmpty:
			if err := pushEmpty(plan.gaps[i].duration, mediaRateOne); err != nil {
				return nil, false, err
			}
			end = nil
		case gapFill:
			if len(edits) > 0 {
				var err error
				edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, plan.gaps[i].duration)
				if err != nil {
					return nil, false, err
				}
			}
			if end != nil {
				end = &pieceEnd{
					val: biAdd(end.val, biMulU32(biU64(plan.gaps[i].ticks), end.den)),
					den: end.den,
				}
			}
		}
		for _, pc := range pieces {
			if pc.mediaTime == -1 {
				if err := pushEmpty(pc.duration, pc.mediaRate); err != nil {
					return nil, false, err
				}
				end = nil
				continue
			}
			mtBig := biAdd(biU64(offsets[i]), biI64(pc.mediaTime))
			if !mtBig.IsInt64() {
				return nil, false, mp4Error("invalid data", "%s of file %d has an edit past 2^63 ticks", name, i+1)
			}
			mt := mtBig.Int64()
			continues := pc.mediaRate == mediaRateOne && end != nil &&
				biAbsDiff(end.val, biMulU32(biU64(uint64(mt)), end.den)).Cmp(biU32(timescale)) < 0
			if continues && len(edits) > 0 {
				var err error
				edits[len(edits)-1].segmentDuration, err = add(edits[len(edits)-1].segmentDuration, pc.duration)
				if err != nil {
					return nil, false, err
				}
			} else {
				edits = append(edits, edit{segmentDuration: pc.duration, mediaTime: mt, mediaRate: pc.mediaRate})
			}
			if pc.end != nil {
				end = &pieceEnd{
					val: biAdd(pc.end.val, biMulU32(biU64(offsets[i]), pc.end.den)),
					den: pc.end.den,
				}
			} else {
				end = nil
			}
		}
	}
	whole := len(edits) == 1 && edits[0].mediaTime == 0 && edits[0].mediaRate == mediaRateOne &&
		end != nil && biAbsDiff(end.val, biMulU32(biU64(mediaEnd), end.den)).Cmp(biU32(timescale)) < 0
	return edits, whole, nil
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
	if len(track.sdtp) > 0 && int(kept) < len(track.sdtp) {
		out.sdtp = append([]byte(nil), track.sdtp[:kept]...)
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
		if math.MaxUint64-dropped < term {
			dropped = math.MaxUint64
		} else {
			dropped += term
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

func compositionBounds(stts [][2]uint32, ctts []cttsEntry, has bool, name string) ([5]int64, error) {
	var out [5]int64
	if !has || len(ctts) == 0 {
		return out, nil
	}
	var least, greatest, start, end *big.Int
	dts := biI64(0)
	si := 0
	var sLeft uint32
	for _, c := range ctts {
		o := biI64(int64(c.sampleOffset))
		if least == nil || o.Cmp(least) < 0 {
			least = biClone(o)
		}
		if greatest == nil || o.Cmp(greatest) > 0 {
			greatest = biClone(o)
		}
		for j := uint32(0); j < c.sampleCount; j++ {
			for si < len(stts) && sLeft == 0 {
				sLeft = stts[si][0]
				if sLeft == 0 {
					si++
				}
			}
			var d uint32
			if si < len(stts) {
				d = stts[si][1]
				sLeft--
				if sLeft == 0 {
					si++
				}
			}
			cts := biAdd(dts, o)
			ctsEnd := biAdd(cts, biU32(d))
			if start == nil || cts.Cmp(start) < 0 {
				start = cts
			}
			if end == nil || ctsEnd.Cmp(end) > 0 {
				end = ctsEnd
			}
			dts = biAdd(dts, biU32(d))
		}
	}
	if least == nil {
		return out, nil
	}
	shift := new(big.Int).Neg(least)
	if shift.Sign() < 0 {
		shift = biI64(0)
	}
	if start == nil {
		start = biI64(0)
	}
	if end == nil {
		end = biI64(0)
	}
	vals := [5]*big.Int{shift, least, greatest, start, end}
	for i, v := range vals {
		if !v.IsInt64() {
			return out, mp4Error("unsupported input", "%s: its composition times don't fit the 64 bits of a composition shift box (cslg)", name)
		}
		out[i] = v.Int64()
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

// These types mirror the current upstream mp4-merge description model.
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
	hasElst        bool
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
	withSilence := func(left uint32, i int) (uint32, error) {
		var s uint32
		if i < len(silence) {
			s = silence[i]
		}
		if math.MaxUint32-left < s {
			return 0, invalid("has more than 2^32 samples")
		}
		return left + s, nil
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
			left, err := withSilence(left, i)
			if err != nil {
				return nil, nil, err
			}
			if left > 0 {
				index, ok := defaultGroup(x, base.groupingType)
				if !ok {
					return nil, nil, invalid("the default entry for sample group %q in file %d cannot be read", typeToString(base.groupingType), i+1)
				}
				if index != 0 && !has && groupCompactUnmerged(x) {
					return nil, nil, refused("file %d may map samples to sample group %q in an unmerged compact mapping and its description has a default entry", i+1, typeToString(base.groupingType))
				}
				grouped = grouped || index != 0
				pushGroup(&out.entries, left, index)
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
			left, err := withSilence(left, i)
			if err != nil {
				return nil, nil, err
			}
			if left > 0 {
				index, ok := defaultGroup(x, base.groupingType)
				if !ok {
					return nil, nil, invalid("the default entry for compact sample group %q in file %d cannot be read", typeToString(base.groupingType), i+1)
				}
				if index > 15 {
					return nil, nil, refused("file %d's default index %d for sample group %q doesn't fit csgm's 4 bits", i+1, index, typeToString(base.groupingType))
				}
				if index != 0 && !has && groupCompactUnmerged(x) {
					return nil, nil, refused("file %d may map samples to compact sample group %q in an unmerged compact mapping and its description has a default entry", i+1, typeToString(base.groupingType))
				}
				grouped = grouped || index != 0
				if len(out.patterns) > 0 && len(out.patterns[len(out.patterns)-1].indices) == 1 && out.patterns[len(out.patterns)-1].indices[0] == index && math.MaxUint32-out.patterns[len(out.patterns)-1].sampleCount >= left {
					out.patterns[len(out.patterns)-1].sampleCount += left
				} else {
					out.patterns = append(out.patterns, groupPattern{sampleCount: left, indices: []uint32{index}})
				}
			}
			if grouped && !sameGroupDescription(first, x, base.groupingType) {
				return nil, nil, refused("sample group descriptions of type %q differ in file %d", typeToString(base.groupingType), i+1)
			}
		}
		compact = append(compact, out)
	}
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
				leftWithSil, err := withSilence(left, i+1)
				if err != nil {
					return nil, nil, err
				}
				if leftWithSil > 0 {
					index, ok := defaultGroup(x, typ)
					if !ok {
						return nil, nil, invalid("the default entry for sample group %q in file %d cannot be read", typeToString(typ), i+2)
					}
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

func pcmTrack(tracks []*partTrack, scaled [][][2]uint32) pcmMode {
	if len(tracks) == 0 || len(scaled) != len(tracks) || tracks[0].handler != fourCC("soun") {
		return pcmMode{kind: pcmKindNo}
	}
	any := false
	for _, runs := range scaled {
		for _, run := range runs {
			if run[0] == 0 {
				continue
			}
			any = true
			if run[1] != 1 {
				return pcmMode{kind: pcmKindNo}
			}
		}
	}
	if !any {
		return pcmMode{kind: pcmKindNo}
	}
	first := tracks[0]
	for i, track := range tracks {
		if track.hasCTTS {
			return pcmMode{kind: pcmKindUnfilled, why: fmt.Sprintf("file %d gives its samples composition offsets (ctts)", i+1)}
		}
		if len(track.unmerged) != 0 {
			return pcmMode{kind: pcmKindUnfilled, why: fmt.Sprintf("file %d has a %q box, which the merge doesn't extend", i+1, typeToString(track.unmerged[0]))}
		}
		if track.stszSampleSize == 0 {
			return pcmMode{kind: pcmKindUnfilled, why: fmt.Sprintf("file %d lists its samples' sizes one by one", i+1)}
		}
		if track.stszSampleSize != first.stszSampleSize {
			return pcmMode{kind: pcmKindUnfilled, why: fmt.Sprintf("its samples have %d bytes in file 1 and %d in file %d", first.stszSampleSize, track.stszSampleSize, i+1)}
		}
	}
	var common *pcmFrames
	for _, entry := range first.sampleEntries {
		pcm, why := pcmEntry(first.stsd, entry)
		if why != "" {
			return pcmMode{kind: pcmKindUnfilled, why: why}
		}
		if common != nil && (common.frame != pcm.frame || common.byte != pcm.byte) {
			return pcmMode{kind: pcmKindUnfilled, why: "its sample entries describe other PCM frames"}
		}
		if common != nil && common.byEntry != pcm.byEntry {
			return pcmMode{kind: pcmKindUnfilled, why: "FFmpeg reads the frames of some of its sample entries by the entry, and of others in the samples' size"}
		}
		cp := pcm
		common = &cp
	}
	if common == nil {
		return pcmMode{kind: pcmKindUnfilled, why: "it has no sample entry"}
	}
	if !common.byEntry && first.stszSampleSize != common.frame {
		return pcmMode{kind: pcmKindUnfilled, why: fmt.Sprintf("its samples of %d bytes aren't its PCM frames of %d bytes", first.stszSampleSize, common.frame)}
	}
	return pcmMode{kind: pcmKindSilence, frame: common.frame, byte: common.byte}
}

// pcmTrackFrames is a helper returning the silence frame description when a track
// is supported PCM, or nil otherwise.
func pcmTrackFrames(tracks []*partTrack, scaled [][][2]uint32) *pcmFrames {
	m := pcmTrack(tracks, scaled)
	if m.kind != pcmKindSilence {
		return nil
	}
	return &pcmFrames{frame: m.frame, byte: m.byte, byEntry: true}
}

func pcmEntry(stsd []byte, entry sampleEntry) (pcmFrames, string) {
	var out pcmFrames
	if entry.at < 0 || entry.at+8 > len(stsd) {
		return out, "sample entry offset out of range"
	}
	box := stsd[entry.at:]
	size := uint64(binary.BigEndian.Uint32(box[:4]))
	if size < 36 || size > uint64(len(box)) {
		return out, "PCM sample entry is too short"
	}
	box = box[:int(size)]
	format := entry.format
	name := string([]byte{byte(format >> 24), byte(format >> 16), byte(format >> 8), byte(format)})
	switch name {
	case "twos", "sowt", "lpcm", "NONE", "raw ", "in24", "in32", "fl32", "fl64", "ipcm", "fpcm":
	default:
		return out, fmt.Sprintf("its samples are %q, not supported PCM", name)
	}
	stsdVersion := uint8(0)
	if len(stsd) > 0 {
		stsdVersion = stsd[0]
	}
	version := uint32(binary.BigEndian.Uint16(box[16:18]))
	channels := uint32(binary.BigEndian.Uint16(box[24:26]))
	entryBits := uint32(binary.BigEndian.Uint16(box[26:28]))
	var packetFrames, packetBytes, flags *uint32
	boxesAt := 36
	switch version {
	case 0:
	case 1:
		if stsdVersion != 0 {
			return out, fmt.Sprintf("sample entry version 1 in stsd version %d", stsdVersion)
		}
		if len(box) < 52 {
			return out, "PCM sample entry v1 is too short"
		}
		a, b := binary.BigEndian.Uint32(box[36:40]), binary.BigEndian.Uint32(box[44:48])
		packetFrames, packetBytes = &a, &b
		boxesAt = 52
	case 2:
		if stsdVersion != 0 {
			return out, fmt.Sprintf("sample entry version 2 in stsd version %d", stsdVersion)
		}
		if len(box) < 72 {
			return out, "PCM sample entry v2 is too short"
		}
		channels = binary.BigEndian.Uint32(box[48:52])
		entryBits = binary.BigEndian.Uint32(box[56:60])
		f, bf, bp := binary.BigEndian.Uint32(box[60:64]), binary.BigEndian.Uint32(box[64:68]), binary.BigEndian.Uint32(box[68:72])
		flags, packetBytes, packetFrames = &f, &bf, &bp
		boxesAt = 72
	default:
		return out, fmt.Sprintf("unsupported sample entry version %d", version)
	}

	bits := entryBits
	switch format {
	case fourCC("in24"):
		bits = 24
	case fourCC("in32"), fourCC("fl32"):
		bits = 32
	case fourCC("fl64"):
		bits = 64
	case fourCC("raw "):
		bits = 8
	case fourCC("ipcm"), fourCC("fpcm"):
		var config []byte
		for at := boxesAt; at+8 <= len(box); {
			childSize := uint64(binary.BigEndian.Uint32(box[at : at+4]))
			if childSize < 8 || childSize > uint64(len(box)-at) {
				break
			}
			if binary.BigEndian.Uint32(box[at+4:at+8]) == fourCC("pcmC") {
				config = box[at+8 : at+int(childSize)]
				break
			}
			at += int(childSize)
		}
		if config == nil {
			return out, "it has no pcmC"
		}
		if len(config) < 6 || config[0] != 0 || config[1] != 0 || config[2] != 0 || config[3] != 0 {
			return out, "its pcmC isn't of version 0"
		}
		bits = uint32(config[5])
		if format == fourCC("ipcm") && bits != 16 && bits != 24 && bits != 32 || format == fourCC("fpcm") && bits != 32 && bits != 64 {
			return out, fmt.Sprintf("its pcmC gives samples of %d bits", bits)
		}
	}

	if channels == 0 {
		return out, "it has no channels"
	}
	if bits == 0 || bits%8 != 0 {
		return out, fmt.Sprintf("its samples of %d bits aren't whole bytes", bits)
	}
	if channels > math.MaxUint32/(bits/8) {
		return out, "frame size exceeds 32 bits"
	}
	out.frame = channels * (bits / 8)
	if packetFrames != nil && *packetFrames != 1 {
		return pcmFrames{}, fmt.Sprintf("it declares packets of %d frames", *packetFrames)
	}
	if packetBytes != nil && *packetBytes != out.frame {
		return pcmFrames{}, fmt.Sprintf("it declares frames of %d bytes, expected %d", *packetBytes, out.frame)
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
		if entryBits == 16 {
			v = 16
		}
		ffmpegBits = &v
	case fourCC("ipcm"), fourCC("fpcm"):
	default:
		v := bits
		ffmpegBits = &v
	}

	if ffmpegBits != nil {
		if *ffmpegBits != bits {
			return pcmFrames{}, fmt.Sprintf("FFmpeg reads its %q samples of %d bits as %d-bit ones", name, bits, *ffmpegBits)
		}
		out.byEntry = out.frame <= math.MaxInt32
	}

	if format == fourCC("raw ") {
		out.byte = 0x80
	} else if format == fourCC("NONE") && bits == 8 {
		out.byte = 0x80
	} else if format == fourCC("lpcm") && flags != nil && *flags&1 == 0 && *flags&4 == 0 {
		if bits == 8 {
			out.byte = 0x80
		} else {
			return pcmFrames{}, fmt.Sprintf("its unsigned samples of %d bits have no silence of one repeated byte", bits)
		}
	}
	return out, ""
}
