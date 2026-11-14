package fcproto

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCleanTarget(t *testing.T) {
	ok := map[string]string{
		"/artifacts/a.js":    "/artifacts/a.js",
		"artifacts/x/y.js":   "/artifacts/x/y.js",
		"/artifacts":         "/artifacts",
		"/sdk/0/mod.ts":      "/sdk/0/mod.ts",
		"/denodir/npm/x/bin": "/denodir/npm/x/bin",
	}
	for in, want := range ok {
		roots := BoxRoots
		if !strings.Contains(in, "artifacts") {
			roots = BuildRoots
		}
		if _, got, err := CleanTarget(in, roots); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"/", "/etc/passwd", "/artifacts/../etc/x", "../artifacts/a", "/artifactsX/a", "/tmp/a"} {
		if _, _, err := CleanTarget(bad, BoxRoots); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAddTree(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.js"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "tool"), []byte("#!"), 0o755)
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("S"), 0o644)
	links := runtime.GOOS != "windows"
	if links {
		os.Symlink(outside, filepath.Join(dir, "leak"))                    // points outside: skipped
		os.Symlink(filepath.Join(dir, "a.js"), filepath.Join(dir, "b.js")) // inside: followed
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := AddTree(tw, dir, "/artifacts"); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
		if _, _, err := CleanTarget(h.Name, BoxRoots); err != nil {
			t.Errorf("entry outside the root: %s", h.Name)
		}
		if h.Name == "/artifacts/sub/tool" && runtime.GOOS != "windows" && h.Mode != 0o755 {
			t.Errorf("tool: mode %o", h.Mode)
		}
	}
	if got["/artifacts/a.js"] != "A" || got["/artifacts/sub/tool"] != "#!" {
		t.Fatalf("%v", got)
	}
	if _, ok := got["/artifacts/leak"]; ok {
		t.Fatal("symlink to the outside followed")
	}
	if links && got["/artifacts/b.js"] != "A" {
		t.Fatalf("symlink inside the tree not followed: %v", got)
	}
}

func TestMessages(t *testing.T) {
	var buf bytes.Buffer
	h := Header{Mode: ModeBuild, Args: []string{"bundle", "x.ts"}, MemoryBytes: 1 << 20}
	if err := WriteJSON(&buf, h); err != nil {
		t.Fatal(err)
	}
	var back Header
	if err := ReadJSON(&buf, MaxHeader, &back); err != nil || back.Mode != ModeBuild || len(back.Args) != 2 {
		t.Fatalf("%+v %v", back, err)
	}
	if err := WriteJSON(&buf, strings.Repeat("x", 100)); err != nil {
		t.Fatal(err)
	}
	var s string
	if err := ReadJSON(&buf, 10, &s); err == nil {
		t.Fatal("oversized message accepted")
	}
	buf.Reset()
	if err := WriteResult(&buf, Result{Exit: 3}, []byte("bundle"), []byte("diag")); err != nil {
		t.Fatal(err)
	}
	r, out, errb, err := ReadResult(&buf)
	if err != nil || r.Exit != 3 || string(out) != "bundle" || string(errb) != "diag" {
		t.Fatalf("%+v %q %q %v", r, out, errb, err)
	}
	if _, _, _, err := ReadResult(bytes.NewReader([]byte{0, 0})); err == nil {
		t.Fatal("truncated result accepted")
	}
}
