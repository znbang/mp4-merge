package mp4merge

import (
	"bufio"
	"io"
)

type bufferedWriteSeeker struct {
	inner  io.WriteSeeker
	writer *bufio.Writer
}

func newBufferedWriteSeeker(inner io.WriteSeeker, writeSize int) *bufferedWriteSeeker {
	return &bufferedWriteSeeker{
		inner:  inner,
		writer: bufio.NewWriterSize(inner, writeSize),
	}
}

func (b *bufferedWriteSeeker) Write(p []byte) (int, error) {
	return b.writer.Write(p)
}

func (b *bufferedWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := b.writer.Flush(); err != nil {
		return 0, err
	}
	pos, err := b.inner.Seek(offset, whence)
	if err != nil {
		return 0, err
	}
	return pos, nil
}

func (b *bufferedWriteSeeker) Flush() error {
	return b.writer.Flush()
}
