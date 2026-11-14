// Package cgroup reads and sets cgroup v2 values of a sandbox (Linux). The functions work on a directory
// path and can therefore be tested on any platform with test directories.
package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Root is the root of the cgroup v2 hierarchy.
const Root = "/sys/fs/cgroup"

// Stats are the raw values of a cgroup.
type Stats struct {
	MemoryCurrent int64 // bytes
	MemoryPeak    int64 // bytes; 0 if memory.peak is missing (kernel < 5.19)
	MemoryMax     int64 // bytes; -1 = unlimited
	OOMKills      int
	CPUUsage      time.Duration
	CPUThrottled  time.Duration
	PIDs          int
}

// Read reads the values of the cgroup in dir.
func Read(dir string) (Stats, error) {
	var s Stats
	var err error
	if s.MemoryCurrent, err = readInt(dir, "memory.current"); err != nil {
		return s, err
	}
	if v, err := readInt(dir, "memory.peak"); err == nil {
		s.MemoryPeak = v
	}
	if s.MemoryMax, err = readMax(dir, "memory.max"); err != nil {
		return s, err
	}
	if ev, err := readKV(dir, "memory.events"); err == nil {
		s.OOMKills = int(ev["oom_kill"])
	}
	cpu, err := readKV(dir, "cpu.stat")
	if err != nil {
		return s, err
	}
	s.CPUUsage = time.Duration(cpu["usage_usec"]) * time.Microsecond
	s.CPUThrottled = time.Duration(cpu["throttled_usec"]) * time.Microsecond
	if v, err := readInt(dir, "pids.current"); err == nil {
		s.PIDs = int(v)
	}
	return s, nil
}

// Limits sets memory.max (without swap), cpu.max and pids.max.
func Limits(dir string, memoryBytes int64, cpuCores float64, pids int) error {
	const period = 100_000 // µs
	quota := int64(cpuCores * period)
	writes := []struct{ file, value string }{
		{"memory.max", strconv.FormatInt(memoryBytes, 10)},
		{"memory.swap.max", "0"},
		{"cpu.max", fmt.Sprintf("%d %d", quota, period)},
		{"pids.max", strconv.Itoa(pids)},
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.file), []byte(w.value), 0o644); err != nil {
			if w.file == "memory.swap.max" && errors.Is(err, os.ErrNotExist) {
				continue // kernel without swap accounting
			}
			return fmt.Errorf("cgroup: %s: %w", w.file, err)
		}
	}
	return nil
}

func readInt(dir, file string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// readMax reads files like memory.max that may contain "max" for unlimited.
func readMax(dir, file string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0, err
	}
	v := strings.TrimSpace(string(b))
	if v == "max" {
		return -1, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// readKV reads "key value" files like cpu.stat or memory.events.
func readKV(dir, file string) (map[string]int64, error) {
	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int64{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out, s.Err()
}

// Kill ends all processes of the cgroup (cgroup v2, kernel ≥ 5.14). Works even if runsc itself no
// longer responds (spike f).
func Kill(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
}

// SetCPU changes cpu.max of a running cgroup (CPU boost at start, Limits.StartCPU).
func SetCPU(dir string, cpuCores float64) error {
	const period = 100_000 // µs
	v := fmt.Sprintf("%d %d", int64(cpuCores*period), period)
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(v), 0o644); err != nil {
		return fmt.Errorf("cgroup: cpu.max: %w", err)
	}
	return nil
}
