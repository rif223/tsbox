package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileSHA256 returns the SHA-256 of a file in hex.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Within reports whether p is root or below it. The check is lexical: callers resolve links first where
// they matter.
func Within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// LimitWriter passes at most N bytes on to W; a write beyond that fails with "output too large".
type LimitWriter struct {
	W io.Writer
	N int64
}

func (l *LimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.N {
		return 0, errors.New("output too large")
	}
	l.N -= int64(len(p))
	return l.W.Write(p)
}
