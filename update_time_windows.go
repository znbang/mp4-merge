//go:build windows

package mp4merge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	moveFileEx                   = kernel32.NewProc("MoveFileExW")
	getFileType                  = kernel32.NewProc("GetFileType")
	getFileInformationByHandleEx = kernel32.NewProc("GetFileInformationByHandleEx")
)

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

type winFileIDInfo struct {
	volumeSerialNumber uint64
	fileID             [16]byte
}

func winFileID(f *os.File) (winFileIDInfo, bool) {
	const fileIDInfoClass = 18
	var info winFileIDInfo
	r, _, _ := getFileInformationByHandleEx.Call(
		f.Fd(),
		fileIDInfoClass,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		return winFileIDInfo{}, false
	}
	return info, true
}

func isZeroFileID(id [16]byte) bool {
	for _, b := range id {
		if b != 0 {
			return false
		}
	}
	return true
}

func sameFileHandle(a, b *os.File) (bool, bool, error) {
	if ia, okA := winFileID(a); okA {
		if ib, okB := winFileID(b); okB {
			if isZeroFileID(ia.fileID) || isZeroFileID(ib.fileID) {
				return false, false, nil
			}
			return true, ia.volumeSerialNumber == ib.volumeSerialNumber && ia.fileID == ib.fileID, nil
		}
	}
	var ba, bb syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(a.Fd()), &ba); err != nil {
		return false, false, err
	}
	if err := syscall.GetFileInformationByHandle(syscall.Handle(b.Fd()), &bb); err != nil {
		return false, false, err
	}
	if (ba.FileIndexHigh == 0 && ba.FileIndexLow == 0) || (bb.FileIndexHigh == 0 && bb.FileIndexLow == 0) {
		return false, false, nil
	}
	same := ba.VolumeSerialNumber == bb.VolumeSerialNumber &&
		ba.FileIndexHigh == bb.FileIndexHigh &&
		ba.FileIndexLow == bb.FileIndexLow
	return true, same, nil
}

// checkOutputLinkName rejects Windows symlink targets with components ending in
// '.' or ' ' or containing ':', which Windows resolves differently from normal paths.
func checkOutputLinkName(link, to string) error {
	rest := to
	// Skip drive prefix (e.g. C:\) or UNC prefix (\\server\share)
	if vol := filepath.VolumeName(rest); vol != "" {
		rest = rest[len(vol):]
	}
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		if part == "." || part == ".." {
			continue
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.ContainsRune(part, ':') {
			return fmt.Errorf("the symbolic link %s points to %s, a name that Windows follows as written but programs can't use", link, to)
		}
	}
	return nil
}

// checkOutputUpLink rejects relative Windows symlinks containing ".." when the
// link's directory is reached through another link or junction.
func checkOutputUpLink(link, to, next string) error {
	if filepath.IsAbs(to) || (len(to) >= 2 && to[1] == ':') || strings.HasPrefix(to, `\`) || strings.HasPrefix(to, `/`) {
		return nil
	}
	hasParent := false
	for _, part := range strings.FieldsFunc(to, func(r rune) bool { return r == '\\' || r == '/' }) {
		if part == ".." {
			hasParent = true
			break
		}
	}
	if !hasParent {
		return nil
	}
	dirNext, errNext := canonicalPath(outputFolder(next))
	dirLink, errLink := canonicalPath(outputFolder(link))
	if errLink != nil {
		return fmt.Errorf("the symbolic link %s points to %s, up from its folder, and where that folder really is can't be told: %w", link, to, errLink)
	}
	dirReal, errReal := canonicalPath(outputFolder(filepath.Join(dirLink, to)))
	if errNext == nil && errReal == nil && dirNext == dirReal {
		return nil
	}
	if os.IsNotExist(errNext) && os.IsNotExist(errReal) {
		return nil
	}
	if errNext != nil && errReal != nil {
		return fmt.Errorf("the symbolic link %s points to %s, up from its folder, and where that folder really is can't be told: %w", link, to, errNext)
	}
	return fmt.Errorf("the symbolic link %s points to %s, up from its folder, which is reached through another link: which folder Windows takes it to can't be told", link, to)
}
