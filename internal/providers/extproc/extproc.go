// Package extproc runs external providers: one process per SDK (any language) that speaks the provider
// protocol over stdin/stdout (frames as in internal/proto, SID = session):
//
//	Engine → Provider: init{p:{proto,sdk,version,config}}, open{sid,p:{box,script,grants}}, call{sid,id,m,p},
//	                   close{sid}, ping{id}
//	Provider → Engine: init_ok, opened{sid[,err]}, ret{sid,id,p|err}, emit{sid,name,p}, log{level,msg}, pong{id}
//
// The provider is trusted operator code and runs on the host. The engine enforces the limits from the
// manifest before a call reaches the provider. If the process dies, open calls fail with "unavailable";
// the next Open restarts it (at the earliest after restartDelay).
package extproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rif223/tsbox/internal/proto"
	"github.com/rif223/tsbox/sdk"
)

// ProtoVersion is the version of the provider protocol (in the init frame).
const ProtoVersion = 1

const (
	handshakeTimeout = 15 * time.Second
	restartDelay     = 2 * time.Second
)

// Config of an external provider.
type Config struct {
	Name, Version string
	Dir           string            // SDK directory (working directory of the process)
	Exec          []string          // from the manifest; "deno" = the host's Deno, "./x" relative to Dir
	Deno          string            // the host's Deno
	Settings      map[string]string // resolved operator configuration (including secrets)
	Logger        *slog.Logger
}

// Provider starts the process on demand and multiplexes sessions over it.
type Provider struct {
	cfg Config

	mu        sync.Mutex
	proc      *proc
	lastStart time.Time
	nextSID   atomic.Uint64
}

func New(cfg Config) *Provider {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Provider{cfg: cfg}
}

type proc struct {
	cmd     *exec.Cmd
	wmu     sync.Mutex
	w       io.WriteCloser
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan *proto.Frame
	opens   map[string]chan *proto.Frame
	sess    map[string]sdk.Session
	done    chan struct{}
	ready   chan error
}

func (p *Provider) command() (*exec.Cmd, error) {
	args := append([]string(nil), p.cfg.Exec...)
	switch {
	case args[0] == "deno":
		args[0] = p.cfg.Deno
	case strings.HasPrefix(args[0], "./") || strings.HasPrefix(args[0], ".\\"):
		args[0] = filepath.Join(p.cfg.Dir, args[0])
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = p.cfg.Dir
	cmd.Env = []string{"NO_COLOR=1", "DENO_NO_UPDATE_CHECK=1"}
	for _, k := range []string{"PATH", "SystemRoot", "TEMP", "TMP", "HOME", "USERPROFILE", "LOCALAPPDATA", "LANG", "DENO_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return cmd, nil
}

// ensure returns a running, initialized process.
func (p *Provider) ensure(ctx context.Context) (*proc, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.proc != nil {
		select {
		case <-p.proc.done:
			p.proc = nil
		default:
			return p.proc, nil
		}
	}
	if wait := restartDelay - time.Since(p.lastStart); wait > 0 && !p.lastStart.IsZero() {
		return nil, sdk.Errorf(sdk.Unavailable, "provider %s is restarting", p.cfg.Name)
	}
	p.lastStart = time.Now()
	cmd, err := p.command()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &logWriter{l: p.cfg.Logger, name: p.cfg.Name}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("starting provider %s: program %s not found (a compiled provider must be built first, e.g. go build -C provider -o provider .)", p.cfg.Name, cmd.Args[0])
		}
		return nil, fmt.Errorf("starting provider %s: %w", p.cfg.Name, err)
	}
	pr := &proc{cmd: cmd, w: stdin, pending: map[uint64]chan *proto.Frame{}, opens: map[string]chan *proto.Frame{},
		sess: map[string]sdk.Session{}, done: make(chan struct{}), ready: make(chan error, 1)}
	go p.readLoop(pr, stdout)
	go func() {
		_ = cmd.Wait()
		pr.fail()
	}()
	settings := p.cfg.Settings
	if settings == nil {
		settings = map[string]string{} // always an object for the provider, never null
	}
	if err := pr.send(&proto.Frame{T: proto.TInit, P: proto.Raw(map[string]any{
		"proto": ProtoVersion, "sdk": p.cfg.Name, "version": p.cfg.Version, "config": settings,
	})}); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	select {
	case err = <-pr.ready:
	case <-time.After(handshakeTimeout):
		err = errors.New("no init_ok")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("provider %s: %w", p.cfg.Name, err)
	}
	p.proc = pr
	return pr, nil
}

func (pr *proc) send(f *proto.Frame) error {
	pr.wmu.Lock()
	defer pr.wmu.Unlock()
	return proto.Write(pr.w, f)
}

// fail ends all open calls with "unavailable".
func (pr *proc) fail() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	select {
	case <-pr.done:
		return
	default:
		close(pr.done)
	}
	for id, ch := range pr.pending {
		close(ch)
		delete(pr.pending, id)
	}
	for sid, ch := range pr.opens {
		close(ch)
		delete(pr.opens, sid)
	}
}

func (p *Provider) readLoop(pr *proc, r io.Reader) {
	defer func() {
		if pr.cmd != nil && pr.cmd.Process != nil {
			_ = pr.cmd.Process.Kill()
		}
		pr.fail()
	}()
	br := bufio.NewReaderSize(r, 64<<10)
	inited := false
	for {
		f, err := proto.Read(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.cfg.Logger.Warn("provider: protocol error", "sdk", p.cfg.Name, "err", err)
			}
			if !inited {
				pr.ready <- fmt.Errorf("process ended before init_ok: %v", err)
			}
			return
		}
		switch f.T {
		case proto.TInitOK:
			if !inited {
				inited = true
				pr.ready <- nil
			}
		case proto.TOpened:
			pr.mu.Lock()
			ch := pr.opens[f.SID]
			delete(pr.opens, f.SID)
			pr.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case proto.TRet:
			pr.mu.Lock()
			ch := pr.pending[f.ID]
			delete(pr.pending, f.ID)
			pr.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case proto.TEmit:
			pr.mu.Lock()
			s := pr.sess[f.SID]
			pr.mu.Unlock()
			if s != nil {
				_ = s.Emit(f.Name, f.P)
			}
		case proto.TLog:
			p.cfg.Logger.Info("provider", "sdk", p.cfg.Name, "level", f.Level, "msg", f.Msg)
		case proto.TPong:
		default:
			p.cfg.Logger.Warn("provider: unexpected frame, terminating", "sdk", p.cfg.Name, "t", f.T)
			return
		}
	}
}

// Open opens a session in the provider process.
func (p *Provider) Open(ctx context.Context, s sdk.Session) (sdk.Handler, error) {
	pr, err := p.ensure(ctx)
	if err != nil {
		return nil, err
	}
	sid := "s" + strconv.FormatUint(p.nextSID.Add(1), 10)
	ch := make(chan *proto.Frame, 1)
	pr.mu.Lock()
	pr.opens[sid] = ch
	pr.sess[sid] = s
	pr.mu.Unlock()
	grants := s.Grants()
	if grants == nil {
		grants = sdk.Grants{} // always an object for the provider, never null
	}
	open := map[string]any{"box": s.Box(), "script": s.Script(), "grants": grants}
	if err := pr.send(&proto.Frame{T: proto.TOpen, SID: sid, P: proto.Raw(open)}); err != nil {
		return nil, sdk.Errorf(sdk.Unavailable, "provider not reachable")
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return nil, sdk.Errorf(sdk.Unavailable, "provider ended")
		}
		if f.Err != nil {
			pr.mu.Lock()
			delete(pr.sess, sid)
			pr.mu.Unlock()
			return nil, &sdk.Error{Code: f.Err.Code, Msg: f.Err.Msg}
		}
	case <-time.After(handshakeTimeout):
		return nil, sdk.Errorf(sdk.Unavailable, "provider does not answer open")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &handler{pr: pr, sid: sid}, nil
}

type handler struct {
	pr  *proc
	sid string
}

func (h *handler) Call(ctx context.Context, method string, params json.RawMessage) (any, error) {
	id := h.pr.nextID.Add(1)
	ch := make(chan *proto.Frame, 1)
	h.pr.mu.Lock()
	select {
	case <-h.pr.done:
		h.pr.mu.Unlock()
		return nil, sdk.Errorf(sdk.Unavailable, "provider ended")
	default:
	}
	h.pr.pending[id] = ch
	h.pr.mu.Unlock()
	if err := h.pr.send(&proto.Frame{T: proto.TCall, SID: h.sid, ID: id, M: method, P: params}); err != nil {
		return nil, sdk.Errorf(sdk.Unavailable, "provider not reachable")
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return nil, sdk.Errorf(sdk.Unavailable, "provider ended")
		}
		if f.Err != nil {
			return nil, &sdk.Error{Code: f.Err.Code, Msg: f.Err.Msg}
		}
		if len(f.P) == 0 {
			return json.RawMessage("null"), nil
		}
		return f.P, nil
	case <-ctx.Done():
		h.pr.mu.Lock()
		delete(h.pr.pending, id)
		h.pr.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (h *handler) Close() error {
	h.pr.mu.Lock()
	delete(h.pr.sess, h.sid)
	h.pr.mu.Unlock()
	select {
	case <-h.pr.done:
		return nil
	default:
	}
	return h.pr.send(&proto.Frame{T: proto.TClose, SID: h.sid})
}

// Close ends the provider process.
func (p *Provider) Close() error {
	p.mu.Lock()
	pr := p.proc
	p.proc = nil
	p.mu.Unlock()
	if pr == nil {
		return nil
	}
	_ = pr.w.Close()
	select {
	case <-pr.done:
	case <-time.After(3 * time.Second):
		_ = pr.cmd.Process.Kill()
	}
	return nil
}

type logWriter struct {
	l    *slog.Logger
	name string
}

func (w *logWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			w.l.Warn("provider stderr", "sdk", w.name, "msg", line)
		}
	}
	return len(b), nil
}
