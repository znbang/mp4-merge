package mp4merge

import (
	"bufio"
	"io"
)

type progressStream struct {
	inner    io.WriteSeeker
	callback func(int64)
	total    int64
}

func newProgressStream(inner io.WriteSeeker, callback func(int64)) *progressStream {
	if callback == nil {
		callback = func(int64) {}
	}
	return &progressStream{
		inner:    inner,
		callback: callback,
	}
}

func (p *progressStream) Write(b []byte) (int, error) {
	n, err := p.inner.Write(b)
	if n > 0 {
		p.total += int64(n)
		p.callback(p.total)
	}
	return n, err
}

func (p *progressStream) Seek(offset int64, whence int) (int64, error) {
	return p.inner.Seek(offset, whence)
}

func (p *progressStream) Flush() error {
	type flusher interface {
		Flush() error
	}
	if f, ok := p.inner.(flusher); ok {
		return f.Flush()
	}
	return nil
}

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
