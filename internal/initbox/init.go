// Package initbox is tsbox-init: the first process of every box (PID 1 under gVisor). It talks to the
// host over stdin/stdout and starts one Deno process with minimal rights per script. Every frame of a
// script gets the script ID of its pipe – a script cannot pose as another one.
//
// Per script, init enforces: integrity (SHA-256), V8 heap, RSS, threads (RLIMIT_NPROC in UID mode,
// otherwise measured), run time, watchdog (heartbeat "alive"), protocol and backpressure.
//
// Usage: tsbox __init --deno <path> [--uid-mode]. In UID mode (gVisor) init runs as UID 0 of the sandbox
// with only SETUID/SETGID/KILL and starts every script via "tsbox __exec" with its own UID.
package initbox

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/internal/proto"
)

// Version of tsbox-init (in hello).
const Version = "0.1.0"

const (
	monitorEvery   = 500 * time.Millisecond
	pstatsEvery    = 5 * time.Second
	startupGrace   = 10 * time.Second // the first heartbeat may come this much later
	maxStderrLine  = 4096
	scriptQueueLen = 256     // frames towards the script's stdin
	scriptQueueMax = 4 << 20 // bytes towards the script's stdin
)

// SIDPattern are valid script names (they end up in argv and logs).
var SIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

type initState struct {
	deno     string
	uidMode  bool
	self     string
	baseline int64

	out   *hostWriter
	mu    sync.Mutex
	procs map[string]*scriptProc
	wg    sync.WaitGroup
}

// Main runs init and returns the exit code.
func Main(args []string) int {
	fs := flag.NewFlagSet("__init", flag.ContinueOnError)
	deno := fs.String("deno", "/usr/local/bin/deno", "Deno in the box")
	uidMode := fs.Bool("uid-mode", false, "start every script with its own UID (needs SETUID/SETGID/KILL)")
	baseline := fs.Int64("baseline-rss", 0, "VmRSS of an empty Deno measured by an earlier start (0 = measure now)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsbox-init:", err)
		return 1
	}
	s := &initState{deno: *deno, uidMode: *uidMode, self: self, out: &hostWriter{w: os.Stdout}, procs: map[string]*scriptProc{}}
	if s.uidMode && !hasRssAnon() {
		// Measuring costs about a second before hello; the host passes the value of an earlier start of the same
		// rootfs and init (Engine.baselineRSS).
		if s.baseline = *baseline; s.baseline <= 0 {
			s.baseline = s.calibrate()
		}
	}
	s.out.send(&proto.Frame{T: proto.THello, P: proto.Raw(proto.Hello{
		Proto: proto.Version, Init: Version, UIDMode: s.uidMode, BaselineRSS: s.baseline,
	})})
	stopStats := make(chan struct{})
	go s.pstatsLoop(stopStats)
	err = s.readHost(os.Stdin)
	close(stopStats)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "tsbox-init: protocol error from the host:", err)
	}
	s.shutdown()
	if err != nil && !errors.Is(err, io.EOF) {
		return 1
	}
	return 0
}

// hostWriter serializes frames towards the host. Writing blocks: a script that sends faster than the host
// reads only slows down its own pipe (backpressure).
type hostWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (h *hostWriter) send(f *proto.Frame) {
	b, err := proto.Encode(f)
	if err != nil {
		return // too large: script frames are already checked, init's own frames are small
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.w.Write(b); err != nil {
		os.Exit(0) // host gone: the box ends
	}
}

func (s *initState) readHost(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		f, err := proto.Read(br)
		if err != nil {
			return err
		}
		switch f.T {
		case proto.TStart:
			var st proto.Start
			if err := proto.Payload(f, &st); err != nil {
				s.exit(f.SID, proto.Exit{Code: -1, Reason: "spawn_failed", Detail: err.Error()})
				continue
			}
			s.start(f.SID, st)
		case proto.TStop:
			if p := s.get(f.SID); p != nil {
				p.kill("stopped", "")
			}
		case proto.TRet:
			if p := s.get(f.SID); p != nil {
				p.deliver(&proto.Frame{T: proto.TRet, ID: f.ID, P: f.P, Err: f.Err})
			}
		case proto.TEvent:
			for _, sid := range f.SIDs {
				if p := s.get(sid); p != nil {
					p.deliver(&proto.Frame{T: proto.TEvent, SDK: f.SDK, Name: f.Name, P: f.P})
				}
			}
		case proto.TPing:
			s.out.send(&proto.Frame{T: proto.TPong, ID: f.ID})
		default:
			return fmt.Errorf("unexpected frame %q", f.T)
		}
	}
}

func (s *initState) get(sid string) *scriptProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[sid]
}

func (s *initState) exit(sid string, e proto.Exit) {
	s.out.send(&proto.Frame{T: proto.TExit, SID: sid, P: proto.Raw(e)})
}

func (s *initState) start(sid string, st proto.Start) {
	if !SIDPattern.MatchString(sid) {
		s.exit(sid, proto.Exit{Code: -1, Reason: "spawn_failed", Detail: "invalid script name"})
		return
	}
	s.mu.Lock()
	if _, running := s.procs[sid]; running {
		s.mu.Unlock()
		return
	}
	s.procs[sid] = nil // reserve
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		delete(s.procs, sid)
		s.mu.Unlock()
	}
	if len(st.DebugExec) == 0 {
		if sum, err := fsutil.FileSHA256(st.Artifact); err != nil || sum != st.SHA256 || len(st.SHA256) != 64 {
			release()
			s.exit(sid, proto.Exit{Code: -1, Reason: "integrity", Detail: "artifact does not match the build hash"})
			return
		}
	}
	p, err := s.spawn(sid, st)
	if err != nil {
		release()
		s.exit(sid, proto.Exit{Code: -1, Reason: "spawn_failed", Detail: err.Error()})
		return
	}
	s.mu.Lock()
	s.procs[sid] = p
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		e := p.wait()
		release()
		s.exit(sid, e)
	}()
}

func (s *initState) command(sid string, st proto.Start) *exec.Cmd {
	args := st.DebugExec
	if len(args) == 0 {
		args = []string{s.deno, "run", "--no-prompt", "--no-config", "--no-lock", "--no-remote", "--no-npm",
			"--allow-read=" + st.Artifact, fmt.Sprintf("--v8-flags=--max-old-space-size=%d", max(st.HeapMiB, 16)),
			st.Artifact, sid}
		if len(st.Params) > 0 {
			args = append(args, string(st.Params))
		}
	}
	if s.uidMode {
		nproc := 0
		if st.Threads > 0 {
			nproc = st.Threads
		}
		args = append([]string{s.self, "__exec", strconv.Itoa(st.UID), strconv.Itoa(nproc), "--"}, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = scriptEnv(s.uidMode)
	return cmd
}

func (s *initState) spawn(sid string, st proto.Start) (*scriptProc, error) {
	if s.uidMode && st.UID < 1000 {
		return nil, fmt.Errorf("UID %d not allowed (>= 1000 required)", st.UID)
	}
	cmd := s.command(sid, st)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &scriptProc{
		sid: sid, spec: st, cmd: cmd, init: s, started: time.Now(),
		queue: make(chan []byte, scriptQueueLen), done: make(chan struct{}),
	}
	p.lastAlive.Store(0)
	go p.writeStdin(stdin)
	p.pumps.Add(2)
	go p.pumpStdout(stdout)
	go p.pumpStderr(stderr)
	go p.monitor()
	return p, nil
}

func (s *initState) pstatsLoop(stop <-chan struct{}) {
	t := time.NewTicker(pstatsEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.mu.Lock()
			var list []proto.ProcStats
			for _, p := range s.procs {
				if p != nil {
					list = append(list, p.lastStats())
				}
			}
			s.mu.Unlock()
			if len(list) > 0 {
				s.out.send(&proto.Frame{T: proto.TPStats, P: proto.Raw(list)})
			}
		}
	}
}

func (s *initState) shutdown() {
	s.mu.Lock()
	for _, p := range s.procs {
		if p != nil {
			p.kill("init_shutdown", "")
		}
	}
	s.mu.Unlock()
	waitTimeout(&s.wg, 5*time.Second)
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
	case <-time.After(d):
	}
}

// calibrate measures the VmRSS of an empty Deno process. Under gVisor VmRSS includes the mapped binary
// (≈ 100 MB) and RssAnon is missing; the per-script RSS limit therefore applies to VmRSS − baseline (spike, finding 1).
func (s *initState) calibrate() int64 {
	cmd := exec.Command(s.self, "__exec", "999", "0", "--", s.deno, "eval", "--no-config",
		"await new Promise((r) => setTimeout(r, 1500))")
	cmd.Env = scriptEnv(s.uidMode)
	if err := cmd.Start(); err != nil {
		return 0
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var best int64
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		if pi, err := procInfo(cmd.Process.Pid); err == nil {
			best = max(best, pi.vmRSS)
		}
	}
	return best
}

func scriptEnv(uidMode bool) []string {
	env := []string{"NO_COLOR=1", "DENO_NO_UPDATE_CHECK=1"}
	// Without DENO_DIR (and without HOME) Deno aborts. In the box the path cannot be created (ro rootfs) –
	// that is enough for Deno, a script needs no cache (spike, finding 4).
	if uidMode {
		env = append(env, "DENO_DIR=/.tsbox/no-cache")
	}
	// Windows: without SystemRoot parts of the runtime fail (random numbers, sockets). Not a secret.
	if v := os.Getenv("SystemRoot"); v != "" {
		env = append(env, "SystemRoot="+v)
	}
	return env
}

// trimLine limits stderr lines.
func trimLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if len(s) > maxStderrLine {
		s = s[:maxStderrLine]
	}
	return s
}
