package mp4merge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxOutputLinks = 63

// checkedOutputTarget resolves the output's final symlink target and refuses
// aliases of any input, invalid file names, or unsafe symlinks before an output
// file is created.
func checkedOutputTarget(path string, names []string, inputs []*os.File) (string, os.FileMode, bool, error) {
	existing, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			target := path
			if isLink(path) {
				var err error
				target, err = linksEnd(path)
				if err != nil {
					return "", 0, false, fmt.Errorf("mp4merge: checking the output %s: %w", path, err)
				}
			}
			if err := checkNewOutput(path, target); err != nil {
				return "", 0, false, err
			}
			if target != path {
				if _, lerr := os.Lstat(target); lerr == nil {
					return "", 0, false, fmt.Errorf("mp4merge: checking the output %s: the symbolic link leads to %s, an existing file that the system doesn't reach through it", path, target)
				}
			}
			return target, 0, false, nil
		}
		if isOutputDevice(path) {
			return path, 0, true, nil
		}
		return "", 0, false, fmt.Errorf("mp4merge: checking the output %s: %w", path, statErr)
	}

	if existing.IsDir() {
		return "", 0, false, fmt.Errorf("mp4merge: the output %s is a directory", path)
	}
	if !existing.Mode().IsRegular() {
		return path, 0, true, nil
	}

	if err := checkOutputName(path, path); err != nil {
		return "", 0, false, err
	}

	outFile, openErr := os.Open(path)
	target := path
	if isLink(path) {
		var err error
		target, err = linkedFile(path, outFile)
		if err != nil {
			if outFile != nil {
				_ = outFile.Close()
			}
			return "", 0, false, fmt.Errorf("mp4merge: checking the output %s: %w", path, err)
		}
	}
	if err := checkOutputName(path, target); err != nil {
		if outFile != nil {
			_ = outFile.Close()
		}
		return "", 0, false, err
	}

	var unknownErr error
	for i, input := range inputs {
		var known, same bool
		var err error
		if outFile != nil {
			known, same, err = sameFileHandle(input, outFile)
			if err != nil {
				_ = outFile.Close()
				return "", 0, false, fmt.Errorf("mp4merge: checking the output %s: %w", path, err)
			}
		}
		if !known {
			name := ""
			if i < len(names) {
				name = names[i]
			}
			a, errA := canonicalPath(name)
			b, errB := canonicalPath(target)
			if errA == nil && errB == nil {
				same = a == b
			} else if unknownErr == nil {
				e := errA
				if e == nil {
					e = errB
				}
				why := "its filesystem gives no file IDs"
				if openErr != nil {
					why = fmt.Sprintf("it can't be opened (%v)", openErr)
				}
				unknownErr = fmt.Errorf("mp4merge: the output %s can't be told apart from input %d (%s): %s, and there is no canonical path: %w", path, i+1, name, why, e)
			}
		}
		if same {
			if outFile != nil {
				_ = outFile.Close()
			}
			return "", 0, false, fmt.Errorf("mp4merge: output %s is input %d; refusing to overwrite it", path, i+1)
		}
	}
	if outFile != nil {
		_ = outFile.Close()
	}

	writable, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return "", 0, false, fmt.Errorf("mp4merge: output %s can't be written: %w", path, err)
	}
	_ = writable.Close()

	if err := checkOutputCanBeReplaced(target); err != nil {
		return "", 0, false, fmt.Errorf("mp4merge: output %s can't be replaced: %w", path, err)
	}
	if unknownErr != nil {
		return "", 0, false, unknownErr
	}

	var mode os.FileMode
	if runtime.GOOS != "windows" {
		mode = existing.Mode()
	}
	return target, mode, false, nil
}

func createOutputTemp(dir, base string) (*os.File, error) {
	for n := 0; n < 1000; n++ {
		name := filepath.Join(dir, fmt.Sprintf(".%s.%d-%d.mp4merge-partial", base, os.Getpid(), n))
		file, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			return file, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("no free temporary output name next to %s", filepath.Join(dir, base))
}

func isLink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func outputFolder(path string) string {
	dir := filepath.Dir(path)
	if dir == "" {
		return "."
	}
	return dir
}

func checkNewOutput(path, target string) error {
	if err := checkOutputName(path, target); err != nil {
		return err
	}
	dir := outputFolder(target)
	di, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("mp4merge: output directory %s: %w", dir, err)
	}
	if !di.IsDir() {
		return fmt.Errorf("mp4merge: output directory %s is not a directory", dir)
	}
	return nil
}

func checkOutputName(path, target string) error {
	if notAFileName(path) {
		return fmt.Errorf("mp4merge: the output %s isn't a file name", path)
	}
	if target != path && notAFileName(target) {
		return fmt.Errorf("mp4merge: the output %s, a symbolic link to %s, isn't a file name", path, target)
	}
	return nil
}

func isPathSep(c byte) bool {
	return c == '/' || (runtime.GOOS == "windows" && c == '\\')
}

// notAFileName reports whether path names no file: empty, ends with a separator,
// its last component is "." or "..", or on Windows its name contains ':'.
func notAFileName(path string) bool {
	if path == "" {
		return true
	}
	if isPathSep(path[len(path)-1]) {
		return true
	}
	// Find the last path component without calling filepath.Clean (which strips ".").
	lastSep := -1
	for i := len(path) - 1; i >= 0; i-- {
		if isPathSep(path[i]) {
			lastSep = i
			break
		}
	}
	base := path[lastSep+1:]
	if base == "" || base == "." || base == ".." {
		return true
	}
	if runtime.GOOS == "windows" && strings.ContainsRune(base, ':') {
		return true
	}
	return false
}

func linksEnd(path string) (string, error) {
	end := path
	for i := 0; i <= maxOutputLinks; i++ {
		if !isLink(end) {
			return end, nil
		}
		to, err := os.Readlink(end)
		if err != nil {
			return "", err
		}
		if notAFileName(to) {
			return "", fmt.Errorf("the symbolic link %s points to %s, which isn't a file name", end, to)
		}
		if err := checkOutputLinkName(end, to); err != nil {
			return "", err
		}
		var next string
		if filepath.IsAbs(to) || (runtime.GOOS == "windows" && len(to) >= 2 && to[1] == ':') {
			next = to
		} else {
			parent := filepath.Dir(end)
			if parent == "." && !strings.HasPrefix(end, ".") {
				next = to
			} else {
				next = parent + string(os.PathSeparator) + to
			}
		}
		if err := checkOutputUpLink(end, to, next); err != nil {
			return "", err
		}
		end = next
	}
	return "", fmt.Errorf("it leads through more than %d symbolic links, or a loop of them", maxOutputLinks)
}

func linkedFile(path string, outFile *os.File) (string, error) {
	target, err := canonicalPath(path)
	if err == nil {
		return target, nil
	}
	end, linkErr := linksEnd(path)
	if linkErr != nil {
		return "", linkErr
	}
	if outFile != nil {
		if endFile, openErr := os.Open(end); openErr == nil {
			known, same, sameErr := sameFileHandle(outFile, endFile)
			_ = endFile.Close()
			if sameErr == nil && known && same {
				return end, nil
			}
		}
	}
	return "", fmt.Errorf("finding the file its symbolic links lead to: %w", err)
}

func canonicalPath(path string) (string, error) {
	if path == "" {
		return "", os.ErrInvalid
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}
