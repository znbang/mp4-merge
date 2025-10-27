package mp4merge

import (
	"bytes"
	"encoding/binary"
	"io"
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
	if desc.moovMvhdDuration != expectedDuration {
		t.Fatalf("mvhd duration = %d, want %d", desc.moovMvhdDuration, expectedDuration)
	}

	var track *trackDesc
	for _, candidate := range desc.moovTracks {
		if candidate != nil && candidate.stszCount > 0 {
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

	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
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
		t.Fatalf("merged output mismatch: go len=%d rust len=%d", len(goBytes), len(rustBytes))
	}
}

func loadDescFromFile(t *testing.T, path string) *desc {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	d := newDesc()

	for {
		typ, _, size, headerSize, err := readBox(f)
		if err != nil {
			t.Fatalf("locate mdat: %v", err)
		}
		pos, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			t.Fatalf("record offset: %v", err)
		}
		if typ == fourCC("mdat") {
			dataSize := size
			if dataSize >= uint64(headerSize) {
				dataSize -= uint64(headerSize)
			} else {
				dataSize = 0
			}
			d.mdatPosition = append(d.mdatPosition, mdatPosition{
				fileIndex: -1,
				offset:    uint64(pos),
				size:      dataSize,
			})
			d.mdatFinalPosition = uint64(pos)
			break
		}
		if _, err := f.Seek(int64(pos)+int64(size)-headerSize, io.SeekStart); err != nil {
			t.Fatalf("skip box: %v", err)
		}
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind file: %v", err)
	}

	if err := readDesc(f, d, 0, ^uint64(0), 0); err != nil {
		t.Fatalf("read final desc: %v", err)
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
	insta       *instaSpec
}

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
