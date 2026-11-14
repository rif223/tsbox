package initbox

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

func procInfo(pid int) (pinfo, error) {
	var pi pinfo
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return pi, err
	}
	parseStatus(b, &pi)
	if st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		parseStat(st, &pi)
	}
	return pi, nil
}
func hasRssAnon() bool {
	b, err := os.ReadFile("/proc/self/status")
	return err == nil && strings.Contains(string(b), "RssAnon:")
}

func signalName(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
