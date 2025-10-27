//go:build !windows

package mp4merge

import (
	"os"
)

// UpdateFileTimes copies the input file's modification timestamp to the output file.
func UpdateFileTimes(inputPath, outputPath string) error {
	info, err := os.Stat(inputPath)
	if err != nil {
		return err
	}
	mod := info.ModTime()
	return os.Chtimes(outputPath, mod, mod)
}
