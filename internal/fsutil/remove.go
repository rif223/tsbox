package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// osRemoveAll is a variable so that tests can simulate the failure described at RemoveAll.
var osRemoveAll = os.RemoveAll

// RemoveAll removes path with everything in it, like os.RemoveAll. On Windows it falls back to removing entry
// by entry with plain paths when os.RemoveAll fails.
//
// Since Go 1.25, os.RemoveAll on Windows uses os/removeall_at.go: every entry is opened relative to the
// handle of its parent with O_NOFOLLOW_ANY (os/root_windows.go openat), i.e. NtCreateFile with
// OBJ_DONT_REPARSE. The file system virtualization of packaged (MSIX) apps (bindflt) redirects opens in a
// merged directory (one that exists both at the real location and in the package's LocalCache) with a
// reparse; OBJ_DONT_REPARSE forbids it, NtCreateFile fails with STATUS_REPARSE_POINT_ENCOUNTERED, and Go
// reports ELOOP ("openfdat ...: too many levels of symbolic links") or, with go1.26.0, the preceding
// "unlinkat ...: is a directory". os.Root has the same semantics and would fail the same way. tsbox runs this way when it
// is started from a terminal inside such an app; then the default state directory %LOCALAPPDATA%\tsbox is
// virtualized (tsbox doctor warns). Plain paths (FindFirstFile, DeleteFile, RemoveDirectory) are redirected
// correctly.
//
// The fallback never follows links: filepath.WalkDir does not descend into symlinks or junctions (both are
// not reported as directories), and os.Remove removes the link itself, so nothing outside path is touched.
// If both ways fail, the error contains both.
func RemoveAll(path string) error {
	err := osRemoveAll(path)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	ferr := removeByWalk(path)
	if ferr == nil {
		return nil
	}
	return errors.Join(err, ferr)
}

// RemoveAllRetry is RemoveAll with retries on Windows for about 2 s: processes that just ended (scripts,
// virus scanners, indexers) keep files open for a moment. For directories that must be gone, such as a box.
func RemoveAllRetry(path string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = RemoveAll(path); err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// removeByWalk removes files and links first (clearing the read-only attribute of files), then directories
// bottom-up. It only uses functions that take plain paths.
func removeByWalk(path string) error {
	var dirs []string
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() { // false for symlinks and junctions: they are removed as links
			dirs = append(dirs, p)
			return nil
		}
		if d.Type().IsRegular() {
			_ = os.Chmod(p, 0o666) // os.Remove cannot delete read-only files on Windows (artifacts are 0444)
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
