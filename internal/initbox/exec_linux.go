package initbox

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// ExecMain is "tsbox __exec <uid> <nproc> -- cmd…": still runs as UID 0 of the sandbox, sets RLIMIT_NPROC
// (counts threads per UID, spike c), switches UID/GID without supplementary groups, sets no_new_privs and
// replaces itself with cmd. With the UID switch the process loses all capabilities.
func ExecMain(args []string) int {
	if len(args) < 4 || args[2] != "--" {
		fmt.Fprintln(os.Stderr, "usage: tsbox __exec <uid> <nproc> -- cmd...")
		return 2
	}
	uid, err1 := strconv.Atoi(args[0])
	nproc, err2 := strconv.Atoi(args[1])
	if err1 != nil || err2 != nil || uid < 1 {
		fmt.Fprintln(os.Stderr, "tsbox __exec: invalid uid/nproc")
		return 2
	}
	cmd := args[3:]
	fail := func(what string, err error) int {
		fmt.Fprintf(os.Stderr, "tsbox __exec: %s: %v\n", what, err)
		return 126
	}
	if nproc > 0 {
		lim := syscall.Rlimit{Cur: uint64(nproc), Max: uint64(nproc)}
		if err := syscall.Setrlimit(6 /* RLIMIT_NPROC */, &lim); err != nil {
			return fail("setrlimit", err)
		}
	}
	core := syscall.Rlimit{}
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &core)
	if err := syscall.Setgroups(nil); err != nil {
		return fail("setgroups", err)
	}
	if err := syscall.Setgid(uid); err != nil {
		return fail("setgid", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fail("setuid", err)
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, 38 /* PR_SET_NO_NEW_PRIVS */, 1, 0); e != 0 {
		return fail("no_new_privs", e)
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return fail("uid", fmt.Errorf("UID switch had no effect"))
	}
	err := syscall.Exec(cmd[0], cmd, os.Environ())
	return fail("exec "+cmd[0], err)
}

// OOMExecMain is "tsbox __oomexec <adj> -- cmd…" on the host: sets oom_score_adj and replaces itself with
// cmd (runsc run). This way sentry, gofer and all systrap stubs of the box inherit the value from the start;
// with the same value for all of them the cgroup OOM still hits the largest script (as without the change).
func OOMExecMain(args []string) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "usage: tsbox __oomexec <adj> -- cmd...")
		return 2
	}
	adj, err := strconv.Atoi(args[0])
	if err != nil || adj < -1000 || adj > 1000 {
		fmt.Fprintln(os.Stderr, "tsbox __oomexec: invalid value")
		return 2
	}
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(strconv.Itoa(adj)), 0); err != nil {
		fmt.Fprintln(os.Stderr, "tsbox __oomexec:", err)
		return 1
	}
	err = syscall.Exec(args[2], args[2:], os.Environ())
	fmt.Fprintln(os.Stderr, "tsbox __oomexec: exec:", err)
	return 1
}
