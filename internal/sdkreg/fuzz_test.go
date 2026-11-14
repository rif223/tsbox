package sdkreg

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLock: a damaged lock.json leads to errors, never to a crash or a path outside the installation
// directory.
func FuzzLock(f *testing.F) {
	f.Add([]byte(`{"echo":{"source":"/x","hash":"ab"}}`), "echo")
	f.Add([]byte(`{"../../etc":{"source":"x","hash":""}}`), "../../etc")
	f.Add([]byte(`[1]`), "kv")
	f.Fuzz(func(t *testing.T, lock []byte, name string) {
		state := t.TempDir()
		r := New(state)
		os.MkdirAll(r.root, 0o755)
		os.WriteFile(r.lockPath(), lock, 0o644)
		if s, err := r.Resolve(name); err == nil && !IsPath(name) {
			rel, rerr := filepath.Rel(state, s.Dir)
			if s.Source != "builtin" && (rerr != nil || filepath.IsAbs(rel) || rel == ".." || len(rel) > 1 && rel[:2] == "..") {
				t.Fatalf("SDK %q outside the state: %s", name, s.Dir)
			}
		}
		_, _ = r.List()
	})
}
