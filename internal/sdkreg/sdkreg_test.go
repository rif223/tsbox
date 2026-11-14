package sdkreg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rif223/tsbox/sdks"
)

func TestBuiltins(t *testing.T) {
	r := New(t.TempDir())
	for _, n := range sdks.Names {
		s, err := r.Resolve(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if s.Manifest.Name != n || s.Source != "builtin" {
			t.Fatalf("%+v", s)
		}
		if _, err := os.Stat(s.ModulePath()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Resolve("doesnotexist"); err == nil || !strings.Contains(err.Error(), "tsbox sdk add") {
		t.Fatalf("Meldung: %v", err)
	}
}

func writeSDK(t *testing.T, dir, name string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "tsbox-sdk.json"), []byte(`{"name":"`+name+`","version":"1.0.0","tsbox":"^0.1",
		"provider":{"exec":["./p"]},"methods":{"a":{}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "mod.ts"), []byte("export const a = 1;\n"), 0o644)
}

func TestAddTamperRemove(t *testing.T) {
	r := New(t.TempDir())
	src := filepath.Join(t.TempDir(), "demo")
	writeSDK(t, src, "demo")
	s, err := r.Add(src)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve("demo"); err != nil || got.Hash != s.Hash {
		t.Fatal(got, err)
	}
	// Changes after the installation are detected.
	os.WriteFile(filepath.Join(s.Dir, "mod.ts"), []byte("export const a = 2;\n"), 0o644)
	if _, err := r.Resolve("demo"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("tampering not detected: %v", err)
	}
	if err := r.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("demo"); err == nil {
		t.Fatal("still resolvable after Remove")
	}
	// Bundled names are reserved.
	kv := filepath.Join(t.TempDir(), "kv")
	writeSDK(t, kv, "kv")
	if _, err := r.Add(kv); err == nil {
		t.Fatal("name kv accepted")
	}
}

// TestAddRemoveAtomic: reinstalling replaces completely, leftovers of interrupted runs disappear, bundled SDKs
// cannot be removed and lock.json only changes once the files are gone.
func TestAddRemoveAtomic(t *testing.T) {
	r := New(t.TempDir())
	src := filepath.Join(t.TempDir(), "demo")
	writeSDK(t, src, "demo")
	os.WriteFile(filepath.Join(src, "old.txt"), []byte("x"), 0o644)
	if _, err := r.Add(src); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(src, "old.txt"))
	inst := filepath.Join(r.root, "installed")
	os.MkdirAll(filepath.Join(inst, ".old-demo-1"), 0o755) // leftover of an interrupted run
	s, err := r.Add(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "old.txt")); err == nil {
		t.Fatal("a file of the old version survived the reinstall")
	}
	// Readable for others: the build sandbox reads the SDK as UID 65534.
	if st, err := os.Stat(s.Dir); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm()&0o005 != 0o005) {
		t.Fatalf("installed directory not readable for others: %v %v", st.Mode(), err)
	}
	if got, err := r.Resolve("demo"); err != nil || got.Hash != s.Hash {
		t.Fatalf("after the reinstall: %v %v", got, err)
	}
	ents, _ := os.ReadDir(inst)
	if len(ents) != 1 || ents[0].Name() != "demo" {
		t.Fatalf("installed/: %v", ents)
	}
	if err := r.Remove("kv"); err == nil || !strings.Contains(err.Error(), "bundled") {
		t.Fatalf("Remove(kv): %v", err)
	}
	if err := r.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(inst); len(ents) != 0 {
		t.Fatalf("installed/ after Remove: %v", ents)
	}
	if err := r.Remove("demo"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("second Remove: %v", err)
	}
}
