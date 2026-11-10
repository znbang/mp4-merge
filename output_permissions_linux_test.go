//go:build linux

package mp4merge

import (
	"os"
	"syscall"
	"testing"
)

func TestCreateOutputTempRespectsUmask(t *testing.T) {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)

	file, err := createOutputTemp(t.TempDir(), "merged.mp4")
	if err != nil {
		t.Fatalf("createOutputTemp failed: %v", err)
	}
	defer func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat temporary output: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("temporary output mode = %04o, want %04o under umask 077", got, want)
	}
}
