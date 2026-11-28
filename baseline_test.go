package tsbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBaselines(t *testing.T) {
	dir := t.TempDir()
	init := filepath.Join(dir, "tsbox")
	if err := os.WriteFile(init, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: config{stateDir: dir, init: init}}
	if e.baselineKey("gvisor", "") != "" {
		t.Fatal("key without a profile fingerprint")
	}
	key := e.baselineKey("gvisor", "abc")
	if key == "" || e.baselineRSS(key) != 0 {
		t.Fatalf("new key %q: %d", key, e.baselineRSS(key))
	}
	for _, bad := range []int64{0, -1, maxBaselineRSS + 1} {
		e.rememberBaselineRSS(key, bad)
	}
	if v := e.baselineRSS(key); v != 0 {
		t.Fatalf("invalid value kept: %d", v)
	}
	e.rememberBaselineRSS(key, 110<<20)
	// A new engine on the same state directory knows the value (daemon restart).
	e2 := &Engine{cfg: config{stateDir: dir, init: init}}
	if v := e2.baselineRSS(key); v != 110<<20 {
		t.Fatalf("after reload: %d", v)
	}
	// Another init binary (tsbox update) gets another key.
	if err := os.WriteFile(init, []byte("another binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if k2 := e2.baselineKey("gvisor", "abc"); k2 == key || e2.baselineRSS(k2) != 0 {
		t.Fatalf("key after an update: %q", k2)
	}
	// A damaged file means: measure again.
	if err := os.WriteFile(e.baselinePath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := (&Engine{cfg: config{stateDir: dir, init: init}}).baselineRSS(key); v != 0 {
		t.Fatalf("damaged file: %d", v)
	}
}
