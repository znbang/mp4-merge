//go:build windows

package mp4merge

import (
	"os"
	"syscall"
	"unsafe"
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

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
var getFileType = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileType")

func replaceOutputFile(temp, target string) error {
	a, err := syscall.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	b, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	const moveFileReplaceExisting = 0x1
	r, _, e := moveFileEx.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), moveFileReplaceExisting|0x8)
	if r == 0 {
		if e != syscall.Errno(0) {
			return e
		}
		return syscall.EINVAL
	}
	return nil
}

func isOutputDevice(path string) bool {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	const shareAll = 1 | 2 | 4
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, shareAll, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	typ, _, _ := getFileType.Call(uintptr(h))
	return typ == 2
}

func checkOutputCanBeReplaced(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	const deleteAccess = 0x00010000
	const shareAll = 1 | 2 | 4
	h, err := syscall.CreateFile(p, deleteAccess, shareAll, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	return syscall.CloseHandle(h)
}
