package mp4merge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// checkedOutputTarget resolves the output's final symlink target and refuses
// aliases of any input before an output file is created.
func checkedOutputTarget(path string, inputs []*os.File) (string, os.FileMode, bool, error) {
	if isOutputDevice(path) {
		return path, 0, true, nil
	}
	target, err := resolveOutputLinks(path)
	if err != nil {
		return "", 0, false, fmt.Errorf("mp4merge: resolve output %s: %w", path, err)
	}
	info, statErr := os.Stat(target)
	var mode os.FileMode
	direct := false
	if statErr == nil {
		if info.IsDir() {
			return "", 0, false, fmt.Errorf("mp4merge: output %s is a directory", path)
		}
		if !info.Mode().IsRegular() {
			direct = true
		} else {
			writable, openErr := os.OpenFile(target, os.O_WRONLY, 0)
			if openErr != nil {
				return "", 0, false, fmt.Errorf("mp4merge: output %s can't be written: %w", path, openErr)
			}
			_ = writable.Close()
			if err := checkOutputCanBeReplaced(target); err != nil {
				return "", 0, false, fmt.Errorf("mp4merge: output %s can't be replaced: %w", path, err)
			}
			mode = info.Mode()
		}
	} else if !os.IsNotExist(statErr) {
		return "", 0, false, fmt.Errorf("mp4merge: check output %s: %w", path, statErr)
	}
	for i, f := range inputs {
		inputInfo, e := f.Stat()
		if e != nil {
			return "", 0, false, fmt.Errorf("mp4merge: stat input %d: %w", i+1, e)
		}
		if info != nil && os.SameFile(inputInfo, info) {
			return "", 0, false, fmt.Errorf("mp4merge: output %s is input %d; refusing to overwrite it", path, i+1)
		}
	}
	dir := filepath.Dir(target)
	di, e := os.Stat(dir)
	if e != nil {
		return "", 0, false, fmt.Errorf("mp4merge: output directory %s: %w", dir, e)
	}
	if !di.IsDir() {
		return "", 0, false, fmt.Errorf("mp4merge: output directory %s is not a directory", dir)
	}
	return target, mode, direct, nil
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

func resolveOutputLinks(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cur := filepath.Clean(abs)
	for i := 0; i < 64; i++ {
		info, e := os.Lstat(cur)
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			to, e := os.Readlink(cur)
			if e != nil {
				return "", e
			}
			if !filepath.IsAbs(to) {
				to = filepath.Join(filepath.Dir(cur), to)
			}
			cur = filepath.Clean(to)
			continue
		}
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(cur)
		name := filepath.Base(cur)
		resolved, e := filepath.EvalSymlinks(parent)
		if e == nil {
			cur = filepath.Join(resolved, name)
		}
		return cur, nil
	}
	return "", fmt.Errorf("output path follows more than 64 symbolic links or contains a loop")
}
