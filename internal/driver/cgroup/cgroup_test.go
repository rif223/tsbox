package cgroup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"memory.current": "52428800\n",
		"memory.peak":    "73400320\n",
		"memory.max":     "134217728\n",
		"memory.events":  "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n",
		"cpu.stat":       "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\nnr_throttled 4\nthrottled_usec 250000\n",
		"pids.current":   "17\n",
	})
	s, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{
		MemoryCurrent: 50 << 20, MemoryPeak: 70 << 20, MemoryMax: 128 << 20, OOMKills: 1,
		CPUUsage: 1500 * time.Millisecond, CPUThrottled: 250 * time.Millisecond, PIDs: 17,
	}
	if s != want {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
}

func TestReadUnlimitedAndOldKernel(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{ // without memory.peak, memory.events, pids.current
		"memory.current": "1024", "memory.max": "max", "cpu.stat": "usage_usec 10",
	})
	s, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryMax != -1 || s.MemoryPeak != 0 || s.CPUUsage != 10*time.Microsecond {
		t.Fatalf("got %+v", s)
	}
}

func TestLimits(t *testing.T) {
	dir := t.TempDir()
	if err := Limits(dir, 256<<20, 0.5, 64); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"memory.max": "268435456", "memory.swap.max": "0", "cpu.max": "50000 100000", "pids.max": "64",
	} {
		b, _ := os.ReadFile(filepath.Join(dir, file))
		if string(b) != want {
			t.Errorf("%s = %q, want %q", file, b, want)
		}
	}
}

// FuzzRead: contents of the cgroup files (from the kernel, under gVisor partly from runsc) are read
// without crashing.
func FuzzRead(f *testing.F) {
	f.Add("123", "max", "usage_usec 5\nuser_usec 1", "oom_kill 2\noom 3", "7")
	f.Add("x", "-1", "usage_usec", "oom_kill 99999999999999999999", "")
	f.Fuzz(func(t *testing.T, cur, peak, cpu, events, pids string) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"memory.current": cur, "memory.peak": peak, "cpu.stat": cpu,
			"memory.events": events, "pids.current": pids, "memory.max": peak, "pids.max": pids})
		_, _ = Read(dir)
	})
}

func TestSetCPU(t *testing.T) {
	dir := t.TempDir()
	if err := SetCPU(dir, 0.5); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cpu.max")); string(b) != "50000 100000" {
		t.Fatalf("cpu.max = %q", b)
	}
}

func TestInstanceParent(t *testing.T) {
	a, b := InstanceParent("/var/lib/tsbox"), InstanceParent("/tmp/other")
	if a == b || a != InstanceParent("/var/lib/tsbox/") || len(a) != len("/tsbox/")+8 || a[:7] != "/tsbox/" {
		t.Fatalf("%q %q", a, b)
	}
}
