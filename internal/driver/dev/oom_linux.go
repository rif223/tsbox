package dev

import (
	"os"
	"strconv"
	"strings"
)

// setOOMScoreAdj applies to init and (inherited) all scripts. Without CAP_SYS_RESOURCE a process may only
// raise the value; if it is already higher (e.g. in a container), it stays.
func setOOMScoreAdj(pid, adj int) error {
	p := "/proc/" + strconv.Itoa(pid) + "/oom_score_adj"
	if b, err := os.ReadFile(p); err == nil {
		if cur, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && cur >= adj {
			return nil
		}
	}
	return os.WriteFile(p, []byte(strconv.Itoa(adj)), 0)
}
