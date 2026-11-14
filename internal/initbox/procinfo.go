package initbox

import (
	"strconv"
	"strings"
	"time"
)

// pinfo are measurements of a script process.
type pinfo struct {
	vmRSS      int64 // resident (under gVisor including the mapped binary)
	private    int64 // private memory, if the system provides it
	hasPrivate bool
	threads    int // 0 = unknown
	cpu        time.Duration
}

const clockTick = 10 * time.Millisecond // CLK_TCK = 100

// parseStatus reads VmRSS, RssAnon and Threads from /proc/<pid>/status.
func parseStatus(b []byte, pi *pinfo) {
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "VmRSS":
			pi.vmRSS = kb(v)
		case "RssAnon":
			pi.private, pi.hasPrivate = kb(v), true
		case "Threads":
			pi.threads, _ = strconv.Atoi(v)
		}
	}
}

// parseStat reads utime + stime from /proc/<pid>/stat (comm may contain spaces and parentheses).
func parseStat(b []byte, pi *pinfo) {
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		if f := strings.Fields(s[i+1:]); len(f) > 12 {
			ut, _ := strconv.ParseInt(f[11], 10, 64)
			stt, _ := strconv.ParseInt(f[12], 10, 64)
			pi.cpu = time.Duration(ut+stt) * clockTick
		}
	}
}

func kb(v string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(v, "kB")), 10, 64)
	if n < 0 || n > 1<<52 {
		return 0
	}
	return n << 10
}
