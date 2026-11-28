package driver

import (
	"fmt"
	"os"
	"sync"

	"github.com/rif223/tsbox/internal/fsutil"
)

var fileHashes struct {
	mu sync.Mutex
	m  map[string]string // path|size|mtime → SHA-256
}

// FileHash returns the SHA-256 (hex) of a file, cached by path, size and modification time. The drivers hash
// the tsbox binary at every box start; reading and hashing it took 20–75 ms of a start of about 250 ms.
func FileHash(p string) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d", p, st.Size(), st.ModTime().UnixNano())
	fileHashes.mu.Lock()
	h, ok := fileHashes.m[key]
	fileHashes.mu.Unlock()
	if ok {
		return h, nil
	}
	if h, err = fsutil.FileSHA256(p); err != nil {
		return "", err
	}
	fileHashes.mu.Lock()
	if fileHashes.m == nil || len(fileHashes.m) > 64 {
		fileHashes.m = map[string]string{}
	}
	fileHashes.m[key] = h
	fileHashes.mu.Unlock()
	return h, nil
}
