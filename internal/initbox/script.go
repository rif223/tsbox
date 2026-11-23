package initbox

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rif223/tsbox/internal/proto"
)

// scriptProc is a running script process.
type scriptProc struct {
	sid     string
	spec    proto.Start
	cmd     *exec.Cmd
	init    *initState
	started time.Time

	lastAlive atomic.Int64 // UnixNano of the last heartbeat; 0 = none yet
	queued    atomic.Int64 // bytes in queue
	queue     chan []byte
	pumps     sync.WaitGroup
	done      chan struct{}

	mu       sync.Mutex
	reason   string
	detail   string
	stats    proto.ProcStats
	closeOne sync.Once
}

// kill ends the process with a reason. The first reason wins.
func (p *scriptProc) kill(reason, detail string) {
	p.mu.Lock()
	if p.reason == "" {
		p.reason, p.detail = reason, detail
	}
	p.mu.Unlock()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// deliver queues a frame towards the script. If the queue overflows, the script no longer keeps up
// reading: it is ended instead of blocking init or the host.
func (p *scriptProc) deliver(f *proto.Frame) {
	b, err := proto.Encode(f)
	if err != nil {
		return
	}
	if p.queued.Add(int64(len(b))) > scriptQueueMax {
		p.queued.Add(-int64(len(b)))
		p.kill("backpressure", "script does not read its messages")
		return
	}
	select {
	case p.queue <- b:
	case <-p.done:
		p.queued.Add(-int64(len(b)))
	default:
		p.queued.Add(-int64(len(b)))
		p.kill("backpressure", "script does not read its messages")
	}
}

func (p *scriptProc) writeStdin(w io.WriteCloser) {
	defer w.Close()
	for {
		select {
		case <-p.done:
			return
		case b := <-p.queue:
			_, err := w.Write(b)
			p.queued.Add(-int64(len(b)))
			if err != nil {
				return
			}
		}
	}
}

func (p *scriptProc) pumpStdout(r io.Reader) {
	defer p.pumps.Done()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		f, err := proto.Read(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.kill("protocol", "script wrote invalid bytes to stdout (e.g. bypassing Deno.stdout)")
			}
			return
		}
		if !proto.FromScript[f.T] {
			p.kill("protocol", "forbidden frame type "+f.T)
			return
		}
		if f.T == proto.TAlive {
			// Only the first heartbeat of a start reaches the host: it tells that @tsbox/core is up
			// (starting -> running) also for scripts that never log or log through an SDK.
			if p.lastAlive.Swap(time.Now().UnixNano()) != 0 {
				continue
			}
		}
		// Identity comes from the pipe, never from the frame.
		f.SID, f.SIDs = p.sid, nil
		p.init.out.send(f)
	}
}

func (p *scriptProc) pumpStderr(r io.Reader) {
	defer p.pumps.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	sc.Split(scanLinesLimited)
	for sc.Scan() {
		if line := trimLine(sc.Text()); line != "" {
			p.init.out.send(&proto.Frame{T: proto.TLog, SID: p.sid, Level: "stderr", Msg: line})
		}
	}
	_, _ = io.Copy(io.Discard, r)
}

// scanLinesLimited splits at line ends, but after maxStderrLine bytes at the latest.
func scanLinesLimited(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		if c == '\n' {
			return i + 1, data[:i], nil
		}
		if i+1 >= maxStderrLine {
			return i + 1, data[:i+1], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// monitor enforces watchdog, run time, RSS and threads and collects measurements.
func (p *scriptProc) monitor() {
	t := time.NewTicker(monitorEvery)
	defer t.Stop()
	first := true
	for {
		select {
		case <-p.done:
			return
		case now := <-t.C:
			if p.spec.TimeoutMs > 0 && now.Sub(p.started) > time.Duration(p.spec.TimeoutMs)*time.Millisecond {
				p.kill("timeout", "run time limit reached")
				return
			}
			if wd := time.Duration(p.spec.WatchdogMs) * time.Millisecond; wd > 0 && len(p.spec.DebugExec) == 0 {
				last := p.lastAlive.Load()
				if last == 0 && now.Sub(p.started) > wd+startupGrace {
					p.kill("watchdog", "no heartbeat after the start")
					return
				}
				if last != 0 && now.Sub(time.Unix(0, last)) > wd {
					p.kill("watchdog", "event loop blocked")
					return
				}
			}
			pi, err := procInfo(p.cmd.Process.Pid)
			if err != nil {
				continue
			}
			rss := pi.private
			if !pi.hasPrivate {
				rss = max(pi.vmRSS-p.init.baseline, 0)
			}
			p.mu.Lock()
			p.stats = proto.ProcStats{SID: p.sid, RSS: rss, CPUNs: pi.cpu.Nanoseconds(), Threads: pi.threads}
			p.mu.Unlock()
			if first {
				// Send the first measurement to the host right away, not with the next pstats round (up to 5 s):
				// "tsbox stats" shows RSS and threads shortly after the start.
				first = false
				p.init.out.send(&proto.Frame{T: proto.TPStats, P: proto.Raw([]proto.ProcStats{p.lastStats()})})
			}
			if p.spec.RSSBytes > 0 && rss > p.spec.RSSBytes {
				p.kill("rss", "memory limit of the script exceeded")
				return
			}
			// With RLIMIT_NPROC (UID mode) the kernel limits; otherwise init measures.
			if p.spec.Threads > 0 && !p.init.uidMode && pi.threads > p.spec.Threads {
				p.kill("threads", "thread limit of the script exceeded")
				return
			}
		}
	}
}

func (p *scriptProc) lastStats() proto.ProcStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stats
	st.SID = p.sid
	return st
}

// wait waits for the process to end and returns the exit frame.
func (p *scriptProc) wait() proto.Exit {
	// Drain the pipes first, then Wait (exec.Cmd closes them too early otherwise).
	p.pumps.Wait()
	err := p.cmd.Wait()
	p.closeOne.Do(func() { close(p.done) })
	e := proto.Exit{Code: p.cmd.ProcessState.ExitCode()}
	if sig := signalName(p.cmd.ProcessState); sig != "" {
		e.Signal = sig
	}
	p.mu.Lock()
	e.Reason, e.Detail = p.reason, p.detail
	p.mu.Unlock()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) && e.Detail == "" {
		e.Detail = err.Error()
	}
	return e
}
