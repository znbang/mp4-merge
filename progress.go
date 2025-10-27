package mp4merge

import (
	"io"
)

type progressStream struct {
	inner    io.ReadWriteSeeker
	callback func(int64)
	total    int64
}

func newProgressStream(inner io.ReadWriteSeeker, callback func(int64)) *progressStream {
	if callback == nil {
		callback = func(int64) {}
	}
	return &progressStream{
		inner:    inner,
		callback: callback,
	}
}

func (p *progressStream) Read(b []byte) (int, error) {
	n, err := p.inner.Read(b)
	if n > 0 {
		p.total += int64(n)
		p.callback(p.total)
	}
	return n, err
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
