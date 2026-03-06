package mp4merge

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func BenchmarkJoinWriter(b *testing.B) {
	for _, tc := range []struct {
		name     string
		buffered bool
	}{
		{name: "direct", buffered: false},
		{name: "buffered", buffered: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkJoinWriter(b, tc.buffered)
		})
	}
}

func benchmarkJoinWriter(b *testing.B, buffered bool) {
	dir := b.TempDir()
	specs := []testFileSpec{
		makeBenchmarkSpec(1, 5000, 2048),
		makeBenchmarkSpec(1, 5000, 2048),
		makeBenchmarkSpec(1, 5000, 2048),
	}

	inputPaths := make([]string, len(specs))
	var totalInputBytes int64
	for i, spec := range specs {
		path := filepath.Join(dir, "clip"+strconv.Itoa(i+1)+".mp4")
		if err := os.WriteFile(path, buildTestMP4(spec), 0o644); err != nil {
			b.Fatalf("write %s: %v", path, err)
		}
		inputPaths[i] = path
		info, err := os.Stat(path)
		if err != nil {
			b.Fatalf("stat %s: %v", path, err)
		}
		totalInputBytes += info.Size()
	}

	b.ReportAllocs()
	b.SetBytes(totalInputBytes)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		streams := make([]Stream, 0, len(inputPaths))
		inputs := make([]*os.File, 0, len(inputPaths))
		for _, path := range inputPaths {
			f, err := os.Open(path)
			if err != nil {
				b.Fatalf("open %s: %v", path, err)
			}
			info, err := f.Stat()
			if err != nil {
				_ = f.Close()
				b.Fatalf("stat %s: %v", path, err)
			}
			inputs = append(inputs, f)
			streams = append(streams, Stream{Reader: f, Size: info.Size()})
		}

		outputPath := filepath.Join(dir, "bench-output.mp4")
		out, err := os.Create(outputPath)
		if err != nil {
			closeAll(inputs)
			b.Fatalf("create output: %v", err)
		}

		var joinErr error
		if buffered {
			bw := newBufferedWriteSeeker(out, 64*1024)
			joinErr = JoinFileStreams(streams, bw, nil)
			if joinErr == nil {
				joinErr = bw.Flush()
			}
		} else {
			joinErr = JoinFileStreams(streams, out, nil)
		}

		closeErr := out.Close()
		closeAll(inputs)
		_ = os.Remove(outputPath)

		if joinErr != nil {
			b.Fatalf("join failed: %v", joinErr)
		}
		if closeErr != nil {
			b.Fatalf("close output: %v", closeErr)
		}
	}
}

func makeBenchmarkSpec(trackID uint32, samples int, sampleSize uint32) testFileSpec {
	sampleSizes := make([]uint32, samples)
	for i := range sampleSizes {
		sampleSizes[i] = sampleSize
	}
	return testFileSpec{
		trackID:     trackID,
		sampleSizes: sampleSizes,
		sampleDelta: 1000,
		keyframes:   []uint32{1},
	}
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
