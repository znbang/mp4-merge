package mp4merge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// JoinFiles merges input MP4 files into a single output file path.
func JoinFiles(paths []string, outputPath string, progress func(float64)) error {
	if len(paths) == 0 {
		return errors.New("mp4merge: no input files provided")
	}

	inputs := make([]*os.File, 0, len(paths))
	handles := make([]Stream, 0, len(paths))
	defer func() {
		for _, f := range inputs {
			_ = f.Close()
		}
	}()

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("mp4merge: open %s: %w", path, err)
		}
		stat, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("mp4merge: stat %s: %w", path, err)
		}
		inputs = append(inputs, f)
		handles = append(handles, Stream{
			Reader: f,
			Size:   stat.Size(),
		})
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("mp4merge: create %s: %w", outputPath, err)
	}
	defer out.Close()

	bufferedOut := newBufferedWriteSeeker(out, 64*1024)

	if err := JoinFileStreams(handles, bufferedOut, progress); err != nil {
		return err
	}
	if err := bufferedOut.Flush(); err != nil {
		return err
	}
	return nil
}

// JoinFileStreams merges streams into the provided output seeker.
func JoinFileStreams(files []Stream, output io.WriteSeeker, progress func(float64)) error {
	if len(files) == 0 {
		return errors.New("mp4merge: no streams provided")
	}
	if output == nil {
		return errors.New("mp4merge: nil output stream")
	}

	desc := newDesc()
	desc.ensureTrack(9)

	handle := make([]*fileHandle, 0, len(files))
	var totalSize int64
	for _, f := range files {
		if f.Reader == nil {
			return errors.New("mp4merge: stream has nil reader")
		}
		handle = append(handle, &fileHandle{
			stream: f.Reader,
			size:   f.Size,
		})
		totalSize += f.Size
	}

	numFiles := float64(len(handle))
	instaMaxRead := ^uint64(0)
	instaDetected := false

	for i, fh := range handle {
		if _, err := fh.stream.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("mp4merge: seek file %d: %w", i, err)
		}
		for {
			typ, _, size, headerSize, err := readBox(fh.stream)
			if err != nil {
				return fmt.Errorf("mp4merge: failed to locate mdat in file %d: %w", i, err)
			}
			orgPos, err := fh.stream.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
			if typ == fourCC("mdat") {
				dataSize := size
				if dataSize >= uint64(headerSize) {
					dataSize -= uint64(headerSize)
				} else {
					dataSize = 0
				}
				desc.mdatPosition = append(desc.mdatPosition, mdatPosition{
					fileIndex: -1,
					offset:    uint64(orgPos),
					size:      dataSize,
				})
				desc.mdatFinalPosition = uint64(orgPos)
				break
			}
			if _, err := fh.stream.Seek(int64(orgPos)+int64(size)-headerSize, io.SeekStart); err != nil {
				return err
			}
		}

		if !instaDetected {
			if fh.size >= 40 {
				if _, err := fh.stream.Seek(-40, io.SeekEnd); err == nil {
					buf := make([]byte, 40)
					if _, err := io.ReadFull(fh.stream, buf); err == nil {
						if bytes.Equal(buf[8:], instaMagic) {
							extra := binary.LittleEndian.Uint32(buf[0:4])
							if int64(extra) <= fh.size {
								instaMaxRead = uint64(fh.size - int64(extra))
								instaDetected = true
							}
						}
					}
				}
			}
		}

		if _, err := fh.stream.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := readDesc(fh.stream, desc, 0, ^uint64(0), i); err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		last := &desc.mdatPosition[len(desc.mdatPosition)-1]
		last.fileIndex = i
		desc.mdatOffset += last.size

		for _, track := range desc.moovTracks {
			if track == nil {
				continue
			}
			track.sampleOffset = track.stszCount
			track.chunkOffset = uint32(len(track.stco))
		}

		if progress != nil {
			progress(((float64(i) + 1.0) / numFiles) * 0.1)
		}
	}

	var lastEmit time.Time
	ps := newProgressStream(output, func(total int64) {
		if progress == nil || totalSize == 0 {
			return
		}
		now := time.Now()
		if now.Sub(lastEmit) < 100*time.Millisecond && total != 0 {
			return
		}
		ratio := float64(total) / float64(totalSize)
		if ratio > 1 {
			ratio = 1
		}
		value := 0.1 + ratio*0.9
		if value > 0.9999 {
			value = 0.9999
		}
		progress(value)
		lastEmit = now
	})

	if _, err := handle[0].stream.Seek(0, io.SeekStart); err != nil {
		return err
	}

	if _, err := rewriteFromDesc(handle, ps, desc, 0, instaMaxRead); err != nil {
		return err
	}

	for _, track := range desc.moovTracks {
		if track == nil {
			continue
		}
		if track.co64FinalPosition < 0 || len(track.stco) == 0 {
			continue
		}
		if _, err := ps.Seek(track.co64FinalPosition, io.SeekStart); err != nil {
			return err
		}
		for _, offs := range track.stco {
			value := offs + desc.mdatFinalPosition
			if err := binary.Write(ps, binary.BigEndian, value); err != nil {
				return err
			}
		}
	}

	if instaDetected {
		if _, err := ps.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		offsets, err := getInsta360Offsets(handle)
		if err != nil {
			return err
		}
		if err := mergeInsta360Metadata(handle, offsets, ps); err != nil {
			return err
		}
	}

	if err := ps.Flush(); err != nil {
		return err
	}

	if progress != nil {
		progress(1.0)
	}

	return nil
}
