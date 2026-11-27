package tsbox

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/internal/logfile"
)

// LogOptions selects log lines for Engine.Logs and Box.Logs.
type LogOptions struct {
	Since  time.Time // only lines from this time on
	Script string    // only this script ("" = all)
	Tail   int       // only the last n stored lines (0 = all)
	// Follow returns new lines after the stored ones until the box ends (exited, failed, quarantined or
	// removed) or ctx expires.
	Follow bool
}

// logSubQueue limits new lines per reader with Follow; what does not fit is missing for that reader.
const logSubQueue = 1024

type logSub struct{ ch chan LogLine }

type sinkItem struct {
	box   string
	line  logfile.Line
	flush chan struct{}
}

// nextSeq returns a strictly monotonic sequence number, also across restarts of the engine (nanoseconds).
func (e *Engine) nextSeq() int64 {
	for {
		old := e.logSeq.Load()
		n := max(old+1, time.Now().UnixNano())
		if e.logSeq.CompareAndSwap(old, n) {
			return n
		}
	}
}

func (e *Engine) logDir(box string) string {
	return filepath.Join(e.cfg.stateDir, "boxes", box, "logs")
}

// sinkLoop writes log lines to the files (one goroutine per engine, so that the engine never waits for the
// disk).
func (e *Engine) sinkLoop() {
	defer e.wg.Done()
	for it := range e.sink {
		if it.flush != nil {
			close(it.flush)
			continue
		}
		if err := e.writer(it.box).Write(it.line); err != nil {
			e.cfg.logger.Warn("log file", "box", it.box, "err", err)
		}
	}
}

func (e *Engine) writer(box string) *logfile.Writer {
	e.logwMu.Lock()
	defer e.logwMu.Unlock()
	w, ok := e.logw[box]
	if !ok {
		w = logfile.NewWriter(e.logDir(box), e.cfg.logMax)
		e.logw[box] = w
	}
	return w
}

func (e *Engine) closeWriter(box string) {
	e.logwMu.Lock()
	w, ok := e.logw[box]
	delete(e.logw, box)
	e.logwMu.Unlock()
	if ok {
		_ = w.Close()
	}
}

// toFile queues a line for the file (discards and counts when the queue is full).
func (e *Engine) toFile(l LogLine) {
	e.chMu.RLock()
	defer e.chMu.RUnlock()
	if e.sink == nil || e.closed {
		return
	}
	select {
	case e.sink <- sinkItem{box: l.Box, line: logfile.Line{Seq: l.Seq, At: l.At, Script: l.Script, Level: l.Level, Msg: l.Msg}}:
	default:
		e.dropped.Add(1)
	}
}

// flushLogs waits until all lines queued so far are written.
func (e *Engine) flushLogs(ctx context.Context) {
	e.chMu.RLock()
	if e.sink == nil || e.closed {
		e.chMu.RUnlock()
		return
	}
	done := make(chan struct{})
	select {
	case e.sink <- sinkItem{flush: done}:
	case <-ctx.Done():
		e.chMu.RUnlock()
		return
	}
	e.chMu.RUnlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (b *Box) subscribe() *logSub {
	s := &logSub{ch: make(chan LogLine, logSubQueue)}
	b.subMu.Lock()
	if b.subs == nil {
		b.subs = map[*logSub]struct{}{}
	}
	b.subs[s] = struct{}{}
	b.subMu.Unlock()
	return s
}

func (b *Box) unsubscribe(s *logSub) {
	b.subMu.Lock()
	delete(b.subs, s)
	b.subMu.Unlock()
}

func (b *Box) publish(l LogLine) {
	b.subMu.Lock()
	for s := range b.subs {
		select {
		case s.ch <- l:
		default:
		}
	}
	b.subMu.Unlock()
}

// ended reports whether a reader with Follow should stop.
func (b *Box) ended() bool {
	if cur, ok := b.e.Box(b.spec.Name); !ok || cur != b {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BoxExited, BoxFailed, BoxQuarantined:
		return true
	}
	return false
}

// Logs returns the log lines of this box from the log files (per script <state>/boxes/<box>/logs, rotated at
// 10 MiB, at most two files) and with Follow new lines afterwards.
func (b *Box) Logs(ctx context.Context, o LogOptions) iter.Seq2[LogLine, error] {
	return b.e.logsOf(ctx, b.spec.Name, b, o)
}

// Logs returns the log lines of a box by name, even if the box is not (or no longer) loaded in this
// engine. Follow only works for loaded boxes.
func (e *Engine) Logs(ctx context.Context, box string, o LogOptions) iter.Seq2[LogLine, error] {
	b, _ := e.Box(box)
	return e.logsOf(ctx, box, b, o)
}

func (e *Engine) logsOf(ctx context.Context, name string, b *Box, o LogOptions) iter.Seq2[LogLine, error] {
	return func(yield func(LogLine, error) bool) {
		if !boxName.MatchString(name) {
			yield(LogLine{}, fmt.Errorf("invalid box name %q", name))
			return
		}
		var sub *logSub
		if o.Follow && b != nil {
			sub = b.subscribe()
			defer b.unsubscribe(sub)
		}
		e.flushLogs(ctx)
		stopped := false
		last, err := logfile.Read(e.logDir(name), logfile.Filter{Since: o.Since, Script: o.Script, Tail: o.Tail},
			func(l logfile.Line) bool {
				if !yield(LogLine{Seq: l.Seq, At: l.At, Box: name, Script: l.Script, Level: l.Level, Msg: l.Msg}, nil) {
					stopped = true
					return false
				}
				return true
			})
		if stopped {
			return
		}
		if err != nil {
			yield(LogLine{}, err)
			return
		}
		if sub == nil {
			return
		}
		emit := func(l LogLine) bool {
			if l.Seq <= last || (o.Script != "" && l.Script != o.Script) {
				return true
			}
			last = l.Seq
			return yield(l, nil)
		}
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case l := <-sub.ch:
				if !emit(l) {
					return
				}
			case <-t.C:
				if !b.ended() {
					continue
				}
				for {
					select {
					case l := <-sub.ch:
						if !emit(l) {
							return
						}
					default:
						return
					}
				}
			}
		}
	}
}

// Remove removes a box including its directory (artifacts, log files). A running box is only stopped with
// force. The kv storage area stays (like a named volume).
func (e *Engine) Remove(ctx context.Context, name string, force bool) error {
	if !boxName.MatchString(name) {
		return fmt.Errorf("invalid box name %q", name)
	}
	b, known := e.Box(name)
	if known {
		if !b.terminal() {
			if !force {
				return fmt.Errorf("box %q is running: stop it first or use -f", name)
			}
			if err := b.Stop(ctx, time.Second); err != nil {
				return err
			}
		}
	}
	e.flushLogs(ctx)
	e.closeWriter(name)
	// Delete first, forget afterwards: if the directory cannot be removed, the box stays known (terminal) and
	// a second Remove can try again, instead of an unknown box whose directory is left behind.
	if err := fsutil.RemoveAllRetry(filepath.Join(e.cfg.stateDir, "boxes", name)); err != nil {
		return fmt.Errorf("remove box %q: %w%s", name, err, removeHint(err))
	}
	if known {
		e.forget(name, b)
	}
	e.calls.forget(name)
	return nil
}

// removeHint explains the error that file system virtualization of packaged Windows apps causes
// (fsutil.RemoveAll); its fallback normally hides it.
func removeHint(err error) string {
	if runtime.GOOS == "windows" && errors.Is(err, syscall.ELOOP) {
		return " (the state directory seems to be virtualized by a packaged app; run tsbox doctor, or set TSBOX_STATE to a directory outside %LOCALAPPDATA%)"
	}
	return ""
}

// Spec returns the validated specification of the box (with defaults, without env values).
func (b *Box) Spec() BoxSpec {
	s := b.spec
	s.SDKs = append([]SDKUse(nil), b.spec.SDKs...)
	for i := range s.SDKs {
		s.SDKs[i].Env = nil
	}
	return s
}

// Name is the name of the box.
func (b *Box) Name() string { return b.spec.Name }
