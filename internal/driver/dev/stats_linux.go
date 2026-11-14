package dev

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rif223/tsbox/internal/driver"
)

const clockTick = 10 * time.Millisecond // CLK_TCK = 100

// stats sums PSS, CPU time and threads over the process tree from pid.
func (*platform) stats(pid int) (driver.Stats, error) {
	st := driver.Stats{At: time.Now()}
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return st, err
	}
	children := map[int][]int{}
	ticks := map[int]int64{}
	threads := map[int]int{}
	for _, p := range stats {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		j := strings.IndexByte(s, '(')
		if i < 0 || j < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 18 {
			continue
		}
		self, _ := strconv.Atoi(strings.TrimSpace(s[:j]))
		ppid, _ := strconv.Atoi(f[1])
		ut, _ := strconv.ParseInt(f[11], 10, 64)
		stt, _ := strconv.ParseInt(f[12], 10, 64)
		n, _ := strconv.Atoi(f[17])
		children[ppid] = append(children[ppid], self)
		ticks[self] = ut + stt
		threads[self] = n
	}
	if _, ok := ticks[pid]; !ok {
		return st, fmt.Errorf("dev: process %d not found", pid)
	}
	tree := []int{pid}
	for i := 0; i < len(tree); i++ {
		tree = append(tree, children[tree[i]]...)
	}
	for _, p := range tree {
		st.CPUUsage += time.Duration(ticks[p]) * clockTick
		st.MemoryBytes += pssBytes(p)
		st.PIDs += threads[p]
	}
	return st, nil
}

func pssBytes(pid int) int64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "Pss:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb << 10
		}
	}
	return 0
}
