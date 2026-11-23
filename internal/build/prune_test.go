package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeAged(t *testing.T, p string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil { // artifacts are read-only
		t.Fatal(err)
	}
	setAge(t, p, age)
}

func setAge(t *testing.T, p string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestPrune(t *testing.T) {
	const old, fresh = 8 * 24 * time.Hour, time.Hour
	dir := t.TempDir()
	cur := filepath.Join(dir, "work", coreHash())
	gone := []string{
		filepath.Join(dir, "sha256", "old.js"),
		filepath.Join(dir, "cache", "aa", "oldkey"),
		filepath.Join(dir, "work", "core-oldversion", "core", "mod.ts"),
		filepath.Join(cur, "b-old", "entry.ts"),
	}
	kept := []string{
		filepath.Join(dir, "sha256", "new.js"),
		filepath.Join(dir, "cache", "bb", "newkey"),
		filepath.Join(dir, "work", "core-newversion", "core", "mod.ts"),
		filepath.Join(cur, "b-new", "entry.ts"),
		filepath.Join(cur, "core", "mod.ts"), // the current core stays, however old
	}
	for _, p := range gone {
		writeAged(t, p, old)
	}
	for _, p := range kept {
		writeAged(t, p, fresh)
	}
	setAge(t, filepath.Join(cur, "core", "mod.ts"), old)
	for _, d := range []string{filepath.Join(dir, "work", "core-oldversion"), filepath.Join(cur, "b-old"), filepath.Join(cur, "core")} {
		setAge(t, d, old)
	}
	if n := Prune(dir, PruneAge); n != len(gone) {
		t.Errorf("removed %d entries, want %d", n, len(gone))
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there (%v)", p, err)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cache", "aa")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty cache directory still there (%v)", err)
	}
}

// A used artifact counts as fresh again, also when it is read-only.
func TestStoreTouchesExisting(t *testing.T) {
	dir := t.TempDir()
	a, err := Store(dir, []byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	setAge(t, a.Path, 8*24*time.Hour)
	if _, err := Store(dir, []byte("content")); err != nil {
		t.Fatal(err)
	}
	if Prune(dir, PruneAge); !exists(a.Path) {
		t.Fatal("artifact in use was pruned")
	}
}

func TestCacheGetTouches(t *testing.T) {
	dir := t.TempDir()
	a, err := Store(dir, []byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	key := "ab" + a.SHA256
	if err := cachePut(dir, key, a.SHA256, nil); err != nil {
		t.Fatal(err)
	}
	setAge(t, a.Path, 8*24*time.Hour)
	setAge(t, cachePath(dir, key), 8*24*time.Hour)
	if _, ok := cacheGet(dir, key); !ok {
		t.Fatal("no cache hit")
	}
	if Prune(dir, PruneAge); !exists(a.Path) || !exists(cachePath(dir, key)) {
		t.Fatal("cache entry in use was pruned")
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Builds of an engine share a limited number of slots.
func TestBundleSlots(t *testing.T) {
	src := filepath.Join(t.TempDir(), "s.ts")
	if err := os.WriteFile(src, []byte("console.log(1);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var cur, peak atomic.Int32
	o := Options{StoreDir: t.TempDir(), NoCache: true, Slots: make(chan struct{}, 2),
		Runner: func(context.Context, Invocation) ([]byte, []byte, error) {
			n := cur.Add(1)
			defer cur.Add(-1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(50 * time.Millisecond)
			return []byte("// bundle\n"), nil, nil
		}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Bundle(context.Background(), o, src); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Errorf("%d builds at the same time, want 2", p)
	}

	// Waiting for a slot ends with the context.
	o.Slots = make(chan struct{}, 1)
	o.Slots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Bundle(ctx, o, src); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("full slots: %v", err)
	}
}
