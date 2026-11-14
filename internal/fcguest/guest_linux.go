//go:build linux

// Package fcguest is tsbox inside a Firecracker microVM: "tsbox __fcinit" is PID 1 (the kernel starts it
// with init=/.tsbox/tsbox -- __fcinit), "tsbox __fcexec" reduces rights before an exec.
//
// PID 1 prepares a minimal, read-only system and then talks to the host only over vsock (fcproto):
//   - /proc with hidepid=2 (scripts do not see each other), /sys, cgroup v2, a /dev with only null, zero,
//     full, random and urandom, /tmp empty and read-only (box) or a small writable tmpfs (build)
//   - hardening sysctls: no user namespaces, no unprivileged BPF, restricted dmesg/kallsyms/perf
//   - the files of the task (artifacts or build inputs) arrive as a tar stream and are mounted read-only
//   - box mode: a box cgroup with the memory and PIDs limits; inside it "tsbox __init --uid-mode" runs as
//     UID 0 with only SETUID/SETGID/KILL and no_new_privs; its stdin/stdout is the vsock channel
//   - build mode: deno runs as UID/GID 65534 without capabilities; the result goes back to the host
//
// When the task ends, PID 1 restarts the VM; with reboot=k Firecracker then ends.
package fcguest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/rif223/tsbox/internal/driver/cgroup"
	"github.com/rif223/tsbox/internal/fcproto"
)

const (
	self     = "/.tsbox/tsbox"
	deno     = "/usr/local/bin/deno"
	boxCG    = "/sys/fs/cgroup/box"
	envPath  = "PATH=/usr/local/bin:/usr/bin:/bin"
	nobody   = 65534
	maxBuild = 16 << 20
)

// InitMain is "tsbox __fcinit" (PID 1 of the microVM). It never returns: at the end it restarts the VM.
func InitMain(args []string) int {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "tsbox __fcinit: only as PID 1 of a Firecracker microVM")
		return 2
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tsbox __fcinit:", err)
	}
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	select {}
}

func run() error {
	if err := mountBase(); err != nil {
		return err
	}
	harden()
	setup, err := dial(fcproto.PortSetup)
	if err != nil {
		return err
	}
	defer setup.Close()
	var h fcproto.Header
	if err := fcproto.ReadJSON(setup, fcproto.MaxHeader, &h); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if h.Hostname != "" {
		_ = unix.Sethostname([]byte(h.Hostname))
	}
	var roots []string
	switch h.Mode {
	case fcproto.ModeBox:
		roots = fcproto.BoxRoots
		err = mountTmp(true)
	case fcproto.ModeBuild:
		roots = fcproto.BuildRoots
		err = mountTmp(false)
	default:
		return fmt.Errorf("unknown mode %q", h.Mode)
	}
	if err != nil {
		return err
	}
	if err := receive(setup, roots); err != nil {
		return fmt.Errorf("files: %w", err)
	}
	cg, err := boxCgroup(h.MemoryBytes, h.PIDs)
	if err != nil {
		return err
	}
	defer cg.Close()
	if h.Mode == fcproto.ModeBuild {
		return runBuild(h, setup, cg)
	}
	return runBox(cg)
}

// mountBase mounts the pseudo file systems. The rootfs itself stays read-only.
func mountBase() error {
	const nse = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	steps := []struct {
		src, dst, typ, data string
		flags               uintptr
	}{
		{"proc", "/proc", "proc", "hidepid=2", nse},
		{"sysfs", "/sys", "sysfs", "", nse},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", "", nse},
		{"tmpfs", "/dev", "tmpfs", "mode=755,size=64k", unix.MS_NOSUID | unix.MS_NOEXEC},
	}
	for _, s := range steps {
		if err := unix.Mount(s.src, s.dst, s.typ, s.flags, s.data); err != nil {
			return fmt.Errorf("mount %s: %w", s.dst, err)
		}
	}
	for _, d := range []struct {
		name         string
		major, minor uint32
	}{{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7}, {"random", 1, 8}, {"urandom", 1, 9}} {
		p := "/dev/" + d.name
		if err := unix.Mknod(p, unix.S_IFCHR|0o666, int(unix.Mkdev(d.major, d.minor))); err != nil {
			return fmt.Errorf("mknod %s: %w", p, err)
		}
		_ = os.Chmod(p, 0o666)
	}
	for name, target := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2"} {
		_ = os.Symlink(target, "/dev/"+name)
	}
	return unix.Mount("", "/dev", "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NOEXEC, "")
}

// mountTmp: an empty read-only /tmp (box: scripts share no writable directory, spike finding 4) or a small
// writable one (build).
func mountTmp(readonly bool) error {
	const nse = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	if readonly {
		if err := unix.Mount("tmpfs", "/tmp", "tmpfs", nse, "size=4k,mode=555"); err != nil {
			return fmt.Errorf("mount /tmp: %w", err)
		}
		return unix.Mount("", "/tmp", "", unix.MS_REMOUNT|unix.MS_RDONLY|nse, "")
	}
	return unix.Mount("tmpfs", "/tmp", "tmpfs", nse, "size=64m,mode=1777")
}

// harden sets sysctls that make the guest kernel harder to attack from a script. Missing ones are skipped.
func harden() {
	for k, v := range map[string]string{
		"user/max_user_namespaces":         "0",
		"kernel/unprivileged_bpf_disabled": "1",
		"kernel/dmesg_restrict":            "1",
		"kernel/kptr_restrict":             "2",
		"kernel/perf_event_paranoid":       "3",
		"kernel/sysrq":                     "0",
		"fs/protected_symlinks":            "1",
		"fs/protected_hardlinks":           "1",
	} {
		_ = os.WriteFile("/proc/sys/"+k, []byte(v), 0)
	}
}

// receive extracts the tar stream: every root gets its own tmpfs, which becomes read-only afterwards.
func receive(r io.Reader, roots []string) error {
	// noexec everywhere except the DENO_DIR of a build: esbuild is a binary in it (as under gVisor).
	flags := func(root string) uintptr {
		f := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
		if root != "/denodir" {
			f |= unix.MS_NOEXEC
		}
		return f
	}
	for _, root := range roots {
		if err := unix.Mount("tmpfs", root, "tmpfs", flags(root), "mode=755,size=512m"); err != nil {
			return fmt.Errorf("mount %s: %w", root, err)
		}
	}
	tr := tar.NewReader(r)
	var files int
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		_, p, err := fcproto.CleanTarget(hdr.Name, roots)
		if err != nil {
			return err
		}
		if files++; files > fcproto.MaxFiles {
			return errors.New("too many files")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += hdr.Size; total > fcproto.MaxTree {
				return errors.New("files too large")
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, tr, hdr.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
	for _, root := range roots {
		if err := unix.Mount("", root, "", unix.MS_REMOUNT|unix.MS_RDONLY|flags(root), ""); err != nil {
			return fmt.Errorf("remount %s: %w", root, err)
		}
	}
	return nil
}

// boxCgroup creates the cgroup of all processes of the task with the memory and PIDs limits.
func boxCgroup(memory int64, pids int) (*os.File, error) {
	if err := os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+memory +pids +cpu"), 0); err != nil {
		return nil, fmt.Errorf("cgroup controllers: %w", err)
	}
	if err := os.Mkdir(boxCG, 0o755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if memory > 0 {
		if err := os.WriteFile(boxCG+"/memory.max", []byte(strconv.FormatInt(memory, 10)), 0); err != nil {
			return nil, fmt.Errorf("memory.max: %w", err)
		}
		_ = os.WriteFile(boxCG+"/memory.swap.max", []byte("0"), 0)
	}
	if pids > 0 {
		if err := os.WriteFile(boxCG+"/pids.max", []byte(strconv.Itoa(pids)), 0); err != nil {
			return nil, fmt.Errorf("pids.max: %w", err)
		}
	}
	return os.Open(boxCG)
}

// runBox starts tsbox-init in the box cgroup with the vsock channel as stdin/stdout and serves the stats.
func runBox(cg *os.File) error {
	ch, err := dial(fcproto.PortChannel)
	if err != nil {
		return err
	}
	defer ch.Close()
	st, err := dial(fcproto.PortStats)
	if err != nil {
		return err
	}
	go serveStats(st)
	cmd := exec.Command(self, "__fcexec", "--caps", "setuid,setgid,kill", "--", self, "__init", "--deno", deno, "--uid-mode")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, os.Stderr
	cmd.Env = []string{"NO_COLOR=1", envPath}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd()), Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting init: %w", err)
	}
	reap(cmd.Process.Pid)
	drain(ch, 3*time.Second)
	return nil
}

// drain closes the writing direction of c and waits until the host closes it (at most d): data the guest
// has written is otherwise lost when the VM ends right away.
func drain(c *os.File, d time.Duration) {
	_ = unix.Shutdown(int(c.Fd()), unix.SHUT_WR)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// reap waits for pid and collects all other orphans PID 1 inherits meanwhile.
func reap(pid int) {
	for {
		var ws unix.WaitStatus
		p, err := unix.Wait4(-1, &ws, 0, nil)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		if p == pid {
			return
		}
	}
}

func serveStats(c *os.File) {
	defer c.Close()
	r := bufio.NewReader(c)
	enc := json.NewEncoder(c)
	for {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		var s fcproto.Stats
		if cs, err := cgroup.Read(boxCG); err == nil {
			s = fcproto.Stats{MemoryCurrent: cs.MemoryCurrent, MemoryPeak: cs.MemoryPeak, OOMKills: cs.OOMKills,
				PIDs: cs.PIDs, CPUUsage: cs.CPUUsage}
		}
		if err := enc.Encode(s); err != nil {
			return
		}
	}
}

// runBuild runs deno as UID 65534 without capabilities and sends the result back.
func runBuild(h fcproto.Header, setup *os.File, cg *os.File) error {
	cmd := exec.Command(self, append([]string{"__fcexec", "--uid", strconv.Itoa(nobody), "--caps", "none", "--", deno}, h.Args...)...)
	cmd.Env = append([]string{envPath}, h.Env...)
	cmd.Dir = "/tmp"
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitWriter{w: &stdout, n: maxBuild}
	cmd.Stderr = &limitWriter{w: &stderr, n: 1 << 20}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd()), Setsid: true}
	res := fcproto.Result{}
	if err := cmd.Start(); err != nil {
		res.Error = err.Error()
	} else {
		// Orphans of the build (esbuild) are not collected: the VM ends right after the result.
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			res.Exit = ee.ExitCode()
			if res.Exit < 0 {
				res.Exit = 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
			}
		default:
			res.Error = err.Error()
		}
	}
	if err := fcproto.WriteResult(setup, res, stdout.Bytes(), stderr.Bytes()); err != nil {
		return err
	}
	drain(setup, 30*time.Second)
	return nil
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errors.New("output too large")
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

// dial connects to the host (CID 2) on port; Firecracker forwards it to the host socket "<uds>_<port>".
func dial(port uint32) (*os.File, error) {
	var last error
	for i := 0; i < 50; i++ {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("vsock: %w", err)
		}
		// The default connect timeout of AF_VSOCK is 2 s; under load the host may need longer.
		_ = unix.SetsockoptTimeval(fd, unix.AF_VSOCK, 6 /* SO_VM_SOCKETS_CONNECT_TIMEOUT */, &unix.Timeval{Sec: 10})
		if err = unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: port}); err == nil {
			f := os.NewFile(uintptr(fd), "vsock:"+strconv.Itoa(int(port)))
			if _, err = f.Write([]byte(fcproto.Hello)); err == nil {
				return f, nil
			}
			f.Close()
			last = err
			continue
		}
		unix.Close(fd)
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("vsock connect to port %d: %w", port, last)
}

// ExecMain is "tsbox __fcexec [--uid N] [--caps setuid,setgid,kill|none] -- cmd…": sets RLIMIT_NOFILE and
// RLIMIT_CORE, keeps only the named capabilities in the bounding set, switches to uid (without
// supplementary groups), sets no_new_privs and replaces itself with cmd. As UID 0, cmd then has exactly the
// kept capabilities; with another UID it has none.
func ExecMain(args []string) int {
	uid := -1
	keep := map[int]bool{}
	for len(args) > 0 && args[0] != "--" {
		if len(args) < 2 {
			return usageExec()
		}
		switch args[0] {
		case "--uid":
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				return usageExec()
			}
			uid = n
		case "--caps":
			for _, c := range strings.Split(args[1], ",") {
				switch c {
				case "setuid":
					keep[unix.CAP_SETUID] = true
				case "setgid":
					keep[unix.CAP_SETGID] = true
				case "kill":
					keep[unix.CAP_KILL] = true
				case "none", "":
				default:
					return usageExec()
				}
			}
		default:
			return usageExec()
		}
		args = args[2:]
	}
	if len(args) < 2 {
		return usageExec()
	}
	cmd := args[1:]
	fail := func(what string, err error) int {
		fmt.Fprintf(os.Stderr, "tsbox __fcexec: %s: %v\n", what, err)
		return 126
	}
	_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 1024, Max: 1024})
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{})
	last := 63
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = n
		}
	}
	for c := 0; c <= last; c++ {
		if !keep[c] {
			if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
				return fail("capbset_drop", err)
			}
		}
	}
	if uid > 0 {
		if err := unix.Setgroups(nil); err != nil {
			return fail("setgroups", err)
		}
		if err := unix.Setresgid(uid, uid, uid); err != nil {
			return fail("setgid", err)
		}
		if err := unix.Setresuid(uid, uid, uid); err != nil {
			return fail("setuid", err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fail("no_new_privs", err)
	}
	err := unix.Exec(cmd[0], cmd, os.Environ())
	return fail("exec "+cmd[0], err)
}

func usageExec() int {
	fmt.Fprintln(os.Stderr, "usage: tsbox __fcexec [--uid N] [--caps setuid,setgid,kill|none] -- cmd...")
	return 2
}
