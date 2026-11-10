//go:build !windows && !linux

package mp4merge

import (
	"fmt"
	"os"
)

// UpdateFileTimes copies the input file's creation timestamp to the output mtime,
// matching the Rust CLI on Unix (which cannot set creation time there).
func UpdateFileTimes(inputPath, outputPath string) error {
	info, err := os.Stat(inputPath)
	if err != nil {
		return err
	}
	created, ok := unixCreationTime(info)
	if !ok {
		return fmt.Errorf("creation time is unavailable for %s", inputPath)
	}
	out, err := os.Stat(outputPath)
	if err != nil {
		return err
	}
	atime, ok := unixAccessTime(out)
	if !ok {
		return fmt.Errorf("access time is unavailable for %s", outputPath)
	}
	return os.Chtimes(outputPath, atime, created)
}

func replaceOutputFile(temp, target string) error { return os.Rename(temp, target) }

func isOutputDevice(string) bool { return false }

func checkOutputCanBeReplaced(string) error { return nil }
