# Go Port of mp4-merge

This repository is a Go reimplementation of the original
[gyroflow/mp4-merge](https://github.com/gyroflow/mp4-merge) Rust library and CLI.
The tools remain functionally identical: they losslessly join multiple `.mp4`
files (shot with the same camera/settings) into a single output while preserving
raw tracks, metadata, and Insta360 trailers.

The port exists so Go applications can depend on the merger without crossing the
FFI boundary, and to make it easy to produce standalone binaries on any platform
with a Go toolchain.

## Features

- Merges camera-split files without re-encoding or quality loss.
- Preserves video, audio, metadata, edit lists, sample dependency tables, and sample groups.
- Chooses an exact merged media timescale and aligns tracks across recording boundaries.
- Cuts overlapping sound at sample boundaries and fills supported PCM gaps with silence.
- Merges AVC/HEVC parameter sets, Insta360 trailers, and camera segment-table timing.
- Validates box and chunk-table bounds before writing; file output is published atomically.
- Mirrors the Rust CLI flags and exposes stream APIs, including deferred output creation.

## Installation

```bash
go install github.com/znbang/mp4-merge/cmd/mp4merge@latest
```

Alternatively build from source:

```bash
git clone https://github.com/znbang/mp4-merge.git
cd mp4-merge
go build ./cmd/mp4merge
```

## CLI usage

```bash
./mp4merge --out output.mp4 input1.mp4 input2.mp4
```

When `--out` is omitted the CLI writes `<first_input>_joined.mp4`. You can also
drag and drop multiple files onto the executable after compiling if you prefer a
GUI workflow.

## Library usage

```go
package main

import (
	"log"

	"github.com/znbang/mp4-merge"
)

func main() {
	err := mp4merge.JoinFiles(
		[]string{"clip1.mp4", "clip2.mp4"},
		"merged.mp4",
		nil, // optional progress callback
	)
	if err != nil {
		log.Fatal(err)
	}
}
```

`JoinFileStreams` is available when you already have opened `io.ReadSeeker`
handles. `JoinFileStreamsWith` accepts an output factory and calls it only after
all inputs pass validation.

## How it works

The implementation follows the same approach as the Rust project:

1. Scan every input file and collect:
   - `mdat` offsets and sizes
   - Durations from `mvhd`, `tkhd`, `mdhd`
   - Edit lists and sample-table descriptors (`stts`, `ctts`, `stsz`, `stss`, `stps`, `sdtp`, `sbgp`, `csgm`, `stsc`, `stco`/`co64`)
2. Merge the collected metadata: sum durations, append sample tables, and adjust
   chunk offsets based on accumulated `mdat` sizes. Keep sample timing exact,
   align separate recordings to the anchor video track, and handle supported
   audio overlaps and gaps.
3. Walk the first file’s boxes, writing them to the output while:
   - Rebuilding `mdat` as a 64-bit box and copying payloads from all inputs.
   - Patching durations in `mvhd`, `tkhd`, `mdhd`.
   - Re-emitting `stbl` tables from the merged descriptors.
   - Promoting `stco` to `co64` when necessary.
4. Append Insta360 metadata trailers when detected.

## License

This Go port inherits the dual-license model from the upstream Rust project.
You may choose either the MIT or Apache 2.0 license.
