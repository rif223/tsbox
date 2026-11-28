package driver

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
)

func TestFileHash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tsbox")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	h1, err := FileHash(p)
	if want, _ := fsutil.FileSHA256(p); err != nil || h1 != want {
		t.Fatalf("%s %v (want %s)", h1, err, want)
	}
	// A new binary (other size and mtime) gets a new hash, not the cached one.
	if err := os.WriteFile(p, []byte("version 2"), 0o755); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(p, later, later)
	if h2, _ := FileHash(p); h2 == h1 {
		t.Fatal("cached hash after the file changed")
	}
}
