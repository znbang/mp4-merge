package mp4merge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

func fourCC(s string) uint32 {
	if len(s) != 4 {
		panic("fourCC requires 4 byte string")
	}
	return uint32(s[0])<<24 |
		uint32(s[1])<<16 |
		uint32(s[2])<<8 |
		uint32(s[3])
}

func typeToString(typ uint32) string {
	buf := []byte{
		byte(typ >> 24),
		byte(typ >> 16),
		byte(typ >> 8),
		byte(typ),
	}
	if utf8.Valid(buf) {
		return string(buf)
	}
	return fmt.Sprintf("%08X", typ)
}

func readBox(r io.ReadSeeker) (typ uint32, offset int64, size uint64, headerSize int64, err error) {
	offset, err = r.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}

	var size32 uint32
	if err = binary.Read(r, binary.BigEndian, &size32); err != nil {
		return
	}
	if err = binary.Read(r, binary.BigEndian, &typ); err != nil {
		return
	}

	if size32 == 1 {
		var size64 uint64
		if err = binary.Read(r, binary.BigEndian, &size64); err != nil {
			return
		}
		size = size64
		headerSize = 16
	} else {
		size = uint64(size32)
		headerSize = 8
	}
	return
}

// ReadBox reads an ISO BMFF box header from the seeker's current position.
// It returns the box type, absolute offset, declared size, and header size.
// A declared size of zero denotes a box extending to the end of the stream.
func ReadBox(r io.ReadSeeker) (typ uint32, offset uint64, size uint64, headerSize int64, err error) {
	if r == nil {
		return 0, 0, 0, 0, fmt.Errorf("mp4merge: nil box reader")
	}
	boxType, pos, boxSize, header, err := readBox(r)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if pos < 0 {
		return 0, 0, 0, 0, fmt.Errorf("mp4merge: box reader returned a negative position %d", pos)
	}
	const maxInt64 = uint64(1<<63 - 1)
	if boxSize != 0 && (boxSize < uint64(header) || boxSize > maxInt64 || uint64(pos) > maxInt64-boxSize) {
		return 0, 0, 0, 0, fmt.Errorf("mp4merge: the %s box at %d has an invalid size %d", typeToString(boxType), pos, boxSize)
	}
	return boxType, uint64(pos), boxSize, header, nil
}

type boxHeader struct {
	typ        uint32
	offset     uint64
	size       uint64
	headerSize uint64
	end        uint64
}

func readBoxWithin(r io.ReadSeeker, fileEnd, parentEnd uint64) (boxHeader, error) {
	pos64, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return boxHeader{}, err
	}
	if pos64 < 0 {
		return boxHeader{}, mp4Error("invalid data", "negative box offset %d", pos64)
	}
	pos := uint64(pos64)
	left := uint64(0)
	if parentEnd > pos {
		left = parentEnd - pos
	}
	if left < 8 {
		return boxHeader{}, mp4Error("invalid data", "the %d bytes at %d are not a box", left, pos)
	}
	var h [16]byte
	if _, err := io.ReadFull(r, h[:8]); err != nil {
		return boxHeader{}, err
	}
	size32 := binary.BigEndian.Uint32(h[:4])
	typ := binary.BigEndian.Uint32(h[4:8])
	headerSize := uint64(8)
	size := uint64(size32)
	switch size32 {
	case 0:
		end := fileEnd
		if end > parentEnd {
			return boxHeader{}, mp4Error("invalid data", "the %s box at %d runs past the end of its parent at %d", typeToString(typ), pos, parentEnd)
		}
		return boxHeader{typ: typ, offset: pos, size: 0, headerSize: 8, end: end}, nil
	case 1:
		if left < 16 {
			return boxHeader{}, mp4Error("invalid data", "the %s box at %d has a 64-bit size, which does not fit in the %d bytes left", typeToString(typ), pos, left)
		}
		if _, err := io.ReadFull(r, h[8:]); err != nil {
			return boxHeader{}, err
		}
		size = binary.BigEndian.Uint64(h[8:])
		headerSize = 16
	}
	if size < headerSize || pos > math.MaxUint64-size || pos+size > math.MaxInt64 {
		return boxHeader{}, mp4Error("invalid data", "the %s box at %d has an invalid size %d", typeToString(typ), pos, size)
	}
	end := pos + size
	if end > fileEnd {
		return boxHeader{}, mp4Error("invalid data", "the %s box at %d has a size of %d, which runs past the file end at %d", typeToString(typ), pos, size, fileEnd)
	}
	if end > parentEnd {
		return boxHeader{}, mp4Error("invalid data", "the %s box at %d runs past its parent at %d", typeToString(typ), pos, parentEnd)
	}
	return boxHeader{typ: typ, offset: pos, size: size, headerSize: headerSize, end: end}, nil
}

func readBody(r io.Reader, size uint64, typ uint32, offset uint64) ([]byte, error) {
	if size > uint64(maxInt()) {
		return nil, mp4Error("invalid data", "the %s box at %d does not fit in memory", typeToString(typ), offset)
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, mp4Error("unexpected EOF", "the file ends inside the %s box at %d", typeToString(typ), offset)
		}
		return nil, err
	}
	return body, nil
}

func maxInt() int { return int(^uint(0) >> 1) }

func copyExact(dst io.Writer, src io.Reader, size uint64) error {
	n, err := io.CopyN(dst, src, int64(size))
	if err != nil {
		return err
	}
	if uint64(n) != size {
		return fmt.Errorf("unexpected EOF: copied %d of %d bytes", n, size)
	}
	return nil
}

func hasChildren(typ uint32) bool {
	switch typ {
	case fourCC("moov"), fourCC("trak"), fourCC("edts"), fourCC("mdia"), fourCC("minf"), fourCC("stbl"):
		return true
	default:
		return false
	}
}

func parentOf(typ uint32) uint32 {
	switch typ {
	case fourCC("trak"):
		return fourCC("moov")
	case fourCC("edts"), fourCC("mdia"):
		return fourCC("trak")
	case fourCC("minf"):
		return fourCC("mdia")
	case fourCC("stbl"):
		return fourCC("minf")
	default:
		return 0
	}
}

func onlyOne(parent, typ uint32) bool {
	var allowed []string
	switch parent {
	case fourCC("moov"):
		allowed = []string{"mvhd"}
	case fourCC("trak"):
		allowed = []string{"tkhd", "edts", "mdia"}
	case fourCC("edts"):
		allowed = []string{"elst"}
	case fourCC("mdia"):
		allowed = []string{"mdhd", "hdlr", "minf"}
	case fourCC("minf"):
		allowed = []string{"stbl"}
	case fourCC("stbl"):
		allowed = []string{"stsd", "stts", "ctts", "cslg", "stsz", "stz2", "stsc", "stco", "co64", "stss", "stps", "sdtp"}
	}
	key := typeToString(typ)
	if parent == fourCC("stbl") {
		if typ == fourCC("stsz") || typ == fourCC("stz2") {
			key = "stsz or stz2"
		}
		if typ == fourCC("stco") || typ == fourCC("co64") {
			key = "stco or co64"
		}
	}
	for _, name := range allowed {
		if name == key || typ == fourCC(name) {
			return true
		}
	}
	return false
}

func inFile(index int, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("file %d: %w", index+1, err)
}

// Stream describes an input MP4 stream with its readable seeker and size.
type Stream struct {
	Reader io.ReadSeeker
	Size   int64
}

type fileHandle struct {
	stream io.ReadSeeker
	size   int64
}

// JoinFiles merges MP4 files into outputPath. It validates the inputs before
// touching the destination and publishes a completed temporary file atomically.
func JoinFiles(paths []string, outputPath string, progress func(float64)) error {
	if len(paths) == 0 {
		return errors.New("mp4merge: no input files provided")
	}
	inputs := make([]*os.File, 0, len(paths))
	streams := make([]Stream, 0, len(paths))
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
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("mp4merge: stat %s: %w", path, err)
		}
		inputs = append(inputs, f)
		streams = append(streams, Stream{Reader: f, Size: st.Size()})
	}
	target, mode, direct, err := checkedOutputTarget(outputPath, inputs)
	if err != nil {
		return err
	}
	limited := func(p float64) {
		if progress != nil {
			if p >= 1 {
				p = 0.9999
			}
			progress(p)
		}
	}
	d, insta, handles, total, err := prepareInputs(streams, limited)
	if err != nil {
		return err
	}
	if direct {
		output, err := os.OpenFile(target, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("mp4merge: open output device %s: %w", outputPath, err)
		}
		buffered := newBufferedWriteSeeker(output, 64*1024)
		mergeErr := joinPrepared(streams, buffered, d, insta, handles, total, limited)
		flushErr := buffered.Flush()
		closeErr := output.Close()
		if mergeErr != nil {
			return mergeErr
		}
		if flushErr != nil {
			return flushErr
		}
		if closeErr != nil {
			return closeErr
		}
		if progress != nil {
			progress(1)
		}
		return nil
	}
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	tmp, err := createOutputTemp(dir, base)
	if err != nil {
		return fmt.Errorf("mp4merge: create temporary output in %s: %w", dir, err)
	}
	temp := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temp)
		}
	}()
	buffered := newBufferedWriteSeeker(tmp, 64*1024)
	if err := joinPrepared(streams, buffered, d, insta, handles, total, limited); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := buffered.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if mode != 0 {
		if err := tmp.Chmod(mode); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("mp4merge: preserve output permissions: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("mp4merge: flush temporary output to disk: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceOutputFile(temp, target); err != nil {
		keep = true
		return fmt.Errorf("mp4merge: completed output remains at %s; could not replace %s: %w", temp, outputPath, err)
	}
	keep = true
	if progress != nil {
		progress(1)
	}
	return nil
}

// JoinFileStreams merges readable, seekable input streams into a writable
// seeker. Input descriptions are checked before the first output byte is written.
func JoinFileStreams(files []Stream, output io.WriteSeeker, progress func(float64)) error {
	if len(files) == 0 {
		return errors.New("mp4merge: no streams provided")
	}
	if output == nil {
		return errors.New("mp4merge: nil output stream")
	}
	return joinStreams(files, output, progress)
}

// JoinFileStreamsWith validates and plans all inputs before creating the output.
// This lets callers avoid touching a destination when the merge is rejected.
// The returned writer contains the completed merge. If it implements Flush()
// error, that method is called before this function returns.
func JoinFileStreamsWith(files []Stream, createOutput func() (io.WriteSeeker, error), progress func(float64)) (io.WriteSeeker, error) {
	if len(files) == 0 {
		return nil, errors.New("mp4merge: no streams provided")
	}
	if createOutput == nil {
		return nil, errors.New("mp4merge: nil output factory")
	}
	limited := func(p float64) {
		if progress != nil {
			if p >= 1 {
				p = 0.9999
			}
			progress(p)
		}
	}
	d, insta, handles, total, err := prepareInputs(files, limited)
	if err != nil {
		return nil, err
	}
	output, err := createOutput()
	if err != nil {
		return nil, err
	}
	if output == nil {
		return nil, errors.New("mp4merge: output factory returned nil")
	}
	if err := joinPrepared(files, output, d, insta, handles, total, limited); err != nil {
		if closer, ok := output.(io.Closer); ok {
			if closeErr := closer.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("mp4merge: close output after merge failure: %w", closeErr))
			}
		}
		return nil, err
	}
	if progress != nil {
		progress(1)
	}
	return output, nil
}

func prepareInputs(files []Stream, progress func(float64)) (*desc, bool, []*fileHandle, int64, error) {
	if len(files) == 0 {
		return nil, false, nil, 0, errors.New("mp4merge: no streams provided")
	}
	d := &desc{}
	handles := make([]*fileHandle, len(files))
	var total int64
	var insta *bool
	for i, f := range files {
		if f.Reader == nil {
			return nil, false, nil, 0, mp4Error("invalid input", "file %d has no reader", i+1)
		}
		if f.Size < 0 {
			return nil, false, nil, 0, mp4Error("invalid input", "file %d has negative size", i+1)
		}
		if math.MaxInt64-total < f.Size {
			return nil, false, nil, 0, mp4Error("invalid input", "total input size overflows")
		}
		total += f.Size
		handles[i] = &fileHandle{stream: f.Reader, size: f.Size}
	}
	for i, f := range files {
		if f.Size < 40 {
			return nil, false, nil, 0, inFile(i, io.ErrUnexpectedEOF)
		}
		boxesEnd := uint64(f.Size)
		if _, err := f.Reader.Seek(-40, io.SeekEnd); err != nil {
			return nil, false, nil, 0, inFile(i, err)
		}
		var footer [40]byte
		if _, err := io.ReadFull(f.Reader, footer[:]); err != nil {
			return nil, false, nil, 0, inFile(i, err)
		}
		hasInsta := bytes.Equal(footer[8:], instaMagic)
		if hasInsta {
			extra := uint64(binary.LittleEndian.Uint32(footer[:4]))
			if extra > boxesEnd {
				return nil, false, nil, 0, inFile(i, mp4Error("invalid data", "Insta360 metadata is larger than the file"))
			}
			boxesEnd -= extra
		}
		if insta == nil {
			v := hasInsta
			insta = &v
		} else if *insta != hasInsta {
			return nil, false, nil, 0, mp4Error("unsupported input", "file %d %s Insta360 metadata, but file 1 %s", i+1, map[bool]string{true: "has", false: "has no"}[hasInsta], map[bool]string{true: "has it", false: "doesn't"}[*insta])
		}
		p := partDesc{end: boxesEnd, size: uint64(f.Size)}
		if _, err := f.Reader.Seek(0, io.SeekStart); err != nil {
			return nil, false, nil, 0, err
		}
		for pos := uint64(0); pos < boxesEnd; {
			if boxesEnd-pos < 8 {
				break
			}
			h, err := readBoxWithin(f.Reader, boxesEnd, boxesEnd)
			if err != nil {
				return nil, false, nil, 0, inFile(i, err)
			}
			if h.size == 0 {
				pos = h.end
				break
			}
			pos = h.end
			if _, err := f.Reader.Seek(int64(pos), io.SeekStart); err != nil {
				return nil, false, nil, 0, err
			}
		}
		d.parts = append(d.parts, p)
		part := &d.parts[i]
		if _, err := f.Reader.Seek(0, io.SeekStart); err != nil {
			return nil, false, nil, 0, err
		}
		if err := readDescription(f.Reader, d, 0, boxesEnd, i, 0); err != nil {
			return nil, false, nil, 0, err
		}
		if len(part.mdats) == 0 {
			return nil, false, nil, 0, inFile(i, mp4Error("invalid data", "file has no mdat box"))
		}
		if part.moov == 0 {
			return nil, false, nil, 0, inFile(i, mp4Error("invalid data", "file has no moov box; it may be an unfinished recording"))
		}
		if part.moov > 1 {
			return nil, false, nil, 0, inFile(i, mp4Error("invalid data", "file has more than one moov box"))
		}
		if len(part.tracks) == 0 {
			return nil, false, nil, 0, inFile(i, mp4Error("invalid data", "file has no tracks"))
		}
		if part.walkEnd == 0 {
			return nil, false, nil, 0, inFile(i, mp4Error("invalid data", "file has no complete boxes"))
		}
		chosen := -1
		for m, mdat := range part.mdats {
			contains := true
			end := mdat[0] + mdat[1]
			for ti := range part.tracks {
				for _, off := range part.tracks[ti].stco {
					if off < mdat[0] || off-mdat[0] > mdat[1] {
						contains = false
						break
					}
				}
				if !contains {
					break
				}
			}
			if contains {
				chosen = m
				_ = end
				break
			}
		}
		if chosen < 0 {
			chosen = 0
		}
		mdat := part.mdats[chosen]
		if part.walkEnd < mdat[0]+mdat[1] {
			return nil, false, nil, 0, inFile(i, mp4Error("invalid data", "boxes end at %d, before the selected mdat ends at %d", part.walkEnd, mdat[0]+mdat[1]))
		}
		for mi, other := range part.mdats {
			if mi != chosen {
				logWarning("file %d has another mdat at %d; only sample data in one mdat can be merged", i+1, other[0])
			}
		}
		if d.mdatOffset > math.MaxUint64-mdat[1] {
			return nil, false, nil, 0, mp4Error("invalid data", "merged mdat size overflows")
		}
		for ti := range part.tracks {
			for j, off := range part.tracks[ti].stco {
				if off < mdat[0] || off-mdat[0] > mdat[1] || math.MaxUint64-d.mdatOffset < off-mdat[0] {
					part.tracks[ti].stco[j] = math.MaxUint64
				} else {
					part.tracks[ti].stco[j] = d.mdatOffset + off - mdat[0]
				}
			}
		}
		d.mdatPositions = append(d.mdatPositions, mdatPosition{fileIndex: i, offset: mdat[0], size: mdat[1]})
		part.mdat = [2]uint64{d.mdatOffset, mdat[1]}
		d.mdatOffset += mdat[1]
		if progress != nil {
			progress(float64(i+1) / float64(len(files)) * 0.1)
		}
	}
	if err := merge(d); err != nil {
		return nil, false, nil, 0, err
	}
	return d, insta != nil && *insta, handles, total, nil
}

func joinPrepared(files []Stream, out io.WriteSeeker, d *desc, insta bool, handles []*fileHandle, total int64, progress func(float64)) error {
	var last time.Time
	ps := newProgressStream(out, func(n int64) {
		if progress == nil || total <= 0 {
			return
		}
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < 100*time.Millisecond {
			return
		}
		ratio := float64(n) / float64(total)
		if ratio > 1 {
			ratio = 1
		}
		value := 0.1 + ratio*0.9
		if value > 0.9999 {
			value = 0.9999
		}
		progress(value)
		last = now
	})
	if err := rewrite(files, ps, d); err != nil {
		return err
	}
	if insta {
		if _, err := ps.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		offsets, err := getInsta360Offsets(handles)
		if err != nil {
			return err
		}
		if err := mergeInsta360Metadata(handles, offsets, ps); err != nil {
			return err
		}
	}
	if err := ps.Flush(); err != nil {
		return err
	}
	if progress != nil {
		progress(1)
	}
	return nil
}

func joinStreams(files []Stream, out io.WriteSeeker, progress func(float64)) error {
	if out == nil {
		return errors.New("mp4merge: nil output stream")
	}
	d, insta, handles, total, err := prepareInputs(files, progress)
	if err != nil {
		return err
	}
	return joinPrepared(files, out, d, insta, handles, total, progress)
}

func decodeStatxBirthTime(data []byte, order binary.ByteOrder, path string) (time.Time, error) {
	if len(data) < 92 {
		return time.Time{}, fmt.Errorf("statx result is too short for %s", path)
	}
	const statxBTIME = 0x800
	if order.Uint32(data[:4])&statxBTIME == 0 {
		return time.Time{}, fmt.Errorf("creation time is unavailable for %s", path)
	}
	sec := int64(order.Uint64(data[80:88]))
	nsec := int64(order.Uint32(data[88:92]))
	if nsec >= 1_000_000_000 {
		return time.Time{}, fmt.Errorf("invalid statx birth time for %s", path)
	}
	return time.Unix(sec, nsec), nil
}
