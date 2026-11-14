// Package profile manages named profiles: rootfs variants for the gVisor driver (e.g. with a different
// Deno version). "default" is the engine's rootfs (TSBOX_ROOTFS or /var/lib/tsbox/rootfs), every other
// profile a directory <profiles dir>/<name> (TSBOX_PROFILES or /var/lib/tsbox/profiles).
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Default is the name of the default profile.
const Default = "default"

// Deno is the path of Deno in every profile.
const Deno = "usr/local/bin/deno"

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// ValidName checks a profile name ("" stands for default).
func ValidName(name string) error {
	if name != "" && !namePattern.MatchString(name) {
		return fmt.Errorf("invalid profile name %q (allowed: a-z, 0-9, _ . -, 1-32 characters)", name)
	}
	return nil
}

// Set is the set of profiles of an engine.
type Set struct {
	RootFS string // rootfs of "default"
	Dir    string // directory of the other profiles
}

// Info describes a profile.
type Info struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Hash    string `json:"hash"`    // fingerprint (hash), 16 hex characters
	Entries int    `json:"entries"` // number of entries in the rootfs
	Debian  string `json:"debian,omitempty"`
}

// Path returns the rootfs directory of a profile without checking whether it exists.
func (s Set) Path(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	if name == "" || name == Default {
		return s.RootFS, nil
	}
	return filepath.Join(s.Dir, name), nil
}

// Resolve returns the rootfs directory of an existing profile.
func (s Set) Resolve(name string) (string, error) {
	p, err := s.Path(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(p, Deno)); err != nil {
		if name == "" {
			name = Default
		}
		return "", fmt.Errorf("profile %q not found: %s missing (scripts/build-rootfs.sh with PROFILE=%s)", name, filepath.Join(p, Deno), name)
	}
	return p, nil
}

// List returns all existing profiles, default first, the others sorted by name.
func (s Set) List() []Info {
	var out []Info
	add := func(name, dir string) {
		if _, err := os.Stat(filepath.Join(dir, Deno)); err != nil {
			return
		}
		h, n, _ := Hash(dir)
		deb, _ := os.ReadFile(filepath.Join(dir, "etc/debian_version"))
		out = append(out, Info{Name: name, Path: dir, Hash: h, Entries: n, Debian: firstLine(deb)})
	}
	add(Default, s.RootFS)
	ents, _ := os.ReadDir(s.Dir)
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if namePattern.MatchString(e.Name()) && e.Name() != Default {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		add(n, filepath.Join(s.Dir, n))
	}
	return out
}

// Hash is a fast fingerprint of a rootfs (paths, sizes, permissions, link targets). A profile may be a
// symlink to a directory.
func Hash(dir string) (string, int, error) {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	h := sha256.New()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		switch filepath.ToSlash(rel) {
		case "proc", "sys", "dev", "artifacts", ".tsbox":
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		n++
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", filepath.ToSlash(rel), info.Mode(), info.Size())
		if info.Mode()&fs.ModeSymlink != 0 {
			t, _ := os.Readlink(p)
			h.Write([]byte(t))
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:16], n, err
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' || c == '\r' {
			return string(b[:i])
		}
	}
	return string(b)
}
