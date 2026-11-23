package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got, err := FileSHA256(p); err != nil || got != want {
		t.Fatalf("%s, %v", got, err)
	}
	if _, err := FileSHA256(p + "-missing"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	for p, want := range map[string]bool{
		root:                                 true,
		filepath.Join(root, "a", "b"):        true,
		filepath.Join(root, "..dir"):         true,
		filepath.Dir(root):                   false,
		root + "-sibling":                    false,
		filepath.Join(root, "..", "outside"): false,
	} {
		if got := Within(root, p); got != want {
			t.Errorf("Within(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestLimitWriter(t *testing.T) {
	var b bytes.Buffer
	w := &LimitWriter{W: &b, N: 4}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("de")); err == nil {
		t.Fatal("limit exceeded without an error")
	}
	if _, err := w.Write([]byte("d")); err != nil || b.String() != "abcd" {
		t.Fatalf("%q, %v", b.String(), err)
	}
}
