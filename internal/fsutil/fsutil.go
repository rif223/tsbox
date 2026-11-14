// Package fsutil contains file operations that need retries on Windows.
package fsutil

import (
	"os"
	"runtime"
	"time"
)

// Rename atomically replaces to with from. On Windows, virus scanners or indexers sometimes keep freshly
// written files (for directories: a file inside) open for a moment; Rename then fails with "Access is
// denied" and succeeds shortly afterwards. Full go test ./... run in phase 3: TestEcho (directory in sdkreg).
func Rename(from, to string) error {
	var err error
	for i := 0; i < 40; i++ {
		if err = os.Rename(from, to); err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(time.Duration(10+5*i) * time.Millisecond) // ≈ 4 s in total
	}
	return err
}
