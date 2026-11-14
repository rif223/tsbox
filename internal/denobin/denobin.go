// Package denobin finds the host's Deno binary: TSBOX_DENO, then PATH (real binaries only), then the npm
// installation on Windows. The npm wrapper "deno" (a shell script) cannot be started from Go and is
// skipped.
package denobin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ErrNotFound explains how to provide Deno.
var ErrNotFound = errors.New("deno not found: install Deno 2.9 (https://deno.com) or set TSBOX_DENO=<path>")

// Find returns the absolute path of deno.
func Find() (string, error) {
	if p := os.Getenv("TSBOX_DENO"); p != "" {
		if !isBinary(p) {
			return "", fmt.Errorf("TSBOX_DENO=%s is not an executable Deno binary", p)
		}
		return filepath.Abs(p)
	}
	if p, err := exec.LookPath("deno"); err == nil && isBinary(p) {
		return filepath.Abs(p)
	}
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			for _, arch := range []string{"win32-arm64", "win32-x64"} {
				p := filepath.Join(appdata, "npm", "node_modules", "deno", "node_modules", "@deno", arch, "deno.exe")
				if isBinary(p) {
					return p, nil
				}
			}
		}
	}
	return "", ErrNotFound
}

// isBinary recognizes real executables (PE or ELF/Mach-O header) instead of shell wrappers.
func isBinary(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [4]byte
	if _, err := f.Read(hdr[:]); err != nil {
		return false
	}
	switch {
	case hdr[0] == 'M' && hdr[1] == 'Z': // PE (Windows)
		return true
	case hdr == [4]byte{0x7f, 'E', 'L', 'F'}:
		return true
	case hdr == [4]byte{0xcf, 0xfa, 0xed, 0xfe}, hdr == [4]byte{0xca, 0xfe, 0xba, 0xbe}: // Mach-O
		return true
	}
	return false
}
