package dev

import (
	"os"
	"testing"
)

func TestImageOverhead(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if n := imageOverhead(exe); n <= 0 || n%4096 != 0 {
		t.Fatalf("overhead of the test binary: %d", n)
	}
	if n := imageOverhead(`C:\does\not\exist.exe`); n != 0 {
		t.Fatalf("missing file: %d", n)
	}
}
