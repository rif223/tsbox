package initbox

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/rif223/tsbox/internal/proto"
)

// ProbeMain is "tsbox __probe": a simulated escaped script process (native code instead of Deno) that only
// tests start via Start.DebugExec. It attacks sibling processes and the network and reports every result
// as a log frame "PROBE <name> <result>" (invariants I3 and I11).
func ProbeMain([]string) int {
	report := func(name string, err error) {
		res := "DENIED"
		if err == nil {
			res = "ALLOWED"
		}
		_ = proto.Write(os.Stdout, &proto.Frame{T: proto.TLog, Level: "info", Msg: fmt.Sprintf("PROBE %s %s", name, res)})
	}
	self := os.Getpid()
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self || pid == 1 {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat("/proc/"+e.Name(), &st) != nil || int(st.Uid) == os.Getuid() {
			continue
		}
		report("kill-"+e.Name(), syscall.Kill(pid, 0))
		report("ptrace-"+e.Name(), syscall.PtraceAttach(pid))
		_, err = os.ReadFile("/proc/" + e.Name() + "/environ")
		report("environ-"+e.Name(), err)
		f, err := os.Open("/proc/" + e.Name() + "/mem")
		if f != nil {
			f.Close()
		}
		report("mem-"+e.Name(), err)
	}
	report("kill-init", syscall.Kill(1, 0))
	report("setuid0", syscall.Setuid(0))
	report("write-root", os.WriteFile("/probe", []byte("x"), 0o600))
	report("write-tmp", os.WriteFile("/tmp/probe", []byte("x"), 0o600))
	for _, addr := range []string{"1.1.1.1:53", "10.0.0.1:80", "[2606:4700:4700::1111]:53"} {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if c != nil {
			c.Close()
		}
		report("tcp-"+addr, err)
	}
	u, err := net.DialTimeout("udp", "1.1.1.1:53", time.Second)
	if err == nil {
		_, err = u.Write([]byte{0})
		u.Close()
	}
	report("udp", err)
	_, err = net.LookupHost("example.com")
	report("dns", err)
	report("done", fmt.Errorf("end"))
	return 0
}
