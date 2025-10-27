package mp4merge

import "io"

// Stream describes an input MP4 stream with its readable seeker and size.
type Stream struct {
	Reader io.ReadSeeker
	Size   int64
}

type fileHandle struct {
	stream io.ReadSeeker
	size   int64
}
