//go:build linux

package firecracker

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestConsoleWriter(t *testing.T) {
	var out bytes.Buffer
	c := &consoleWriter{w: &out}
	in := "[    1.268263] reboot: Restarting system\r\n" + "tsbox __fcinit: mount /tmp: busy\r\n" + "\r\n" + "init: "
	for i := 0; i < len(in); i++ { // the serial console delivers byte by byte
		c.Write([]byte{in[i]})
	}
	c.Write([]byte("ready\n"))
	if got := out.String(); got != "tsbox __fcinit: mount /tmp: busy\ninit: ready\n" {
		t.Fatalf("%q", got)
	}
}

func TestVCPUs(t *testing.T) {
	n := runtime.NumCPU()
	for cpu, want := range map[float64]int{0.5: min(2, n), 1: min(2, n), 2.5: min(3, n), 64: n} {
		if got := vcpus(cpu); got != want {
			t.Errorf("vcpus(%g) = %d, want %d", cpu, got, want)
		}
	}
}

func TestPruneImages(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration, init string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o644)
		if init != "" {
			os.WriteFile(p+initSuffix, []byte(init), 0o600)
		}
		at := time.Now().Add(-age)
		os.Chtimes(p, at, at)
		return p
	}
	keep := mk("current.ext4", 30*24*time.Hour, "") // in use right now, even if old and without record
	recent := mk("recent.ext4", time.Hour, "h1")    // another profile of this binary
	old := mk("old.ext4", 8*24*time.Hour, "h1")
	other := mk("other.ext4", time.Hour, "h0") // an older tsbox binary: never used again
	legacy := mk("legacy.ext4", time.Hour, "") // built before the records existed
	tmp := mk("half.ext4.tmp", time.Minute, "")
	d := &Driver{}
	d.pruneImages(dir, keep, "h1")
	for p, want := range map[string]bool{keep: true, recent: true, recent + initSuffix: true, old: false,
		other: false, other + initSuffix: false, legacy: false, tmp: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
	os.Remove(recent)
	mk("again-old.ext4", 9*24*time.Hour, "h1")
	d.pruneImages(dir, keep, "h1") // at most once per hour
	if _, err := os.Stat(filepath.Join(dir, "again-old.ext4")); err != nil {
		t.Fatal("pruned twice within an hour")
	}
}
