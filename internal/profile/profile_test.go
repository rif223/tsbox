package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeRootfs(t *testing.T, dir, deb string) {
	t.Helper()
	for _, p := range []string{Deno, "etc/debian_version"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, Deno), []byte("deno"), 0o755)
	os.WriteFile(filepath.Join(dir, "etc/debian_version"), []byte(deb+"\n"), 0o644)
}

func TestSet(t *testing.T) {
	tmp := t.TempDir()
	s := Set{RootFS: filepath.Join(tmp, "rootfs"), Dir: filepath.Join(tmp, "profiles")}
	fakeRootfs(t, s.RootFS, "12.7")
	fakeRootfs(t, filepath.Join(s.Dir, "next"), "13.10")
	os.MkdirAll(filepath.Join(s.Dir, "empty"), 0o755)      // without Deno: not a profile
	os.MkdirAll(filepath.Join(s.Dir, "Invalid"), 0o755)    // wrong name
	fakeRootfs(t, filepath.Join(s.Dir, "default"), "11.0") // never hides the default rootfs

	for name, want := range map[string]string{"": s.RootFS, "default": s.RootFS, "next": filepath.Join(s.Dir, "next")} {
		if p, err := s.Resolve(name); err != nil || p != want {
			t.Errorf("Resolve(%q) = %q, %v", name, p, err)
		}
	}
	for _, name := range []string{"empty", "missing", "../rootfs", "a/b", "X", strings.Repeat("a", 33)} {
		if _, err := s.Resolve(name); err == nil {
			t.Errorf("Resolve(%q) without error", name)
		}
	}
	list := s.List()
	if len(list) != 2 || list[0].Name != "default" || list[1].Name != "next" || list[1].Debian != "13.10" {
		t.Fatalf("List: %+v", list)
	}
	if list[0].Hash == list[1].Hash || len(list[0].Hash) != 16 || list[0].Entries < 4 {
		t.Fatalf("Hashes: %+v", list)
	}
	// The hash changes with the content (size) and is stable otherwise.
	h1, _, _ := Hash(s.RootFS)
	if h2, _, _ := Hash(s.RootFS); h1 != h2 {
		t.Fatal("hash not stable")
	}
	os.WriteFile(filepath.Join(s.RootFS, Deno), []byte("deno 2"), 0o755)
	if h3, _, _ := Hash(s.RootFS); h3 == h1 {
		t.Fatal("hash does not change")
	}
}
