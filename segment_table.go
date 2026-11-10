package mp4merge

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"time"
)

type source struct {
	name     string
	size     uint64
	frames   uint64
	fps      [2]uint64
	creation uint64
	clock    *sourceClock
}

type sourceClock struct {
	modulus *uint64
}

type segment struct {
	source        int
	srcFirstFrame uint64
	frameCount    uint64
	outFirstFrame uint64
	srcStartUS    int64
	outStartUS    int64
	durationUS    int64
	srcCaptureUS  int64
	outCaptureUS  int64
	captureUS     int64
	clockShift    *int64
}

type segmentTable struct {
	sources  []source
	segments []segment
}

func segmentPayloads(udta []byte) [][]byte {
	var out [][]byte
	for pos := 0; len(udta)-pos >= 8; {
		rest := udta[pos:]
		size32 := binary.BigEndian.Uint32(rest[:4])
		header, size := uint64(8), uint64(size32)
		if size32 == 0 {
			size = uint64(len(rest))
		} else if size32 == 1 {
			if len(rest) < 16 {
				break
			}
			header = 16
			size = binary.BigEndian.Uint64(rest[8:16])
		}
		if size < header || size > uint64(len(rest)) {
			break
		}
		if bytes.Equal(rest[4:8], []byte("MTRM")) {
			out = append(out, append([]byte(nil), rest[header:size]...))
		}
		pos += int(size)
	}
	return out
}

func parseSegmentTable(payloads [][]byte, part *partDesc) (*segmentTable, error) {
	if len(payloads) == 0 {
		return nil, nil
	}
	if len(payloads) != 1 {
		return nil, fmt.Errorf("moov/udta has %d MTRM boxes", len(payloads))
	}
	dec := json.NewDecoder(bytes.NewReader(payloads[0]))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid MTRM JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("MTRM JSON has more than one value")
		}
		return nil, fmt.Errorf("invalid trailing MTRM JSON: %w", err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("MTRM JSON is not an object")
	}
	version, ok := jsonU64(root["version"])
	if !ok || version != 1 {
		return nil, fmt.Errorf("MTRM is not a version-1 table")
	}
	get := func(m map[string]any, key string) (any, error) {
		v, ok := m[key]
		if !ok {
			return nil, fmt.Errorf("missing %s", key)
		}
		return v, nil
	}
	parsePair := func(v any, what string) ([2]uint64, error) {
		var pair [2]uint64
		a, ok := v.([]any)
		if !ok || len(a) != 2 {
			return pair, fmt.Errorf("%s is not a pair of numbers", what)
		}
		var good bool
		pair[0], good = jsonU64(a[0])
		if !good {
			return pair, fmt.Errorf("%s is not a pair of whole numbers", what)
		}
		pair[1], good = jsonU64(a[1])
		if !good {
			return pair, fmt.Errorf("%s is not a pair of whole numbers", what)
		}
		return pair, nil
	}
	var table segmentTable
	sourcesRaw, ok := root["sources"].([]any)
	if !ok {
		return nil, fmt.Errorf("no sources")
	}
	for _, raw := range sourcesRaw {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("a source is not an object")
		}
		nameRaw, err := get(m, "name")
		if err != nil {
			return nil, fmt.Errorf("a source: %w", err)
		}
		name, ok := nameRaw.(string)
		if !ok {
			return nil, fmt.Errorf("a source name is not a string")
		}
		sizeRaw, err := get(m, "size")
		if err != nil {
			return nil, fmt.Errorf("a source: %w", err)
		}
		size, ok := jsonU64(sizeRaw)
		if !ok {
			return nil, fmt.Errorf("a source size is not a whole number")
		}
		framesRaw, err := get(m, "frames")
		if err != nil {
			return nil, fmt.Errorf("a source: %w", err)
		}
		frames, ok := jsonU64(framesRaw)
		if !ok {
			return nil, fmt.Errorf("a source frames is not a whole number")
		}
		fpsRaw, err := get(m, "fps")
		if err != nil {
			return nil, fmt.Errorf("a source: %w", err)
		}
		fps, err := parsePair(fpsRaw, "a source fps")
		if err != nil {
			return nil, err
		}
		creationRaw, err := get(m, "creation_time")
		if err != nil {
			return nil, fmt.Errorf("a source: %w", err)
		}
		var creation uint64
		if creationRaw != nil {
			s, ok := creationRaw.(string)
			if !ok {
				return nil, fmt.Errorf("a source creation_time is not a string")
			}
			creation, ok = utcSecondSince1904(s)
			if !ok {
				return nil, fmt.Errorf("a source creation_time is not UTC YYYY-MM-DDTHH:MM:SSZ")
			}
		}
		clockRaw, err := get(m, "clock")
		if err != nil {
			return nil, fmt.Errorf("a source: %w", err)
		}
		var clock *sourceClock
		if clockRaw != nil {
			cm, ok := clockRaw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("a source clock is not an object")
			}
			nameRaw, _ := cm["name"]
			if _, ok := nameRaw.(string); !ok {
				return nil, fmt.Errorf("a source clock has no name")
			}
			rateRaw, ok := cm["rate"]
			if !ok {
				return nil, fmt.Errorf("a source clock has no rate")
			}
			rate, err := parsePair(rateRaw, "a clock rate")
			if err != nil {
				return nil, err
			}
			modRaw, ok := cm["modulus"]
			if !ok {
				return nil, fmt.Errorf("a source clock has no modulus")
			}
			c := &sourceClock{}
			if modRaw != nil {
				modulus, ok := jsonU64(modRaw)
				if !ok {
					return nil, fmt.Errorf("a clock modulus is not a whole number")
				}
				c.modulus = &modulus
			}
			if rate[0] == 0 || rate[1] == 0 || c.modulus != nil && *c.modulus == 0 {
				return nil, fmt.Errorf("a source clock has a zero rate or modulus")
			}
			clock = c
		}
		if frames == 0 || fps[0] == 0 || fps[1] == 0 {
			return nil, fmt.Errorf("a source has %d frames at %d/%d fps", frames, fps[0], fps[1])
		}
		table.sources = append(table.sources, source{name, size, frames, fps, creation, clock})
	}
	if len(table.sources) == 0 {
		return nil, fmt.Errorf("no sources")
	}
	segmentsRaw, ok := root["segments"].([]any)
	if !ok {
		return nil, fmt.Errorf("no segments")
	}
	var frame uint64
	var prevTime, prevCapture *int64
	for k, raw := range segmentsRaw {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("segment %d is not an object", k)
		}
		getU := func(key string) (uint64, error) {
			v, err := get(m, key)
			if err != nil {
				return 0, err
			}
			n, ok := jsonU64(v)
			if !ok {
				return 0, fmt.Errorf("%s is not a whole number", key)
			}
			return n, nil
		}
		getI := func(key string) (int64, error) {
			v, err := get(m, key)
			if err != nil {
				return 0, err
			}
			n, ok := jsonI64(v)
			if !ok {
				return 0, fmt.Errorf("%s is not a whole number", key)
			}
			return n, nil
		}
		source64, err := getU("source")
		if err != nil || source64 > uint64(maxInt()) {
			return nil, fmt.Errorf("segment %d has an invalid source", k)
		}
		srcFirst, err := getU("src_first_frame")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		count, err := getU("frame_count")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		outFirst, err := getU("out_first_frame")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		srcStart, err := getI("src_start_us")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		outStart, err := getI("out_start_us")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		duration, err := getI("duration_us")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		srcCapture, err := getI("src_capture_start_us")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		outCapture, err := getI("out_capture_start_us")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		captureDuration, err := getI("capture_duration_us")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		requested, err := get(m, "requested")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		if requested != nil {
			if _, ok := requested.(map[string]any); !ok {
				return nil, fmt.Errorf("segment requested is not an object or null")
			}
		}
		for _, key := range []string{"start", "entry", "end"} {
			v, err := get(m, key)
			if err != nil {
				return nil, fmt.Errorf("segment %d: %w", k, err)
			}
			if _, ok := v.(string); !ok {
				return nil, fmt.Errorf("segment %s is not a string", key)
			}
		}
		clockRaw, err := get(m, "clock_shift")
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", k, err)
		}
		var shift *int64
		if clockRaw != nil {
			value, ok := jsonI64(clockRaw)
			if !ok {
				return nil, fmt.Errorf("segment clock_shift is not a whole number")
			}
			shift = &value
		}
		source := int(source64)
		if source >= len(table.sources) {
			return nil, fmt.Errorf("segment %d names source %d of %d", k, source, len(table.sources))
		}
		if count == 0 {
			return nil, fmt.Errorf("segment %d has no frames", k)
		}
		if math.MaxUint64-outFirst < count || math.MaxUint64-srcFirst < count {
			return nil, fmt.Errorf("segment %d frame range overflows", k)
		}
		if srcFirst+count > table.sources[source].frames {
			return nil, fmt.Errorf("segment %d exceeds source frames", k)
		}
		if srcStart < 0 || outStart < 0 || duration < 0 || srcCapture < 0 || outCapture < 0 || captureDuration < 0 {
			return nil, fmt.Errorf("segment %d has a negative time", k)
		}
		if math.MaxInt64-srcStart < duration || math.MaxInt64-srcCapture < captureDuration || math.MaxInt64-outStart < duration || math.MaxInt64-outCapture < captureDuration {
			return nil, fmt.Errorf("segment %d time overflows", k)
		}
		if outFirst != frame {
			return nil, fmt.Errorf("segment %d starts at frame %d, expected %d", k, outFirst, frame)
		}
		if prevTime != nil && (*prevTime != outStart || *prevCapture != outCapture) {
			return nil, fmt.Errorf("segment %d does not tile the previous segment's times", k)
		}
		clock := table.sources[source].clock
		if clock == nil {
			if shift != nil {
				return nil, fmt.Errorf("segment %d has a clock shift for a source without a clock", k)
			}
		} else if shift == nil {
			return nil, fmt.Errorf("segment %d lacks its source clock shift", k)
		} else if clock.modulus != nil && (*shift < 0 || uint64(*shift) >= *clock.modulus) {
			return nil, fmt.Errorf("segment %d clock shift exceeds modulus", k)
		}
		frame = outFirst + count
		nextTime := outStart + duration
		nextCapture := outCapture + captureDuration
		prevTime = &nextTime
		prevCapture = &nextCapture
		table.segments = append(table.segments, segment{source, srcFirst, count, outFirst, srcStart, outStart, duration, srcCapture, outCapture, captureDuration, shift})
	}
	if len(table.segments) == 0 {
		return nil, fmt.Errorf("no segments")
	}
	if err := segmentTableCovers(&table, part); err != nil {
		return nil, err
	}
	return &table, nil
}

func jsonU64(v any) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok || bytes.ContainsAny([]byte(n.String()), ".eE-") {
		return 0, false
	}
	u, err := strconv.ParseUint(n.String(), 10, 64)
	return u, err == nil
}
func jsonI64(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok || bytes.ContainsAny([]byte(n.String()), ".eE") {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}

func utcSecondSince1904(s string) (uint64, bool) {
	if len(s) != 20 || s[19] != 'Z' {
		return 0, false
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		return 0, false
	}
	const epoch int64 = -2082844800
	seconds := t.Unix() - epoch
	if seconds < 0 {
		return 0, false
	}
	return uint64(seconds), true
}

type videoFrames struct {
	timescale    uint64
	starts       []int64
	end, nominal int64
}

func videoFramesOf(part *partDesc) *videoFrames {
	var track *partTrack
	for i := range part.tracks {
		if part.tracks[i].handler == fourCC("vide") {
			track = &part.tracks[i]
			break
		}
	}
	if track == nil || track.mdhdTimescale == 0 || part.mvhdTimescale == 0 {
		return nil
	}
	if uint64(track.stszCount) > uint64(maxInt()) {
		return nil
	}
	durations := make([]int64, 0, track.stszCount)
	for _, e := range track.stts {
		if uint64(len(durations))+uint64(e[0]) > uint64(track.stszCount) {
			return nil
		}
		for j := uint32(0); j < e[0]; j++ {
			durations = append(durations, int64(e[1]))
		}
	}
	if len(durations) == 0 {
		return nil
	}
	offsets := make([]int64, len(durations))
	if track.hasCTTS {
		at := 0
		for _, e := range track.ctts {
			for j := uint32(0); j < e.sampleCount && at < len(offsets); j++ {
				offsets[at] = int64(e.sampleOffset)
				at++
			}
		}
	}
	type decoded struct{ dts, cts, duration int64 }
	samples := make([]decoded, 0, len(durations))
	var dts int64
	for i, d := range durations {
		if dts > math.MaxInt64-d {
			return nil
		}
		samples = append(samples, decoded{dts, dts + offsets[i], d})
		dts += d
	}
	empty, mediaTime, mediaEnd, edited := int64(0), int64(0), int64(math.MaxInt64), false
	if track.hasElst {
		e := 0
		if len(track.elst) > 1 {
			if track.elst[0].mediaTime != -1 {
				return nil
			}
			if track.elst[0].segmentDuration > math.MaxInt64 {
				return nil
			}
			empty = int64(track.elst[0].segmentDuration)
			e = 1
		}
		if e < len(track.elst) {
			edit := track.elst[e]
			if len(track.elst) > e+1 || edit.mediaTime < 0 || edit.mediaRate != mediaRateOne {
				return nil
			}
			mediaTime = edit.mediaTime
			edited = true
			if edit.segmentDuration > 0 {
				d := (uint64(edit.segmentDuration)*uint64(track.mdhdTimescale) + uint64(part.mvhdTimescale)/2) / uint64(part.mvhdTimescale)
				if d > math.MaxInt64 || mediaTime > math.MaxInt64-int64(d) {
					return nil
				}
				mediaEnd = mediaTime + int64(d)
			}
		}
	}
	emptyTicks := int64((uint64(empty)*uint64(track.mdhdTimescale) + uint64(part.mvhdTimescale)/2) / uint64(part.mvhdTimescale))
	inEdit := func(cts int64) bool { return cts >= mediaTime && cts < mediaEnd }
	origin := int64(0)
	if edited {
		found := false
		var firstDTS, firstCTS int64
		for _, s := range samples {
			if inEdit(s.cts) {
				firstDTS = s.dts
				firstCTS = s.cts
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		for _, s := range samples {
			if inEdit(s.cts) && s.cts < firstCTS {
				firstCTS = s.cts
			}
		}
		origin = firstCTS
		if firstDTS > origin {
			origin = firstDTS
		}
	}
	type presented struct{ cts, duration int64 }
	var ps []presented
	for _, s := range samples {
		if inEdit(s.cts) {
			ps = append(ps, presented{s.cts, s.duration})
		}
	}
	if len(ps) == 0 {
		return nil
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].cts < ps[j].cts })
	last := ps[len(ps)-1]
	end := last.cts + last.duration
	if end > mediaEnd {
		end = mediaEnd
	}
	end = end - origin + emptyTicks
	starts := make([]int64, len(ps))
	for i, s := range ps {
		starts[i] = s.cts - origin + emptyTicks
	}
	copyDurations := append([]int64(nil), durations...)
	sort.Slice(copyDurations, func(i, j int) bool { return copyDurations[i] < copyDurations[j] })
	nominal, best, run := copyDurations[0], 0, 0
	for i, d := range copyDurations {
		if i > 0 && copyDurations[i-1] == d {
			run++
		} else {
			run = 1
		}
		if run > best {
			best = run
			nominal = d
		}
	}
	if nominal <= 0 {
		return nil
	}
	return &videoFrames{uint64(track.mdhdTimescale), starts, end, nominal}
}

func videoTimeUS(v *videoFrames, k int) (int64, bool) {
	if k < 0 || k >= len(v.starts) {
		return 0, false
	}
	n := int64(v.starts[k]) * 1_000_000
	d := int64(v.timescale)
	q := (absInt64(n)*2 + d) / (2 * d)
	if n < 0 {
		q = -q
	}
	return q, true
}
func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func segmentTableCovers(t *segmentTable, part *partDesc) error {
	v := videoFramesOf(part)
	if v == nil {
		return fmt.Errorf("the file's video frames cannot be read")
	}
	last := t.segments[len(t.segments)-1]
	frames := last.outFirstFrame + last.frameCount
	if frames != uint64(len(v.starts)) {
		return fmt.Errorf("segments cover %d frames, video has %d", frames, len(v.starts))
	}
	for _, s := range t.segments {
		tm, ok := videoTimeUS(v, int(s.outFirstFrame))
		if !ok || tm != s.outStartUS {
			return fmt.Errorf("segment starts at %d µs, video at %d µs", s.outStartUS, tm)
		}
	}
	endUS := int64(last.outStartUS + last.durationUS)
	left := 2*endUS*int64(v.timescale) - 2*v.end*1_000_000
	if left < 0 {
		left = -left
	}
	if left > 2*v.nominal*1_000_000+int64(v.timescale) {
		return fmt.Errorf("segment table ends more than a frame from video end")
	}
	return nil
}

func (t *segmentTable) first() *segment { return &t.segments[0] }
func (t *segmentTable) last() *segment  { return &t.segments[len(t.segments)-1] }
