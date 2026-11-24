package mp4merge

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTimelineKeepsLastShortensCodedSound(t *testing.T) {
	makeTrack := func(offset uint64) partTrack {
		stsd := make([]byte, 8)
		binary.BigEndian.PutUint32(stsd[4:], 1)
		return partTrack{
			handler:       fourCC("soun"),
			tkhdDuration:  2000,
			mdhdTimescale: 1000,
			mdhdDuration:  2000,
			stsd:          stsd,
			sampleEntries: []sampleEntry{{format: fourCC("mp4a"), at: 8}},
			stts:          [][2]uint32{{2, 1000}},
			stszCount:     2,
			stsz:          []uint32{1, 1},
			stsc:          [][3]uint32{{1, 2, 1}},
			stco:          []uint64{offset},
		}
	}
	first, second := makeTrack(0), makeTrack(2)
	movie := &movieInfo{
		parts: []partDesc{
			{mvhdTimescale: 1000, mvhdDuration: 2000, mdat: [2]uint64{0, 2}, tracks: []partTrack{first}},
			{mvhdTimescale: 1000, mvhdDuration: 2000, mdat: [2]uint64{2, 2}, tracks: []partTrack{second}},
		},
		timescale: 1000,
		continues: []bool{false},
		ends:      []uint64{1500, 2000},
	}
	// Overlap of 500 <= 500 lost: keeps sample 2 as file 1's last and shortens its duration to 500.
	filler := &mdatFillerState{at: 4}
	merged, _, _, err := mergeTrack(movie, 0, trackRole{kind: roleAligned, starts: makeTestStarts([]uint64{0, 1500})}, filler)
	if err != nil {
		t.Fatalf("mergeTrack failed: %v", err)
	}
	if merged.stszCount != 4 || merged.mdhdDuration != 3500 {
		t.Fatalf("merged samples=%d mdhdDuration=%d; want 4 and 3500", merged.stszCount, merged.mdhdDuration)
	}
	wantSTTS := [][2]uint32{{1, 1000}, {1, 500}, {2, 1000}}
	if len(merged.stts) != len(wantSTTS) {
		t.Fatalf("merged stts=%v; want %v", merged.stts, wantSTTS)
	}
	for i := range wantSTTS {
		if merged.stts[i] != wantSTTS[i] {
			t.Fatalf("merged stts[%d]=%v; want %v (full=%v)", i, merged.stts[i], wantSTTS[i], merged.stts)
		}
	}
	if merged.hasElst {
		t.Fatalf("expected continuous single edit (whole media) without needing an edit list, got %#v", merged.elst)
	}
}

func TestTimelinePlaceholderEditTrimmed(t *testing.T) {
	makeTrack := func(offset uint64, editDur uint64) partTrack {
		stsd := make([]byte, 8)
		binary.BigEndian.PutUint32(stsd[4:], 1)
		return partTrack{
			handler:       fourCC("meta"),
			tkhdDuration:  1000,
			mdhdTimescale: 1000,
			mdhdDuration:  1000,
			stsd:          stsd,
			sampleEntries: []sampleEntry{{format: fourCC("gpmd"), at: 8}},
			stts:          [][2]uint32{{1, 1000}},
			stszCount:     1,
			stsz:          []uint32{1},
			stsc:          [][3]uint32{{1, 1, 1}},
			stco:          []uint64{offset},
			hasElst:       true,
			elst:          []edit{{segmentDuration: editDur, mediaTime: 0, mediaRate: mediaRateOne}},
		}
	}
	// File 1 has a Canon-style 1000000-tick placeholder edit on 1000-tick media.
	movie := &movieInfo{
		parts: []partDesc{
			{mvhdTimescale: 1000, mvhdDuration: 1000, mdat: [2]uint64{0, 1}, tracks: []partTrack{makeTrack(0, 1000000)}},
			{mvhdTimescale: 1000, mvhdDuration: 1000, mdat: [2]uint64{1, 1}, tracks: []partTrack{makeTrack(1, 1000)}},
		},
		timescale: 1000,
		continues: []bool{true},
		ends:      []uint64{1000, 1000},
	}
	filler := &mdatFillerState{at: 2}
	merged, _, _, err := mergeTrack(movie, 0, trackRole{kind: roleContinuous}, filler)
	if err != nil {
		t.Fatalf("mergeTrack failed: %v", err)
	}
	if len(merged.elst) != 1 || merged.elst[0].segmentDuration != 2000 || merged.elst[0].mediaTime != 0 {
		t.Fatalf("merged elst = %#v; want single 2000-tick edit after trimming placeholder", merged.elst)
	}
}

func TestTimelineLeadingEmptyEditBecomesGapNotMiddleEmpty(t *testing.T) {
	makeTrack := func(offset uint64, lead uint64) partTrack {
		stsd := make([]byte, 8)
		binary.BigEndian.PutUint32(stsd[4:], 1)
		var edits []edit
		if lead > 0 {
			edits = append(edits, edit{segmentDuration: lead, mediaTime: -1, mediaRate: mediaRateOne})
		}
		edits = append(edits, edit{segmentDuration: 1000, mediaTime: 0, mediaRate: mediaRateOne})
		return partTrack{
			handler:       fourCC("soun"),
			tkhdDuration:  1000 + lead,
			mdhdTimescale: 1000,
			mdhdDuration:  1000,
			stsd:          stsd,
			sampleEntries: []sampleEntry{{format: fourCC("mp4a"), at: 8}},
			stts:          [][2]uint32{{1, 1000}},
			stszCount:     1,
			stsz:          []uint32{1},
			stsc:          [][3]uint32{{1, 1, 1}},
			stco:          []uint64{offset},
			hasElst:       true,
			elst:          edits,
		}
	}
	movie := &movieInfo{
		parts: []partDesc{
			{mvhdTimescale: 1000, mvhdDuration: 1000, mdat: [2]uint64{0, 1}, tracks: []partTrack{makeTrack(0, 0)}},
			{mvhdTimescale: 1000, mvhdDuration: 1200, mdat: [2]uint64{1, 1}, tracks: []partTrack{makeTrack(1, 200)}},
		},
		timescale: 1000,
		continues: []bool{false},
		ends:      []uint64{1000, 1200},
	}
	filler := &mdatFillerState{at: 2}
	merged, _, _, err := mergeTrack(movie, 0, trackRole{kind: roleAligned, starts: makeTestStarts([]uint64{0, 1000})}, filler)
	if err != nil {
		t.Fatalf("mergeTrack failed: %v", err)
	}
	for _, e := range merged.elst {
		if e.mediaTime == -1 {
			t.Fatalf("unexpected empty edit in middle of track: %#v", merged.elst)
		}
	}
	if len(merged.stts) != 2 || merged.stts[0] != [2]uint32{1, 1200} || merged.stts[1] != [2]uint32{1, 1000} {
		t.Fatalf("merged stts = %v; want first sample lengthened by 200-tick leading empty edit", merged.stts)
	}
}

func TestTimelinePCMSilenceUsesSGPDDefaultEntry(t *testing.T) {
	// sgpd v2 with default_length=2, default_group_description_index=1, entry_count=1, entry=[0,0]
	sgpdBody := make([]byte, 22)
	sgpdBody[0] = 2
	copy(sgpdBody[4:8], "roll")
	binary.BigEndian.PutUint32(sgpdBody[8:12], 2)
	binary.BigEndian.PutUint32(sgpdBody[12:16], 1)
	binary.BigEndian.PutUint32(sgpdBody[16:20], 1)

	makeTrack := func() *partTrack {
		return &partTrack{
			stszCount: 10,
			sbgp:      []sampleToGroup{{groupingType: fourCC("roll"), entries: [][2]uint32{{10, 1}}}},
			sgpd:      []sgpd{{groupingType: fourCC("roll"), body: sgpdBody}},
		}
	}
	tracks := []*partTrack{makeTrack(), makeTrack()}
	groups, _, err := mergeSampleGroups(tracks, []uint32{5, 0}, "PCM sgpd")
	if err != nil {
		t.Fatalf("mergeSampleGroups failed: %v", err)
	}
	// 10 (file 1) + 5 (silence using default entry 1) + 10 (file 2) = 25 samples in group 1
	if len(groups) != 1 || len(groups[0].entries) != 1 || groups[0].entries[0] != [2]uint32{25, 1} {
		t.Fatalf("merged sbgp = %#v; want [{25, 1}] (silence inherits sgpd default entry)", groups)
	}
}

func TestPCMEntryRejectsMismatchedBits(t *testing.T) {
	entry := make([]byte, 36)
	binary.BigEndian.PutUint32(entry[0:4], uint32(len(entry)))
	binary.BigEndian.PutUint32(entry[4:8], fourCC("sowt"))
	binary.BigEndian.PutUint16(entry[24:26], 2)
	binary.BigEndian.PutUint16(entry[26:28], 12) // non-standard 12-bit sowt; FFmpeg reads as 16-bit
	stsd := make([]byte, 8, 8+len(entry))
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, entry...)

	_, why := pcmEntry(stsd, sampleEntry{format: fourCC("sowt"), at: 8})
	if why == "" {
		t.Fatal("expected 12-bit sowt to be rejected by pcmEntry")
	}
}

func TestTimecodeTrackUsesWithinMdat(t *testing.T) {
	stsd := make([]byte, 8)
	binary.BigEndian.PutUint32(stsd[4:], 1)
	// Sparse stsc (does not cover stszCount=2, only 1 sample in chunk 1), which checkChunks would reject
	// but withinMdat accepts for skipped timecode tracks.
	makeTMCD := func(off uint64) partTrack {
		return partTrack{
			handler:        fourCC("tmcd"),
			skip:           true,
			tkhdDuration:   1000,
			mdhdTimescale:  1000,
			mdhdDuration:   1000,
			stsd:           stsd,
			stts:           [][2]uint32{{1, 1000}},
			stszSampleSize: 4,
			stszCount:      2,
			stsc:           [][3]uint32{{1, 1, 1}},
			stco:           []uint64{off},
		}
	}
	movie := &movieInfo{
		parts: []partDesc{
			{mvhdTimescale: 1000, mvhdDuration: 1000, mdat: [2]uint64{0, 4}, tracks: []partTrack{makeTMCD(0)}},
			{mvhdTimescale: 1000, mvhdDuration: 1000, mdat: [2]uint64{4, 4}, tracks: []partTrack{makeTMCD(4)}},
		},
		timescale: 1000,
		continues: []bool{true},
		ends:      []uint64{1000, 1000},
	}
	merged, _, _, err := mergeTrack(movie, 0, trackRole{kind: roleContinuous}, &mdatFillerState{at: 8})
	if err != nil {
		t.Fatalf("mergeTrack failed for timecode track: %v", err)
	}
	if merged.mdhdDuration != 2000 {
		t.Fatalf("timecode mdhdDuration = %d, want 2000", merged.mdhdDuration)
	}
}

type memWriteSeeker struct {
	buf []byte
	pos int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case 0:
		next = offset
	case 1:
		next = m.pos + offset
	case 2:
		next = int64(len(m.buf)) + offset
	}
	if next < 0 {
		return 0, os.ErrInvalid
	}
	m.pos = next
	return m.pos, nil
}

func (m *memWriteSeeker) Bytes() []byte { return m.buf }

func TestJoinToleratesTrailingJunkAfterMoov(t *testing.T) {
	clip1 := buildTestMP4(testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4},
		sampleDelta: 1000,
	})
	// Append non-box trailing padding/junk after moov (must be >= 8 bytes so box reader hits it).
	clip1 = append(clip1, []byte("JUNK_PADDING_AFTER_MOOV")...)
	clip2 := buildTestMP4(testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4},
		sampleDelta: 1000,
	})

	var out memWriteSeeker
	err := JoinFileStreams([]Stream{
		{Reader: bytes.NewReader(clip1), Size: int64(len(clip1))},
		{Reader: bytes.NewReader(clip2), Size: int64(len(clip2))},
	}, &out, nil)
	if err != nil {
		t.Fatalf("JoinFileStreams failed on file with trailing junk after moov: %v", err)
	}
}

func TestJoinToleratesQuickTimeZeroTerminator(t *testing.T) {
	// Build a valid MP4 and append a 4-byte 0x00000000 QuickTime terminator inside moov.
	raw := buildTestMP4(testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4},
		sampleDelta: 1000,
	})
	// Find moov at the end of raw and grow it by 4 zero bytes.
	moovIdx := bytes.Index(raw, []byte("moov"))
	if moovIdx < 4 {
		t.Fatal("moov not found in test MP4")
	}
	moovStart := moovIdx - 4
	moovSize := binary.BigEndian.Uint32(raw[moovStart : moovStart+4])
	if int(moovStart)+int(moovSize) != len(raw) {
		t.Fatalf("expected moov at end of test MP4")
	}
	withZero := append(append([]byte(nil), raw...), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(withZero[moovStart:moovStart+4], moovSize+4)

	var out memWriteSeeker
	err := JoinFileStreams([]Stream{
		{Reader: bytes.NewReader(withZero), Size: int64(len(withZero))},
		{Reader: bytes.NewReader(raw), Size: int64(len(raw))},
	}, &out, nil)
	if err != nil {
		t.Fatalf("JoinFileStreams failed on container with 4-byte QuickTime zero terminator: %v", err)
	}
}

func replaceEdtsInTrak(t *testing.T, mp4 []byte, newEdts []byte) []byte {
	t.Helper()
	moovIdx := bytes.Index(mp4, []byte("moov"))
	trakIdx := bytes.Index(mp4, []byte("trak"))
	edtsIdx := bytes.Index(mp4, []byte("edts"))
	if moovIdx < 4 || trakIdx < 4 || edtsIdx < 4 {
		t.Fatal("expected moov/trak/edts in test MP4")
	}
	edtsStart := edtsIdx - 4
	oldSize := int(binary.BigEndian.Uint32(mp4[edtsStart : edtsStart+4]))
	delta := len(newEdts) - oldSize
	out := make([]byte, 0, len(mp4)+delta)
	out = append(out, mp4[:edtsStart]...)
	out = append(out, newEdts...)
	out = append(out, mp4[edtsStart+oldSize:]...)
	for _, pos := range []int{moovIdx - 4, trakIdx - 4} {
		sz := int(binary.BigEndian.Uint32(out[pos : pos+4]))
		binary.BigEndian.PutUint32(out[pos:pos+4], uint32(sz+delta))
	}
	return out
}

func TestJoinPreservesElstVersionAndFlags(t *testing.T) {
	makeV1Edts := func(dur uint64) []byte {
		body := make([]byte, 0, 28)
		body = binary.BigEndian.AppendUint32(body, (1<<24)|0x000005)
		body = binary.BigEndian.AppendUint32(body, 1)
		body = binary.BigEndian.AppendUint64(body, dur)
		body = binary.BigEndian.AppendUint64(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(mediaRateOne))
		return makeBox("edts", makeBox("elst", body))
	}
	base := buildTestMP4(testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4},
		sampleDelta: 1000,
	})
	clip1 := replaceEdtsInTrak(t, base, makeV1Edts(1000))
	clip2 := replaceEdtsInTrak(t, base, makeV1Edts(1000))

	var out memWriteSeeker
	err := JoinFileStreams([]Stream{
		{Reader: bytes.NewReader(clip1), Size: int64(len(clip1))},
		{Reader: bytes.NewReader(clip2), Size: int64(len(clip2))},
	}, &out, nil)
	if err != nil {
		t.Fatalf("JoinFileStreams failed: %v", err)
	}
	outBytes := out.Bytes()
	outElstPos := bytes.Index(outBytes, []byte("elst"))
	if outElstPos < 4 {
		t.Fatal("merged output has no elst")
	}
	vf := binary.BigEndian.Uint32(outBytes[outElstPos+4 : outElstPos+8])
	if vf>>24 != 1 || vf&0x00ffffff != 0x000005 {
		t.Fatalf("merged elst version/flags = 0x%08x; want version 1 and flags 0x000005", vf)
	}
}

func TestJoinInsertsElstIntoEmptyEdts(t *testing.T) {
	base := buildTestMP4(testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4},
		sampleDelta: 1000,
	})
	// File 1 has an empty edts (no elst).
	clip1 := replaceEdtsInTrak(t, base, makeBox("edts", nil))
	// File 2 has an elst with a non-zero mediaTime offset (200) so merged track needs an elst (!whole).
	elstBody := make([]byte, 0, 20)
	elstBody = binary.BigEndian.AppendUint32(elstBody, 0)
	elstBody = binary.BigEndian.AppendUint32(elstBody, 1)
	elstBody = binary.BigEndian.AppendUint32(elstBody, 800)
	elstBody = binary.BigEndian.AppendUint32(elstBody, 200)
	elstBody = binary.BigEndian.AppendUint32(elstBody, uint32(mediaRateOne))
	clip2 := replaceEdtsInTrak(t, base, makeBox("edts", makeBox("elst", elstBody)))

	var out memWriteSeeker
	err := JoinFileStreams([]Stream{
		{Reader: bytes.NewReader(clip1), Size: int64(len(clip1))},
		{Reader: bytes.NewReader(clip2), Size: int64(len(clip2))},
	}, &out, nil)
	if err != nil {
		t.Fatalf("JoinFileStreams failed: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("elst")) {
		t.Fatal("expected merged output to insert elst inside the empty edts box")
	}
}

func TestCompositionBoundsComputesExactShiftAndRange(t *testing.T) {
	stts := [][2]uint32{{2, 1000}}
	ctts := []cttsEntry{{sampleCount: 1, sampleOffset: -500}, {sampleCount: 1, sampleOffset: 250}}
	bounds, err := compositionBounds(stts, ctts, true, "test")
	if err != nil {
		t.Fatalf("compositionBounds failed: %v", err)
	}
	want := [5]int64{500, -500, 250, -500, 2250}
	if bounds != want {
		t.Fatalf("compositionBounds = %v, want %v", bounds, want)
	}
}

func TestOutputRejectsInvalidFileNames(t *testing.T) {
	invalid := []string{"", "dir/", ".", "..", "dir/.", "dir/.."}
	if runtime.GOOS == "windows" {
		invalid = append(invalid, `dir\`, `dir\.`, `out.mp4:alt`)
	}
	for _, p := range invalid {
		if !notAFileName(p) {
			t.Errorf("notAFileName(%q) = false, want true", p)
		}
	}
	valid := []string{"out.mp4", filepath.Join("dir", "out.mp4")}
	for _, p := range valid {
		if notAFileName(p) {
			t.Errorf("notAFileName(%q) = true, want false", p)
		}
	}
}

func TestOutputRefusesInputBeforeWriteCheck(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "readonly.mp4")
	if err := os.WriteFile(inPath, []byte("data"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(inPath, 0o644) })
	f, err := os.Open(inPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	_, _, _, err = checkedOutputTarget(inPath, []string{inPath}, []*os.File{f})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("checkedOutputTarget error = %v; want same-file refusal before write check", err)
	}
}

func TestInsta360RejectsCorruptedRecordTrailer(t *testing.T) {
	// Build a minimal Insta360 trailer with 1 record whose trailing ID doesn't match the offsets table.
	var buf bytes.Buffer
	// Record payload (4 bytes) + corrupted trailer (format=0, id=99 instead of id=3, size=4)
	recOffset := buf.Len()
	buf.Write([]byte{1, 2, 3, 4})
	buf.Write([]byte{0, 99})
	_ = binary.Write(&buf, binary.LittleEndian, uint32(4))

	// Offsets record (id=0): 10-byte entry pointing to id=3, format=0, size=4, offset=0
	var offRec bytes.Buffer
	offRec.WriteByte(3)
	offRec.WriteByte(0)
	_ = binary.Write(&offRec, binary.LittleEndian, uint32(4))
	_ = binary.Write(&offRec, binary.LittleEndian, uint32(0))
	buf.Write(offRec.Bytes())
	buf.Write([]byte{0, 0}) // format=0, id=0 (Offsets)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(offRec.Len()))

	extraSize := uint32(buf.Len() - recOffset + instaHeaderSize)
	buf.Write(make([]byte, 32))
	_ = binary.Write(&buf, binary.LittleEndian, extraSize)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(3))
	buf.Write(instaMagic)

	data := buf.Bytes()
	fh := []*fileHandle{{stream: bytes.NewReader(data), size: int64(len(data))}}
	offsets, err := getInsta360Offsets(fh)
	if err != nil {
		t.Fatalf("getInsta360Offsets failed: %v", err)
	}
	var out memWriteSeeker
	err = mergeInsta360Metadata(fh, offsets, &out)
	if err == nil || !strings.Contains(err.Error(), "invalid Insta360 metadata") {
		t.Fatalf("mergeInsta360Metadata error = %v; want invalid Insta360 metadata error", err)
	}
}

func TestSegmentTableHighTimescaleNoOverflow(t *testing.T) {
	// 1 GHz timescale, 10-hour video: 2 * endUS * timescale overflows int64, must succeed with big.Int.
	const ts = 1_000_000_000
	part := &partDesc{
		mvhdTimescale: 1000,
		tracks: []partTrack{{
			handler:       fourCC("vide"),
			mdhdTimescale: ts,
			stszCount:     10,
			stts:          [][2]uint32{{10, 1_000_000_000}}, // 10 seconds at 1 GHz
		}},
	}
	// Shift start by 10 hours (36000s) via empty edit -> 36010s * 1e9 * 2 > MaxInt64
	part.tracks[0].hasElst = true
	part.tracks[0].elst = []edit{
		{segmentDuration: 36_000_000, mediaTime: -1, mediaRate: mediaRateOne},
		{segmentDuration: 10_000, mediaTime: 0, mediaRate: mediaRateOne},
	}
	tbl := &segmentTable{
		sources: []source{{name: "a.mp4", size: 100, frames: 10, fps: [2]uint64{1, 1}}},
		segments: []segment{{
			source:        0,
			srcFirstFrame: 0,
			frameCount:    10,
			outFirstFrame: 0,
			outStartUS:    36_000_000_000,
			durationUS:    10_000_000,
		}},
	}
	if err := segmentTableCovers(tbl, part); err != nil {
		t.Fatalf("segmentTableCovers failed on high timescale: %v", err)
	}
}

func TestJoinFileStreamsWithProgressMatchesUpstream(t *testing.T) {
	clip := buildTestMP4(testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4},
		sampleDelta: 1000,
	})
	var reported []float64
	_, err := JoinFileStreamsWith(
		[]Stream{
			{Reader: bytes.NewReader(clip), Size: int64(len(clip))},
			{Reader: bytes.NewReader(clip), Size: int64(len(clip))},
		},
		func() (io.WriteSeeker, error) { return &memWriteSeeker{}, nil },
		func(p float64) { reported = append(reported, p) },
	)
	if err != nil {
		t.Fatalf("JoinFileStreamsWith failed: %v", err)
	}
	// Fast merge (<100ms): scanning reports 0.05 and 0.1, then final 1.0 directly without 0.9999.
	if len(reported) != 3 || reported[0] != 0.05 || reported[1] != 0.1 || reported[2] != 1.0 {
		t.Fatalf("progress callbacks = %v; want [0.05 0.1 1]", reported)
	}
}
