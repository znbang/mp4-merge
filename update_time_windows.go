//go:build windows

package mp4merge

import (
	"os"
	"syscall"
)

// UpdateFileTimes copies the input file's creation time onto the output file.
func UpdateFileTimes(inputPath, outputPath string) error {
	info, err := os.Stat(inputPath)
	if err != nil {
		return err
	}
	attr, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return nil
	}

	ptr, err := syscall.UTF16PtrFromString(outputPath)
	if err != nil {
		return err
	}
	handle, err := syscall.CreateFile(ptr, syscall.FILE_WRITE_ATTRIBUTES, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)

	return syscall.SetFileTime(handle, &attr.CreationTime, nil, nil)
}
