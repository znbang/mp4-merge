package mp4merge

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestJoinAggregatesDurations(t *testing.T) {
	dir := t.TempDir()
	specA := testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{5, 7, 6},
		sampleDelta: 1000,
		keyframes:   []uint32{1, 3},
	}
	specB := testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4, 6},
		sampleDelta: 750,
		keyframes:   []uint32{1},
	}

	input1 := filepath.Join(dir, "clip1.mp4")
	input2 := filepath.Join(dir, "clip2.mp4")
	output := filepath.Join(dir, "merged.mp4")

	writeTestMP4(t, input1, specA)
	writeTestMP4(t, input2, specB)

	if err := JoinFiles([]string{input1, input2}, output, nil); err != nil {
		t.Fatalf("JoinFiles failed: %v", err)
	}

	desc := loadDescFromFile(t, output)

	expectedDuration := uint64(specA.sampleDelta*uint32(len(specA.sampleSizes)) + specB.sampleDelta*uint32(len(specB.sampleSizes)))
	if desc.movieDuration != expectedDuration {
		t.Fatalf("mvhd duration = %d, want %d", desc.movieDuration, expectedDuration)
	}

	var track *mergedTrack
	for i := range desc.tracks {
		candidate := &desc.tracks[i]
		if candidate.stszCount > 0 {
			track = candidate
			break
		}
	}
	if track == nil {
		t.Fatalf("no track metadata found in merged output")
	}

	if track.tkhdDuration != expectedDuration {
		t.Fatalf("tkhd duration = %d, want %d", track.tkhdDuration, expectedDuration)
	}
	if track.mdhdDuration != expectedDuration {
		t.Fatalf("mdhd duration = %d, want %d", track.mdhdDuration, expectedDuration)
	}

	expectedSamples := append(append([]uint32{}, specA.sampleSizes...), specB.sampleSizes...)
	if track.stszCount != uint32(len(expectedSamples)) {
		t.Fatalf("stsz count = %d, want %d", track.stszCount, len(expectedSamples))
	}
	if !reflect.DeepEqual(track.stsz, expectedSamples) {
		t.Fatalf("merged sample sizes = %v, want %v", track.stsz, expectedSamples)
	}

	if len(track.stco) == 0 {
		t.Fatalf("expected non-empty chunk offsets")
	}
}

func TestJoinAppendsMetadata(t *testing.T) {
	dir := t.TempDir()
	specA := testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{5, 5, 5},
		sampleDelta: 900,
		keyframes:   []uint32{1},
		insta: &instaSpec{
			version: 4,
			entries: []instaEntrySpec{
				{id: 10, format: 1, payload: []byte("camera-1-meta-frame-a")},
				{id: 11, format: 2, payload: []byte{1, 2, 3, 4, 5, 6}},
			},
		},
	}
	specB := testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{6, 6},
		sampleDelta: 600,
		keyframes:   []uint32{1, 2},
		insta: &instaSpec{
			version: 4,
			entries: []instaEntrySpec{
				{id: 10, format: 1, payload: []byte("camera-2-meta-frame-b")},
				{id: 12, format: 3, payload: []byte{9, 8, 7, 6}},
			},
		},
	}

	input1 := filepath.Join(dir, "clip1.mp4")
	input2 := filepath.Join(dir, "clip2.mp4")
	output := filepath.Join(dir, "merged.mp4")

	writeTestMP4(t, input1, specA)
	writeTestMP4(t, input2, specB)

	if err := JoinFiles([]string{input1, input2}, output, nil); err != nil {
		t.Fatalf("JoinFiles failed: %v", err)
	}

	tail := readTail(t, output, 512)
	if !bytes.Contains(tail, specA.insta.entries[0].payload) {
		t.Fatalf("expected metadata to include payload from first input")
	}
	if !bytes.Contains(tail, specB.insta.entries[0].payload) {
		t.Fatalf("expected metadata to include payload from second input")
	}
	if !bytes.HasSuffix(tail, instaMagic) {
		t.Fatalf("expected insta360 magic footer in merged output")
	}
}

func TestJoinKeepsDestinationWhenOutputAliasesInput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")
	spec := testFileSpec{trackID: 1, sampleSizes: []uint32{4, 5}, sampleDelta: 1000, keyframes: []uint32{1}}
	writeTestMP4(t, input, spec)
	before, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := JoinFiles([]string{input}, input, nil); err == nil {
		t.Fatal("expected input/output identity to be refused")
	}
	after, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("input was changed after the merge was refused")
	}
}

func TestJoinFailureLeavesExistingDestinationUntouched(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "invalid.mp4")
	output := filepath.Join(dir, "existing.mp4")
	if err := os.WriteFile(input, []byte("not an mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := []byte("previous complete output")
	if err := os.WriteFile(output, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := JoinFiles([]string{input}, output, nil); err == nil {
		t.Fatal("expected malformed input to be rejected")
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination changed: got %q, want %q", got, want)
	}
}

func TestJoinWritesDirectlyToDevice(t *testing.T) {
	device := "/dev/null"
	if _, err := os.Stat(device); err != nil {
		t.Skip("/dev/null is unavailable")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")
	writeTestMP4(t, input, testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{5},
		sampleDelta: 1000,
		keyframes:   []uint32{1},
	})
	if err := JoinFiles([]string{input}, device, nil); err != nil {
		t.Fatalf("JoinFiles to device failed: %v", err)
	}
}

func TestJoinSelectsMdatContainingTrackChunks(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.mp4"), filepath.Join(dir, "b.mp4")
	out := filepath.Join(dir, "merged.mp4")
	first := testFileSpec{trackID: 1, sampleSizes: []uint32{3, 4}, sampleDelta: 1000, keyframes: []uint32{1}}
	second := testFileSpec{trackID: 1, sampleSizes: []uint32{2, 5}, sampleDelta: 1000, keyframes: []uint32{1}}
	if err := os.WriteFile(a, buildTestMP4WithTwoMdats(first), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, buildTestMP4WithTwoMdats(second), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := JoinFiles([]string{a, b}, out, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("unused-mdat")) {
		t.Fatal("unreferenced mdat payload was copied")
	}
	if !bytes.Contains(data, makeSampleData(first.sampleSizes, byte(first.trackID))) || !bytes.Contains(data, makeSampleData(second.sampleSizes, byte(second.trackID))) {
		t.Fatal("selected mdat payload is missing")
	}
}

func TestMergeEditsCutsOverlapAtSeparateRecording(t *testing.T) {
	track := func(editDuration uint64) *partTrack {
		return &partTrack{
			mdhdTimescale: 1000,
			stts:          [][2]uint32{{1, 1000}},
			stszCount:     1,
			hasElst:       true,
			elst:          []edit{{segmentDuration: editDuration, mediaTime: 0, mediaRate: mediaRateOne}},
		}
	}
	tracks := []*partTrack{track(1200), track(1000)}
	parts := []partDesc{{mvhdTimescale: 1000}, {mvhdTimescale: 1000}}
	starts := []uint64{0, 1000}
	continues := []bool{false}

	edits, whole, duration, fills, lengthens, _, err := mergeEdits(tracks, parts, 1000, 1000, 2000, starts, continues, true, nil)
	if err != nil {
		t.Fatalf("mergeEdits failed: %v", err)
	}
	if whole {
		t.Fatal("expected an edit list for the separate recording boundary")
	}
	if duration != 2000 {
		t.Fatalf("timeline duration = %d, want 2000", duration)
	}
	if len(fills) != 2 || fills[0] != 0 || fills[1] != 0 {
		t.Fatalf("PCM fills = %v, want none", fills)
	}
	if len(lengthens) != 2 || lengthens[0] != 0 || lengthens[1] != 0 {
		t.Fatalf("sample lengthenings = %v, want none", lengthens)
	}
	if len(edits) != 2 {
		t.Fatalf("got %d edits, want 2: %#v", len(edits), edits)
	}
	if edits[0].segmentDuration != 1000 || edits[0].mediaTime != 0 {
		t.Fatalf("first edit = %#v, want 1000 ticks at media time 0", edits[0])
	}
	if edits[1].segmentDuration != 1000 || edits[1].mediaTime != 1000 {
		t.Fatalf("second edit = %#v, want 1000 ticks at media time 1000", edits[1])
	}
}

func TestMergeTrackCutsSoundAtSampleBoundary(t *testing.T) {
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
	desc := &desc{parts: []partDesc{
		{mvhdTimescale: 1000, mvhdDuration: 2000, mdat: [2]uint64{0, 2}, tracks: []partTrack{first}},
		{mvhdTimescale: 1000, mvhdDuration: 2000, mdat: [2]uint64{2, 2}, tracks: []partTrack{second}},
	}, movieTimescale: 1000, mdatOffset: 4}
	merged, _, err := mergeTrack(desc, 0, 1000, []uint64{0, 1500}, []bool{false})
	if err != nil {
		t.Fatalf("mergeTrack failed: %v", err)
	}
	if merged.stszCount != 3 || merged.mdhdDuration != 3500 {
		t.Fatalf("merged samples=%d media duration=%d; want 3 samples and 3500 ticks", merged.stszCount, merged.mdhdDuration)
	}
	if len(merged.stts) != 2 || merged.stts[0] != [2]uint32{1, 1500} || merged.stts[1] != [2]uint32{2, 1000} {
		t.Fatalf("merged stts=%v; want first kept sample extended over the gap", merged.stts)
	}
	if len(merged.stco) != 2 || len(merged.stsc) != 2 || merged.stsc[0] != [3]uint32{1, 1, 1} || merged.stsc[1] != [3]uint32{2, 2, 1} {
		t.Fatalf("merged chunks: stco=%v stsc=%v; want one kept first-file sample followed by second-file chunk", merged.stco, merged.stsc)
	}
}

func TestRecordingTimeArithmeticMatchesRustWidths(t *testing.T) {
	if !endsNear(big.NewInt(1), 1000, 3000) {
		t.Fatal("a 2-second timestamp margin should count as a nearby chapter")
	}
	if endsNear(big.NewInt(1), 1000, 3001) {
		t.Fatal("a timestamp outside the 2-second margin must not count as a nearby chapter")
	}
	if endsNear(big.NewInt(0).SetUint64(math.MaxUint64), math.MaxUint32, 0) {
		t.Fatal("very large creation times must not wrap into the chapter window")
	}
	if frameEndIs(math.MaxUint64-1, 2, 0) {
		t.Fatal("overflowing segment frame ranges must not wrap to frame zero")
	}
}

func TestPCMGapsProduceSilenceSamples(t *testing.T) {
	makeTrack := func() *partTrack {
		entry := make([]byte, 36)
		binary.BigEndian.PutUint32(entry[0:4], uint32(len(entry)))
		binary.BigEndian.PutUint32(entry[4:8], fourCC("sowt"))
		binary.BigEndian.PutUint16(entry[24:26], 2)
		binary.BigEndian.PutUint16(entry[26:28], 16)
		stsd := make([]byte, 8, 8+len(entry))
		binary.BigEndian.PutUint32(stsd[4:8], 1)
		stsd = append(stsd, entry...)
		return &partTrack{
			handler:        fourCC("soun"),
			mdhdTimescale:  1000,
			mdhdDuration:   10,
			stts:           [][2]uint32{{10, 1}},
			stszSampleSize: 4,
			stszCount:      10,
			stsd:           stsd,
			sampleEntries:  []sampleEntry{{format: fourCC("sowt"), at: 8}},
			stsc:           [][3]uint32{{1, 10, 1}},
			stco:           []uint64{0},
			hasElst:        true,
			elst:           []edit{{segmentDuration: 10, mediaTime: 0, mediaRate: mediaRateOne}},
		}
	}
	tracks := []*partTrack{makeTrack(), makeTrack()}
	tracks[0].sbgp = []sampleToGroup{{groupingType: fourCC("roll"), entries: [][2]uint32{{10, 1}}}}
	scaled := [][][2]uint32{{{10, 1}}, {{10, 1}}}
	pcm := pcmTrackFrames(tracks, scaled)
	if pcm == nil || pcm.frame != 4 || pcm.byte != 0 {
		t.Fatalf("PCM frame description = %#v, want 4-byte little-endian PCM silence", pcm)
	}
	parts := []partDesc{{mvhdTimescale: 1000}, {mvhdTimescale: 1000}}
	edits, whole, duration, fills, lengthens, _, err := mergeEdits(tracks, parts, 1000, 1000, 20, []uint64{0, 15}, []bool{false}, true, pcm)
	if err != nil {
		t.Fatalf("mergeEdits failed: %v", err)
	}
	if !whole || duration != 25 || len(edits) != 1 || edits[0].segmentDuration != 25 || fills[0] != 5 {
		t.Fatalf("edits=%#v whole=%v duration=%d fills=%v; want one 25-tick edit with five silence samples", edits, whole, duration, fills)
	}
	if lengthens[0] != 0 || lengthens[1] != 0 {
		t.Fatalf("PCM silence should not lengthen a source sample: %v", lengthens)
	}
	groups, compact, err := mergeSampleGroups(tracks, fills, "PCM test")
	if err != nil {
		t.Fatalf("mergeSampleGroups failed: %v", err)
	}
	if len(groups) != 1 || len(groups[0].entries) != 2 || groups[0].entries[0] != [2]uint32{10, 1} || groups[0].entries[1] != [2]uint32{15, 0} || len(compact) != 0 {
		t.Fatalf("sample groups = %v %v; want source group samples plus 15 unmapped samples", groups, compact)
	}

	second := *tracks[1]
	second.stco = []uint64{40}
	parts = []partDesc{
		{mvhdTimescale: 1000, mvhdDuration: 15, mdat: [2]uint64{0, 40}, tracks: []partTrack{*tracks[0]}},
		{mvhdTimescale: 1000, mvhdDuration: 15, mdat: [2]uint64{40, 40}, tracks: []partTrack{second}},
	}
	desc := &desc{parts: parts, movieTimescale: 1000, mdatOffset: 80}
	merged, _, err := mergeTrack(desc, 0, 1000, []uint64{0, 15}, []bool{false})
	if err != nil {
		t.Fatalf("mergeTrack failed: %v", err)
	}
	if merged.stszCount != 25 || merged.stszSampleSize != 4 || merged.mdhdDuration != 25 || len(merged.stts) != 1 || merged.stts[0] != [2]uint32{25, 1} {
		t.Fatalf("merged PCM tables: sample count=%d size=%d mdhd=%d stts=%v", merged.stszCount, merged.stszSampleSize, merged.mdhdDuration, merged.stts)
	}
	if len(merged.stco) != 3 || merged.stco[0] != 0 || merged.stco[1] != 80 || merged.stco[2] != 40 {
		t.Fatalf("merged PCM chunk offsets = %v, want source, silence, next source", merged.stco)
	}
	if len(merged.stsc) != 3 || merged.stsc[0][0] != 1 || merged.stsc[1][0] != 2 || merged.stsc[2][0] != 3 {
		t.Fatalf("merged PCM chunk map = %v", merged.stsc)
	}
	if len(desc.mdatFiller) != 1 || desc.mdatFiller[0] != (fill{length: 20, value: 0}) {
		t.Fatalf("mdat silence = %v, want 20 zero bytes", desc.mdatFiller)
	}
}

func TestGapLengthensLastCodedSample(t *testing.T) {
	track := func() *partTrack {
		return &partTrack{
			handler:       fourCC("soun"),
			mdhdTimescale: 1000,
			mdhdDuration:  10,
			stts:          [][2]uint32{{1, 10}},
			stszCount:     1,
		}
	}
	tracks := []*partTrack{track(), track()}
	parts := []partDesc{{mvhdTimescale: 1000}, {mvhdTimescale: 1000}}
	edits, whole, duration, fills, lengthens, _, err := mergeEdits(tracks, parts, 1000, 1000, 20, []uint64{0, 15}, []bool{false}, true, nil)
	if err != nil {
		t.Fatalf("mergeEdits failed: %v", err)
	}
	if !whole || duration != 25 || len(edits) != 1 || edits[0].segmentDuration != 25 || fills[0] != 0 || lengthens[0] != 5 {
		t.Fatalf("edits=%#v whole=%v duration=%d fills=%v lengthens=%v", edits, whole, duration, fills, lengthens)
	}
	runs, err := lengthenLastRun([][2]uint32{{1, 10}}, lengthens[0])
	if err != nil || len(runs) != 1 || runs[0] != [2]uint32{1, 15} {
		t.Fatalf("lengthened stts = %v, err=%v; want one 15-tick sample", runs, err)
	}
}

func TestUnfilledOneTickSoundLeavesGapOut(t *testing.T) {
	track := func() *partTrack {
		return &partTrack{
			handler:        fourCC("soun"),
			mdhdTimescale:  1000,
			mdhdDuration:   10,
			stts:           [][2]uint32{{10, 1}},
			stszCount:      10,
			stszSampleSize: 2,
		}
	}
	tracks := []*partTrack{track(), track()}
	parts := []partDesc{{mvhdTimescale: 1000}, {mvhdTimescale: 1000}}
	edits, whole, duration, fills, lengthens, _, err := mergeEdits(tracks, parts, 1000, 1000, 20, []uint64{0, 15}, []bool{false}, true, nil)
	if err != nil {
		t.Fatalf("mergeEdits failed: %v", err)
	}
	if !whole || duration != 20 || len(edits) != 1 || edits[0].segmentDuration != 20 || fills[0] != 0 || lengthens[0] != 0 {
		t.Fatalf("unfilled one-tick sound should leave the gap out: edits=%#v whole=%v duration=%d fills=%v lengthens=%v", edits, whole, duration, fills, lengthens)
	}
}

func TestJoinMatchesRustWithMultipleMdats(t *testing.T) {
	if os.Getenv("MP4MERGE_RUN_PARITY") == "" {
		t.Skip("set MP4MERGE_RUN_PARITY=1 to enable parity tests")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not available, skipping parity test")
	}
	rustDir := os.Getenv("MP4MERGE_RUST_DIR")
	if rustDir == "" {
		t.Skip("set MP4MERGE_RUST_DIR to the Rust mp4-merge checkout to enable parity tests")
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.mp4"), filepath.Join(dir, "b.mp4")
	outGo, outRust := filepath.Join(dir, "go.mp4"), filepath.Join(dir, "rust.mp4")
	s1 := testFileSpec{trackID: 1, sampleSizes: []uint32{3, 4}, sampleDelta: 1000, keyframes: []uint32{1}}
	s2 := testFileSpec{trackID: 1, sampleSizes: []uint32{2, 5}, sampleDelta: 1000, keyframes: []uint32{1}}
	if err := os.WriteFile(a, buildTestMP4WithTwoMdats(s1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, buildTestMP4WithTwoMdats(s2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := JoinFiles([]string{a, b}, outGo, nil); err != nil {
		t.Fatal(err)
	}
	cargo := exec.Command("cargo", "run", "--quiet", "--", "--out", outRust, a, b)
	cargo.Dir = rustDir
	var stdout, stderr bytes.Buffer
	cargo.Stdout = &stdout
	cargo.Stderr = &stderr
	if err := cargo.Run(); err != nil {
		t.Fatalf("cargo run failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	goBytes, err := os.ReadFile(outGo)
	if err != nil {
		t.Fatal(err)
	}
	rustBytes, err := os.ReadFile(outRust)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(goBytes, rustBytes) {
		t.Fatalf("multi-mdat parity mismatch: go=%d bytes rust=%d bytes", len(goBytes), len(rustBytes))
	}
}

func TestJoinValidationErrors(t *testing.T) {
	if err := JoinFiles(nil, "noop", nil); err == nil {
		t.Fatalf("expected error when no input files are provided")
	}

	var out io.ReadWriteSeeker
	if err := JoinFileStreams(nil, out, nil); err == nil {
		t.Fatalf("expected error when no streams are provided")
	}

	tmp, err := os.CreateTemp("", "mp4merge-output-*.mp4")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	if err := JoinFileStreams([]Stream{{Reader: nil, Size: 123}}, tmp, nil); err == nil {
		t.Fatalf("expected error when stream has nil reader")
	}
}

func TestJoinFileStreamsWithCreatesOutputAfterValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "merged.mp4")
	data := buildTestMP4(testFileSpec{trackID: 1, sampleSizes: []uint32{2}, sampleDelta: 1000, keyframes: []uint32{1}})
	created := false
	output, err := JoinFileStreamsWith([]Stream{{Reader: bytes.NewReader(data), Size: int64(len(data))}}, func() (io.WriteSeeker, error) {
		created = true
		return os.Create(path)
	}, nil)
	if err != nil {
		t.Fatalf("JoinFileStreamsWith failed: %v", err)
	}
	if !created {
		t.Fatal("output factory was not called")
	}
	file, ok := output.(*os.File)
	if !ok {
		t.Fatalf("returned writer type = %T, want *os.File", output)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close returned output: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("merged output missing or empty: info=%v err=%v", info, err)
	}

	invalidCalled := false
	_, err = JoinFileStreamsWith([]Stream{{Reader: bytes.NewReader(make([]byte, 40)), Size: 40}}, func() (io.WriteSeeker, error) {
		invalidCalled = true
		return nil, nil
	}, nil)
	if err == nil {
		t.Fatal("expected invalid input to be rejected")
	}
	if invalidCalled {
		t.Fatal("output factory was called before input validation completed")
	}
}

func TestJoinFileStreamsWithClosesOutputAfterWriteFailure(t *testing.T) {
	data := buildTestMP4(testFileSpec{trackID: 1, sampleSizes: []uint32{2}, sampleDelta: 1000, keyframes: []uint32{1}})
	output := &failingOutput{writeErr: io.ErrClosedPipe}
	returned, err := JoinFileStreamsWith([]Stream{{Reader: bytes.NewReader(data), Size: int64(len(data))}}, func() (io.WriteSeeker, error) {
		return output, nil
	}, nil)
	if err == nil {
		t.Fatal("expected merge write failure")
	}
	if returned != nil {
		t.Fatalf("returned output = %T, want nil on failure", returned)
	}
	if !output.closed {
		t.Fatal("output closer was not called after merge failure")
	}
}

func TestJoinPreservesSDTPFromLaterInput(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.mp4")
	second := filepath.Join(dir, "second.mp4")
	output := filepath.Join(dir, "merged.mp4")
	writeTestMP4(t, first, testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{2, 2},
		sampleDelta: 1000,
		keyframes:   []uint32{1},
	})
	writeTestMP4(t, second, testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{2, 2},
		sampleDelta: 1000,
		keyframes:   []uint32{1},
		sdtp:        []byte{0x12, 0x34},
	})
	if err := JoinFiles([]string{first, second}, output, nil); err != nil {
		t.Fatalf("JoinFiles failed: %v", err)
	}

	d := loadDescFromFile(t, output)
	if len(d.parts) != 1 {
		t.Fatalf("merged description has %d parts, want 1", len(d.parts))
	}
	if len(d.parts[0].tracks) != 1 {
		t.Fatalf("merged part has %d tracks, want 1", len(d.parts[0].tracks))
	}
	track := d.parts[0].tracks[0]
	if !track.hasSDTP {
		t.Fatal("merged track has no sdtp table")
	}
	want := []byte{0, 0, 0x12, 0x34}
	if !bytes.Equal(track.sdtp, want) {
		t.Fatalf("merged sdtp = %x, want %x", track.sdtp, want)
	}
}

func TestReadBoxValidatesSizeAndReturnsPosition(t *testing.T) {
	data := make([]byte, 16)
	binary.BigEndian.PutUint32(data[:4], 16)
	binary.BigEndian.PutUint32(data[4:8], fourCC("free"))
	reader := bytes.NewReader(data)
	typ, offset, size, header, err := ReadBox(reader)
	if err != nil {
		t.Fatalf("ReadBox failed: %v", err)
	}
	if typ != fourCC("free") || offset != 0 || size != 16 || header != 8 {
		t.Fatalf("ReadBox = (%q, %d, %d, %d), want (free, 0, 16, 8)", typeToString(typ), offset, size, header)
	}
	binary.BigEndian.PutUint32(data[:4], 4)
	if _, _, _, _, err := ReadBox(bytes.NewReader(data)); err == nil {
		t.Fatal("expected a box smaller than its header to be rejected")
	}
}

func TestJoinMatchesRust(t *testing.T) {
	if os.Getenv("MP4MERGE_RUN_PARITY") == "" {
		t.Skip("set MP4MERGE_RUN_PARITY=1 to enable parity tests")
	}
	runParityTest(t, testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{5, 7, 6},
		sampleDelta: 1000,
		keyframes:   []uint32{1, 3},
	}, testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{4, 6},
		sampleDelta: 750,
		keyframes:   []uint32{1},
	})
}

func TestJoinMatchesRustWithInsta(t *testing.T) {
	if os.Getenv("MP4MERGE_RUN_PARITY") == "" {
		t.Skip("set MP4MERGE_RUN_PARITY=1 to enable parity tests")
	}
	runParityTest(t, testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{5, 5, 5},
		sampleDelta: 900,
		keyframes:   []uint32{1},
		insta: &instaSpec{
			version: 4,
			entries: []instaEntrySpec{
				{id: 10, format: 1, payload: []byte("camera-1-meta-frame-a")},
				{id: 11, format: 2, payload: []byte{1, 2, 3, 4, 5, 6}},
			},
		},
	}, testFileSpec{
		trackID:     1,
		sampleSizes: []uint32{6, 6},
		sampleDelta: 600,
		keyframes:   []uint32{1, 2},
		insta: &instaSpec{
			version: 4,
			entries: []instaEntrySpec{
				{id: 10, format: 1, payload: []byte("camera-2-meta-frame-b")},
				{id: 12, format: 3, payload: []byte{9, 8, 7, 6}},
			},
		},
	})
}

func TestJoinMatchesRustWithPCMSilenceGap(t *testing.T) {
	if os.Getenv("MP4MERGE_RUN_PARITY") == "" {
		t.Skip("set MP4MERGE_RUN_PARITY=1 to enable parity tests")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not available, skipping parity test")
	}
	rustDir := os.Getenv("MP4MERGE_RUST_DIR")
	if rustDir == "" {
		t.Skip("set MP4MERGE_RUST_DIR to the Rust mp4-merge checkout to enable parity tests")
	}
	dir := t.TempDir()
	inputs := []string{filepath.Join(dir, "clip1.mp4"), filepath.Join(dir, "clip2.mp4")}
	outputGo, outputRust := filepath.Join(dir, "merged-go.mp4"), filepath.Join(dir, "merged-rust.mp4")
	writeTestPCMGapMP4(t, inputs[0], []uint32{3, 3, 3}, 5, 10, 1, 100)
	writeTestPCMGapMP4(t, inputs[1], []uint32{3, 3}, 5, 10, 1, 200)
	if err := JoinFiles(inputs, outputGo, nil); err != nil {
		t.Fatalf("Go JoinFiles failed: %v", err)
	}
	cargo := exec.Command("cargo", "run", "--quiet", "--", "--out", outputRust, inputs[0], inputs[1])
	cargo.Dir = rustDir
	var stdout, stderr bytes.Buffer
	cargo.Stdout, cargo.Stderr = &stdout, &stderr
	if err := cargo.Run(); err != nil {
		t.Fatalf("cargo run failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	goBytes, err := os.ReadFile(outputGo)
	if err != nil {
		t.Fatalf("read Go output: %v", err)
	}
	rustBytes, err := os.ReadFile(outputRust)
	if err != nil {
		t.Fatalf("read Rust output: %v", err)
	}
	if !bytes.Equal(goBytes, rustBytes) {
		at := 0
		for at < len(goBytes) && at < len(rustBytes) && goBytes[at] == rustBytes[at] {
			at++
		}
		t.Fatalf("PCM gap parity mismatch: Go len=%d Rust len=%d first diff=%d", len(goBytes), len(rustBytes), at)
	}
}

func TestJoinMatchesRustWithSoundOverlap(t *testing.T) {
	if os.Getenv("MP4MERGE_RUN_PARITY") == "" {
		t.Skip("set MP4MERGE_RUN_PARITY=1 to enable parity tests")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not available, skipping parity test")
	}
	rustDir := os.Getenv("MP4MERGE_RUST_DIR")
	if rustDir == "" {
		t.Skip("set MP4MERGE_RUST_DIR to the Rust mp4-merge checkout to enable parity tests")
	}
	dir := t.TempDir()
	inputs := []string{filepath.Join(dir, "clip1.mp4"), filepath.Join(dir, "clip2.mp4")}
	outputGo, outputRust := filepath.Join(dir, "merged-go.mp4"), filepath.Join(dir, "merged-rust.mp4")
	writeTestSoundOverlapMP4(t, inputs[0], 100, true)
	writeTestSoundOverlapMP4(t, inputs[1], 200, false)
	if err := JoinFiles(inputs, outputGo, nil); err != nil {
		t.Fatalf("Go JoinFiles failed: %v", err)
	}
	cargo := exec.Command("cargo", "run", "--quiet", "--", "--out", outputRust, inputs[0], inputs[1])
	cargo.Dir = rustDir
	var stdout, stderr bytes.Buffer
	cargo.Stdout, cargo.Stderr = &stdout, &stderr
	if err := cargo.Run(); err != nil {
		t.Fatalf("cargo run failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	goBytes, err := os.ReadFile(outputGo)
	if err != nil {
		t.Fatalf("read Go output: %v", err)
	}
	rustBytes, err := os.ReadFile(outputRust)
	if err != nil {
		t.Fatalf("read Rust output: %v", err)
	}
	if !bytes.Equal(goBytes, rustBytes) {
		at := 0
		for at < len(goBytes) && at < len(rustBytes) && goBytes[at] == rustBytes[at] {
			at++
		}
		lo, goHi, rustHi := at-16, at+32, at+32
		if lo < 0 {
			lo = 0
		}
		if goHi > len(goBytes) {
			goHi = len(goBytes)
		}
		if rustHi > len(rustBytes) {
			rustHi = len(rustBytes)
		}
		t.Fatalf("sound overlap parity mismatch: Go len=%d Rust len=%d first diff=%d\nGo:   %x\nRust: %x", len(goBytes), len(rustBytes), at, goBytes[lo:goHi], rustBytes[lo:rustHi])
	}
}

func writeTestPCMGapMP4(t *testing.T, path string, videoSizes []uint32, videoDelta, audioSamples, audioDelta, creation uint32) {
	t.Helper()
	ftyp := makeFTYP()
	videoData := makeSampleData(videoSizes, 0x30)
	audioData := bytes.Repeat([]byte{0x40}, int(audioSamples*4))
	mdatPayload := append(append([]byte(nil), videoData...), audioData...)
	mdat := makeMDAT(mdatPayload)
	dataOffset := len(ftyp) + 8
	videoDuration := uint32(len(videoSizes)) * videoDelta
	audioDuration := audioSamples * audioDelta
	videoTrack := makeTestTrackBox(1, "vide", videoDuration, uint32(len(videoSizes)), videoDelta, videoSizes, 0, uint32(dataOffset), makeSTSD(1920, 1080), makeVMHD())
	audioSizes := make([]uint32, audioSamples)
	for i := range audioSizes {
		audioSizes[i] = 4
	}
	audioTrack := makeTestTrackBox(2, "soun", audioDuration, audioSamples, audioDelta, audioSizes, 4, uint32(dataOffset+len(videoData)), makePCMSTSD(), makeSMHD())
	duration := videoDuration
	if audioDuration > duration {
		duration = audioDuration
	}
	mvhd := makeMVHD(1000, duration, 3)
	binary.BigEndian.PutUint32(mvhd[12:16], creation)
	moov := makeContainer("moov", mvhd, videoTrack, audioTrack)
	data := append(append(append([]byte(nil), ftyp...), mdat...), moov...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeTestSoundOverlapMP4(t *testing.T, path string, creation uint32, leadingEmpty bool) {
	t.Helper()
	ftyp := makeFTYP()
	videoSizes := []uint32{2, 2}
	audioSizes := []uint32{2, 2, 2}
	videoData := makeSampleData(videoSizes, 0x30)
	audioData := makeSampleData(audioSizes, 0x40)
	mdat := makeMDAT(append(videoData, audioData...))
	dataOffset := uint32(len(ftyp) + 8)
	videoTrack := makeTestTrackBox(1, "vide", 2000, 2, 1000, videoSizes, 0, dataOffset, makeSTSD(1920, 1080), makeVMHD())
	var audioEdits []edit
	if leadingEmpty {
		audioEdits = []edit{{segmentDuration: 1000, mediaTime: -1, mediaRate: mediaRateOne}, {segmentDuration: 3000, mediaTime: 0, mediaRate: mediaRateOne}}
	}
	audioTrack := makeTestTrackBox(2, "soun", 3000, 3, 1000, audioSizes, 0, dataOffset+uint32(len(videoData)), makeAudioTestSTSD(), makeSMHD(), audioEdits...)
	mvhd := makeMVHD(1000, 3000, 3)
	binary.BigEndian.PutUint32(mvhd[12:16], creation)
	moov := makeContainer("moov", mvhd, videoTrack, audioTrack)
	data := append(append(append([]byte(nil), ftyp...), mdat...), moov...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func makeAudioTestSTSD() []byte {
	body := &bytes.Buffer{}
	body.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(body, binary.BigEndian, uint32(1))
	entry := make([]byte, 36)
	binary.BigEndian.PutUint32(entry[:4], uint32(len(entry)))
	binary.BigEndian.PutUint32(entry[4:8], fourCC("mp4a"))
	binary.BigEndian.PutUint16(entry[14:16], 1)
	binary.BigEndian.PutUint16(entry[24:26], 2)
	binary.BigEndian.PutUint16(entry[26:28], 16)
	binary.BigEndian.PutUint32(entry[32:36], 48000<<16)
	body.Write(entry)
	return makeBox("stsd", body.Bytes())
}

func makeTestTrackBox(trackID uint32, handler string, duration, sampleCount, delta uint32, sampleSizes []uint32, fixedSampleSize, chunkOffset uint32, stsd, mediaHeader []byte, editList ...edit) []byte {
	tkhd := makeTKHD(trackID, duration, 1920, 1080)
	mdhd := makeMDHD(1000, duration)
	stblChildren := [][]byte{stsd, makeSTTS(sampleCount, delta), makeSTSC(1, sampleCount, 1)}
	if fixedSampleSize != 0 {
		stblChildren = append(stblChildren, makeSTSZFixed(fixedSampleSize, sampleCount))
	} else {
		stblChildren = append(stblChildren, makeSTSZ(sampleSizes))
	}
	stblChildren = append(stblChildren, makeSTCO(chunkOffset))
	stbl := makeContainer("stbl", stblChildren...)
	minf := makeContainer("minf", mediaHeader, makeDINF(), stbl)
	mdia := makeContainer("mdia", mdhd, makeHDLR(handler), minf)
	elst := makeELST(duration)
	if len(editList) > 0 {
		body := &bytes.Buffer{}
		body.Write([]byte{0, 0, 0, 0})
		_ = binary.Write(body, binary.BigEndian, uint32(len(editList)))
		for _, edit := range editList {
			_ = binary.Write(body, binary.BigEndian, uint32(edit.segmentDuration))
			_ = binary.Write(body, binary.BigEndian, int32(edit.mediaTime))
			_ = binary.Write(body, binary.BigEndian, uint16(uint32(edit.mediaRate)>>16))
			_ = binary.Write(body, binary.BigEndian, uint16(edit.mediaRate))
		}
		elst = makeBox("elst", body.Bytes())
	}
	return makeContainer("trak", tkhd, makeContainer("edts", elst), mdia)
}

func makePCMSTSD() []byte {
	body := &bytes.Buffer{}
	body.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(body, binary.BigEndian, uint32(1))
	entry := make([]byte, 36)
	binary.BigEndian.PutUint32(entry[:4], uint32(len(entry)))
	binary.BigEndian.PutUint32(entry[4:8], fourCC("sowt"))
	binary.BigEndian.PutUint16(entry[14:16], 1)
	binary.BigEndian.PutUint16(entry[24:26], 2)
	binary.BigEndian.PutUint16(entry[26:28], 16)
	binary.BigEndian.PutUint32(entry[32:36], 1000<<16)
	body.Write(entry)
	return makeBox("stsd", body.Bytes())
}

func makeSTSZFixed(size, count uint32) []byte {
	body := &bytes.Buffer{}
	body.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(body, binary.BigEndian, size)
	_ = binary.Write(body, binary.BigEndian, count)
	return makeBox("stsz", body.Bytes())
}

func makeSMHD() []byte {
	return makeBox("smhd", []byte{0, 0, 0, 0, 0, 0, 0, 0})
}

func runParityTest(t *testing.T, spec1, spec2 testFileSpec) {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not available, skipping parity test")
	}

	dir := t.TempDir()

	input1 := filepath.Join(dir, "clip1.mp4")
	input2 := filepath.Join(dir, "clip2.mp4")
	outputGo := filepath.Join(dir, "merged-go.mp4")
	outputRust := filepath.Join(dir, "merged-rust.mp4")

	writeTestMP4(t, input1, spec1)
	writeTestMP4(t, input2, spec2)

	if err := JoinFiles([]string{input1, input2}, outputGo, nil); err != nil {
		t.Fatalf("JoinFiles failed: %v", err)
	}

	repoRoot := os.Getenv("MP4MERGE_RUST_DIR")
	if repoRoot == "" {
		t.Skip("set MP4MERGE_RUST_DIR to the Rust mp4-merge checkout to enable parity tests")
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "Cargo.toml")); err != nil {
		t.Fatalf("MP4MERGE_RUST_DIR %q is not a Rust checkout: %v", repoRoot, err)
	}

	cargo := exec.Command("cargo", "run", "--quiet", "--", "--out", outputRust, input1, input2)
	cargo.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cargo.Stdout = &stdout
	cargo.Stderr = &stderr
	if err := cargo.Run(); err != nil {
		t.Fatalf("cargo run failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	goBytes, err := os.ReadFile(outputGo)
	if err != nil {
		t.Fatalf("read go output: %v", err)
	}
	rustBytes, err := os.ReadFile(outputRust)
	if err != nil {
		t.Fatalf("read rust output: %v", err)
	}

	if !bytes.Equal(goBytes, rustBytes) {
		at := 0
		for at < len(goBytes) && at < len(rustBytes) && goBytes[at] == rustBytes[at] {
			at++
		}
		lo, hi := at-16, at+32
		if lo < 0 {
			lo = 0
		}
		if hi > len(goBytes) {
			hi = len(goBytes)
		}
		if hi > len(rustBytes) {
			hi = len(rustBytes)
		}
		t.Fatalf("merged output mismatch: go len=%d rust len=%d first diff=%d go=%x rust=%x", len(goBytes), len(rustBytes), at, goBytes[lo:hi], rustBytes[lo:hi])
	}
}

func loadDescFromFile(t *testing.T, path string) *desc {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	d, _, _, _, err := prepareInputs([]Stream{{Reader: f, Size: info.Size()}}, nil)
	if err != nil {
		t.Fatalf("read merged description: %v", err)
	}
	return d
}

func readTail(t *testing.T, path string, n int64) []byte {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if n > info.Size() {
		n = info.Size()
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, info.Size()-n); err != nil {
		t.Fatalf("read tail: %v", err)
	}
	return buf
}

type testFileSpec struct {
	trackID     uint32
	sampleSizes []uint32
	sampleDelta uint32
	keyframes   []uint32
	sdtp        []byte
	insta       *instaSpec
}

type failingOutput struct {
	closed   bool
	writeErr error
}

func (w *failingOutput) Write([]byte) (int, error)      { return 0, w.writeErr }
func (w *failingOutput) Seek(int64, int) (int64, error) { return 0, nil }
func (w *failingOutput) Close() error                   { w.closed = true; return nil }

func writeTestMP4(t *testing.T, path string, spec testFileSpec) {
	t.Helper()
	data := buildTestMP4(spec)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func buildTestMP4(spec testFileSpec) []byte {
	ftyp := makeFTYP()
	sampleData := makeSampleData(spec.sampleSizes, byte(spec.trackID))
	mdat := makeMDAT(sampleData)

	totalSamples := uint32(len(spec.sampleSizes))
	totalDuration := spec.sampleDelta * totalSamples

	moov := makeMOOV(spec, totalDuration, totalSamples, len(ftyp)+8)

	file := append([]byte{}, ftyp...)
	file = append(file, mdat...)
	file = append(file, moov...)

	if spec.insta != nil {
		file = append(file, makeInstaMetadata(spec.insta)...)
	}
	return file
}

func buildTestMP4WithTwoMdats(spec testFileSpec) []byte {
	ftyp := makeFTYP()
	unused := makeMDAT([]byte("unused-mdat"))
	sampleData := makeSampleData(spec.sampleSizes, byte(spec.trackID))
	mdat := makeMDAT(sampleData)
	duration := spec.sampleDelta * uint32(len(spec.sampleSizes))
	offset := len(ftyp) + len(unused) + 8
	moov := makeMOOV(spec, duration, uint32(len(spec.sampleSizes)), offset)
	data := append([]byte{}, ftyp...)
	data = append(data, unused...)
	data = append(data, mdat...)
	data = append(data, moov...)
	if spec.insta != nil {
		data = append(data, makeInstaMetadata(spec.insta)...)
	}
	return data
}

func makeFTYP() []byte {
	payload := &bytes.Buffer{}
	payload.WriteString("isom")
	_ = binary.Write(payload, binary.BigEndian, uint32(0x00000200))
	payload.WriteString("isom")
	payload.WriteString("iso2")
	return makeBox("ftyp", payload.Bytes())
}

func makeMDAT(data []byte) []byte {
	return makeBox("mdat", data)
}

func makeMOOV(spec testFileSpec, duration uint32, sampleCount uint32, chunkOffset int) []byte {
	mvhd := makeMVHD(1000, duration, spec.trackID+1)
	tkhd := makeTKHD(spec.trackID, duration, 1920, 1080)
	elst := makeELST(duration)
	mdhd := makeMDHD(1000, duration)
	hdlr := makeHDLR("vide")
	stsd := makeSTSD(1920, 1080)
	stts := makeSTTS(sampleCount, spec.sampleDelta)
	stsc := makeSTSC(1, sampleCount, 1)
	stsz := makeSTSZ(spec.sampleSizes)
	stco := makeSTCO(uint32(chunkOffset))
	stss := makeSTSS(spec.keyframes)

	stblChildren := [][]byte{stsd, stts, stsc, stsz, stco}
	if len(spec.sdtp) > 0 {
		stblChildren = append(stblChildren, makeSDTP(spec.sdtp))
	}
	if len(spec.keyframes) > 0 {
		stblChildren = append(stblChildren, stss)
	}
	stbl := makeContainer("stbl", stblChildren...)
	vmhd := makeVMHD()
	dinf := makeDINF()
	minf := makeContainer("minf", vmhd, dinf, stbl)
	mdia := makeContainer("mdia", mdhd, hdlr, minf)
	edts := makeContainer("edts", elst)
	trak := makeContainer("trak", tkhd, edts, mdia)
	return makeContainer("moov", mvhd, trak)
}

func makeMVHD(timescale, duration, nextTrackID uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	buf.Write(make([]byte, 8)) // creation + modification
	_ = binary.Write(buf, binary.BigEndian, timescale)
	_ = binary.Write(buf, binary.BigEndian, duration)
	_ = binary.Write(buf, binary.BigEndian, uint32(0x00010000)) // rate 1.0
	_ = binary.Write(buf, binary.BigEndian, uint16(0x0100))     // volume 1.0
	buf.Write(make([]byte, 10))                                 // reserved
	buf.Write(identityMatrix())
	buf.Write(make([]byte, 24)) // pre_defined
	_ = binary.Write(buf, binary.BigEndian, nextTrackID)
	return makeBox("mvhd", buf.Bytes())
}

func makeTKHD(trackID, duration uint32, width, height uint16) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0x00, 0x00, 0x07}) // flags
	buf.Write(make([]byte, 8))          // creation + modification
	_ = binary.Write(buf, binary.BigEndian, trackID)
	buf.Write(make([]byte, 4)) // reserved
	_ = binary.Write(buf, binary.BigEndian, duration)
	buf.Write(make([]byte, 8))                              // reserved
	buf.Write(make([]byte, 2))                              // layer
	buf.Write(make([]byte, 2))                              // alternate group
	_ = binary.Write(buf, binary.BigEndian, uint16(0x0100)) // volume
	buf.Write(make([]byte, 2))                              // reserved
	buf.Write(identityMatrix())
	_ = binary.Write(buf, binary.BigEndian, uint32(width)<<16)
	_ = binary.Write(buf, binary.BigEndian, uint32(height)<<16)
	return makeBox("tkhd", buf.Bytes())
}

func makeELST(duration uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	_ = binary.Write(buf, binary.BigEndian, uint32(1)) // entry count
	_ = binary.Write(buf, binary.BigEndian, duration)
	_ = binary.Write(buf, binary.BigEndian, int32(0))
	_ = binary.Write(buf, binary.BigEndian, uint16(1)) // rate 1.0
	_ = binary.Write(buf, binary.BigEndian, uint16(0))
	return makeBox("elst", buf.Bytes())
}

func makeMDHD(timescale, duration uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	buf.Write(make([]byte, 8)) // creation + modification
	_ = binary.Write(buf, binary.BigEndian, timescale)
	_ = binary.Write(buf, binary.BigEndian, duration)
	_ = binary.Write(buf, binary.BigEndian, uint16(0)) // language
	_ = binary.Write(buf, binary.BigEndian, uint16(0))
	return makeBox("mdhd", buf.Bytes())
}

func makeHDLR(handler string) []byte {
	buf := &bytes.Buffer{}
	buf.Write([]byte{0, 0, 0, 0})
	buf.Write(make([]byte, 4)) // pre_defined
	buf.WriteString(handler)
	buf.Write(make([]byte, 12))
	buf.WriteString("VideoHandler")
	buf.WriteByte(0)
	return makeBox("hdlr", buf.Bytes())
}

func makeSTSD(width, height uint16) []byte {
	buf := &bytes.Buffer{}
	buf.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(buf, binary.BigEndian, uint32(1))

	entry := &bytes.Buffer{}
	entry.Write(make([]byte, 4)) // size placeholder
	entry.WriteString("avc1")
	entry.Write(make([]byte, 6))
	_ = binary.Write(entry, binary.BigEndian, uint16(1))
	entry.Write(make([]byte, 16))
	_ = binary.Write(entry, binary.BigEndian, width)
	_ = binary.Write(entry, binary.BigEndian, height)
	_ = binary.Write(entry, binary.BigEndian, uint32(0x00480000))
	_ = binary.Write(entry, binary.BigEndian, uint32(0x00480000))
	_ = binary.Write(entry, binary.BigEndian, uint32(0))
	_ = binary.Write(entry, binary.BigEndian, uint16(1))
	entry.Write(make([]byte, 32))
	_ = binary.Write(entry, binary.BigEndian, uint16(0x0018))
	_ = binary.Write(entry, binary.BigEndian, uint16(0xffff))

	entryBytes := entry.Bytes()
	binary.BigEndian.PutUint32(entryBytes[:4], uint32(len(entryBytes)))
	buf.Write(entryBytes)

	return makeBox("stsd", buf.Bytes())
}

func makeSTTS(count, delta uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	_ = binary.Write(buf, binary.BigEndian, uint32(1))
	_ = binary.Write(buf, binary.BigEndian, count)
	_ = binary.Write(buf, binary.BigEndian, delta)
	return makeBox("stts", buf.Bytes())
}

func makeSTSC(firstChunk, samplesPerChunk, sampleDescIndex uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	_ = binary.Write(buf, binary.BigEndian, uint32(1))
	_ = binary.Write(buf, binary.BigEndian, firstChunk)
	_ = binary.Write(buf, binary.BigEndian, samplesPerChunk)
	_ = binary.Write(buf, binary.BigEndian, sampleDescIndex)
	return makeBox("stsc", buf.Bytes())
}

func makeSTSZ(sizes []uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	_ = binary.Write(buf, binary.BigEndian, uint32(len(sizes)))
	for _, sz := range sizes {
		_ = binary.Write(buf, binary.BigEndian, sz)
	}
	return makeBox("stsz", buf.Bytes())
}

func makeSTCO(offset uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	_ = binary.Write(buf, binary.BigEndian, uint32(1))
	_ = binary.Write(buf, binary.BigEndian, offset)
	return makeBox("stco", buf.Bytes())
}

func makeSTSS(entries []uint32) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	_ = binary.Write(buf, binary.BigEndian, uint32(len(entries)))
	for _, e := range entries {
		_ = binary.Write(buf, binary.BigEndian, e)
	}
	return makeBox("stss", buf.Bytes())
}

func makeSDTP(entries []byte) []byte {
	body := append([]byte{0, 0, 0, 0}, entries...)
	return makeBox("sdtp", body)
}

func makeVMHD() []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 1}) // graphics mode=0, flags=1
	buf.Write(make([]byte, 8))
	return makeBox("vmhd", buf.Bytes())
}

func makeDINF() []byte {
	url := makeBox("url ", []byte{0, 0, 0, 1})
	payload := &bytes.Buffer{}
	payload.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(payload, binary.BigEndian, uint32(1))
	payload.Write(url)
	dref := makeBox("dref", payload.Bytes())
	return makeContainer("dinf", dref)
}

func makeSampleData(sizes []uint32, seed byte) []byte {
	data := make([]byte, 0, sumSizes(sizes))
	value := seed
	for _, sz := range sizes {
		for i := uint32(0); i < sz; i++ {
			data = append(data, value)
			value++
		}
	}
	return data
}

type instaSpec struct {
	version uint32
	entries []instaEntrySpec
}

type instaEntrySpec struct {
	id      uint8
	format  uint8
	payload []byte
}

func makeInstaMetadata(spec *instaSpec) []byte {
	if spec == nil || len(spec.entries) == 0 {
		return nil
	}
	version := spec.version
	if version == 0 {
		version = 3
	}

	buf := &bytes.Buffer{}
	totalSize := 0
	for _, entry := range spec.entries {
		buf.Write(entry.payload)
		if err := buf.WriteByte(entry.format); err != nil {
			panic(err)
		}
		if err := buf.WriteByte(entry.id); err != nil {
			panic(err)
		}
		size := uint32(len(entry.payload))
		if err := binary.Write(buf, binary.LittleEndian, size); err != nil {
			panic(err)
		}
		totalSize += len(entry.payload) + 6
	}

	// Padding + footer
	buf.Write(make([]byte, 16))
	buf.Write(make([]byte, 16))
	if err := binary.Write(buf, binary.LittleEndian, uint32(totalSize+72)); err != nil {
		panic(err)
	}
	if err := binary.Write(buf, binary.LittleEndian, version); err != nil {
		panic(err)
	}
	if _, err := buf.Write(instaMagic); err != nil {
		panic(err)
	}

	return buf.Bytes()
}

func identityMatrix() []byte {
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.BigEndian, uint32(0x00010000))
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	_ = binary.Write(buf, binary.BigEndian, uint32(0x00010000))
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	_ = binary.Write(buf, binary.BigEndian, uint32(0x40000000))
	return buf.Bytes()
}

func makeContainer(typ string, children ...[]byte) []byte {
	payload := bytes.Join(children, nil)
	return makeBox(typ, payload)
}

func makeBox(typ string, payload []byte) []byte {
	size := uint32(len(payload) + 8)
	buf := make([]byte, 8, size)
	binary.BigEndian.PutUint32(buf[:4], size)
	copy(buf[4:8], typ)
	return append(buf, payload...)
}

func sumSizes(values []uint32) int {
	total := 0
	for _, v := range values {
		total += int(v)
	}
	return total
}
