package build

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
)

// PruneAge is how long an unused entry of the artifact store is kept.
const PruneAge = 7 * 24 * time.Hour

// touchEvery limits how often the last use of a store entry is recorded.
const touchEvery = 24 * time.Hour

// touch records the use of a store entry in its modification time, for Prune. The content-addressed file
// stays unchanged.
func touch(p string) {
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	if now := time.Now(); now.Sub(st.ModTime()) > touchEvery {
		_ = os.Chtimes(p, now, now)
	}
}

// Prune removes entries of the artifact store that were not used for maxAge: artifacts, build cache
// entries, work directories of other @tsbox/core versions and leftovers of interrupted builds. It returns
// the number of removed entries; entries that cannot be removed are skipped.
//
// This is safe for existing boxes: every box has its own copy of its artifacts (<state>/boxes/<name>/
// artifacts), and a missing store entry only means a rebuild.
func Prune(storeDir string, maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	n := pruneFiles(filepath.Join(storeDir, "sha256"), cutoff)
	cache := filepath.Join(storeDir, "cache")
	ents, _ := os.ReadDir(cache)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(cache, e.Name())
		n += pruneFiles(dir, cutoff)
		_ = os.Remove(dir) // only succeeds when empty
	}
	work := filepath.Join(storeDir, "work")
	current := coreHash()
	ents, _ = os.ReadDir(work)
	for _, e := range ents {
		p := filepath.Join(work, e.Name())
		if e.Name() == current {
			n += pruneLeftovers(p, cutoff)
			continue
		}
		if old(e, cutoff) && fsutil.RemoveAll(p) == nil {
			n++
		}
	}
	return n
}

func pruneFiles(dir string, cutoff time.Time) int {
	n := 0
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !e.Type().IsRegular() || !old(e, cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// pruneLeftovers removes old build directories (b-*) and half-written core directories (.core-*) of the
// current core version; the core directory itself stays.
func pruneLeftovers(dir string, cutoff time.Time) int {
	n := 0
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "b-") && !strings.HasPrefix(name, ".core-") {
			continue
		}
		if old(e, cutoff) && fsutil.RemoveAll(filepath.Join(dir, name)) == nil {
			n++
		}
	}
	return n
}

func old(e os.DirEntry, cutoff time.Time) bool {
	st, err := e.Info()
	return err == nil && st.ModTime().Before(cutoff)
}
