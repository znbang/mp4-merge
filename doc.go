/*
Package mp4merge provides utilities to merge multiple MP4 streams into a single
file while preserving timing metadata, track tables, and Insta360 extensions.

The API mirrors the original Rust implementation:

	paths := []string{"clip1.mp4", "clip2.mp4"}
	err := mp4merge.JoinFiles(paths, "output.mp4", func(progress float64) {
		fmt.Printf("progress %.0f%%\n", progress*100)
	})
	if err != nil {
		log.Fatal(err)
	}

When finer control is required, JoinFileStreams accepts already opened
read/seek handles and writes into an io.WriteSeeker destination. Use
JoinFileStreamsWith to create that destination only after all input files have
been validated. ReadBox exposes the ISO BMFF header reader used by the merger.
*/
package mp4merge
