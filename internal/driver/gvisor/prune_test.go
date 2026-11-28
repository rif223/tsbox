package gvisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// pruneInits keeps the current copy and those running boxes use (their bundle refers to them).
func TestPruneInits(t *testing.T) {
	state := t.TempDir()
	d := &Driver{cfg: Config{StateDir: state}}
	dir := func(name string) string {
		p := filepath.Join(state, "init", name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "tsbox"), []byte("x"), 0o555); err != nil {
			t.Fatal(err)
		}
		return p
	}
	current, used, old := dir("aaaaaaaaaaaaaaaa"), dir("bbbbbbbbbbbbbbbb"), dir("cccccccccccccccc")
	bundle := filepath.Join(state, "bundles", "box1")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"mounts": []map[string]string{{"destination": "/.tsbox", "source": used}}})
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	d.pruneInits(current)
	for p, want := range map[string]bool{current: true, used: true, old: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}
