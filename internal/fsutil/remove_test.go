package fsutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func mkTree(t *testing.T, dir string) {
	t.Helper()
	for rel, c := range map[string]string{"tsboxd.json": "{}", "artifacts/a.js": "1", "logs/main.log": "x"} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "artifacts", "a.js"), 0o444); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAllFallback(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fallback is Windows only")
	}
	box := filepath.Join(t.TempDir(), "box")
	mkTree(t, box)
	// a link inside the box must be removed as a link, its target must stay
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outside, filepath.Join(box, "link")) // needs developer mode or admin; optional
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(box, "junction"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	osRemoveAll = func(string) error { return syscall.ELOOP }
	defer func() { osRemoveAll = os.RemoveAll }()
	if err := RemoveAll(box); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(box); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("box still there: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("link target removed: %v", err)
	}
}

func TestRemoveAllPlain(t *testing.T) {
	box := filepath.Join(t.TempDir(), "box")
	mkTree(t, box)
	if err := RemoveAll(box); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(box); err != nil { // missing directory is no error
		t.Fatal(err)
	}
}

func TestRemoveAllBothFail(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fallback is Windows only")
	}
	box := filepath.Join(t.TempDir(), "box")
	mkTree(t, box)
	f, err := os.Open(filepath.Join(box, "logs", "main.log")) // Go opens without FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	osRemoveAll = func(string) error { return syscall.ELOOP }
	defer func() { osRemoveAll = os.RemoveAll }()
	err = RemoveAll(box)
	if !errors.Is(err, syscall.ELOOP) || !strings.Contains(err.Error(), "main.log") {
		t.Fatalf("want both errors, got %v", err)
	}
}
