//go:build linux

package mp4merge

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// UpdateFileTimes copies the source creation time to the destination mtime,
// as filetime_creation does on Unix. It leaves the destination access time intact.
func UpdateFileTimes(inputPath, outputPath string) error {
	created, err := linuxBirthTime(inputPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return err
	}
	atime, ok := unixAccessTime(info)
	if !ok {
		return fmt.Errorf("access time is unavailable for %s", outputPath)
	}
	return os.Chtimes(outputPath, atime, created)
}

func linuxBirthTime(path string) (time.Time, error) {
	nr := uint64(0)
	switch runtime.GOARCH {
	case "amd64":
		nr = 332
	case "386":
		nr = 383
	case "arm":
		nr = 397
	case "arm64", "riscv64", "loong64":
		nr = 291
	case "ppc64", "ppc64le":
		nr = 383
	case "s390x":
		nr = 379
	case "mips", "mipsle":
		nr = 4366
	case "mips64", "mips64le":
		nr = 5326
	default:
		return time.Time{}, fmt.Errorf("statx creation time is unsupported on linux/%s", runtime.GOARCH)
	}
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return time.Time{}, err
	}
	var storage [32]uint64
	dirfd := -100
	const statxBTIME = 0x800
	_, _, errno := syscall.Syscall6(uintptr(nr), uintptr(dirfd), uintptr(unsafe.Pointer(p)), 0, statxBTIME, uintptr(unsafe.Pointer(&storage[0])), 0)
	runtime.KeepAlive(p)
	if errno != 0 {
		return time.Time{}, errno
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(&storage[0])), 256)
	return decodeStatxBirthTime(b, binary.NativeEndian, path)
}

func replaceOutputFile(temp, target string) error { return os.Rename(temp, target) }

func isOutputDevice(string) bool { return false }

func checkOutputCanBeReplaced(string) error { return nil }
