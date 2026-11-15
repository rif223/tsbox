//go:build linux || darwin

package build

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestHashTreeSkipsSpecialFiles: a FIFO (and a symlink to it) in the script directory must not block the
// cache key (opening a FIFO without a writer blocks forever).
func TestHashTreeSkipsSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.ts", `console.log(1);`)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	if err := os.Symlink("pipe", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		files, size := 0, int64(0)
		done <- hashTree(sha256.New(), dir, &files, &size)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hashTree blocks on a FIFO")
	}
}
