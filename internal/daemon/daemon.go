// Package daemon is tsboxd: an engine with persistent state and a local HTTP/JSON API over a Unix socket
// (on Windows a named pipe with a DACL for the current user only).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/build"
)

// Config controls the daemon.
type Config struct {
	StateDir    string
	Socket      string        // path of the socket
	System      bool          // system daemon: socket 0660 for the group Group, no idle exit
	Group       string        // group of the system socket (default "tsbox"; if it is missing: root only)
	Idle        time.Duration // exit after this much idle time (0 = never)
	Autostarted bool          // started by the CLI (may be replaced when the binary changes)
	Engine      []tsbox.Option
	Logger      *slog.Logger
}

const (
	eventRing = 1000
	lockWait  = 40 * time.Second
)

var boxName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// Daemon holds the engine and all known boxes.
type Daemon struct {
	cfg   Config
	log   *slog.Logger
	e     *tsbox.Engine
	build string

	mu   sync.Mutex
	recs map[string]*record

	evMu   sync.Mutex
	ring   []tsbox.Event
	evSubs map[chan tsbox.Event]struct{}

	active   atomic.Int64 // open connections
	busy     atomic.Int64 // running operations (build, start, stop)
	stopOnce sync.Once
	dumpOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

type record struct {
	name string
	op   sync.Mutex // serializes lifecycle operations

	mu         sync.Mutex
	spec       tsbox.BoxSpec // validated, with references instead of secret values
	created    time.Time
	desired    string
	autoRemove bool
	last       *tsbox.BoxStatus
	env        map[string]string // values of env: secrets, in memory only
	box        *tsbox.Box
	attached   bool
	removed    bool
}

// Run starts the daemon and returns when ctx ends, /v1/shutdown is called or the idle time expires.
// Running boxes are stopped; their desired state stays stored.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.StateDir == "" {
		cfg.StateDir = tsbox.DefaultStateDir()
	}
	if cfg.Socket == "" {
		p, _ := client.SocketPath(client.Hosts()[len(client.Hosts())-1])
		cfg.Socket = p
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	// An autostarted daemon waits until an ending predecessor (still stopping boxes) releases the
	// state.
	unlock, err := lockState(cfg.StateDir)
	for wait := time.Now().Add(lockWait); err != nil && cfg.Autostarted && time.Now().Before(wait) && ctx.Err() == nil; {
		time.Sleep(200 * time.Millisecond)
		unlock, err = lockState(cfg.StateDir)
	}
	if err != nil {
		return err
	}
	defer unlock()
	d := &Daemon{cfg: cfg, log: cfg.Logger, recs: map[string]*record{}, evSubs: map[chan tsbox.Event]struct{}{},
		stop: make(chan struct{}), build: BuildID()}
	opts := append([]tsbox.Option{tsbox.WithStateDir(cfg.StateDir), tsbox.WithEvents(d.onEvent), tsbox.WithLogger(cfg.Logger)}, cfg.Engine...)
	e, err := tsbox.New(ctx, opts...)
	if err != nil {
		return err
	}
	d.e = e
	ln, err := listen(cfg)
	if err != nil {
		_ = e.Close(context.Background())
		return err
	}
	defer os.Remove(cfg.Socket)

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		Handler:           d.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ConnState: func(_ net.Conn, s http.ConnState) {
			switch s {
			case http.StateNew:
				d.active.Add(1)
			case http.StateClosed, http.StateHijacked:
				d.active.Add(-1)
			}
		},
	}
	d.restore(baseCtx)
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	d.log.Info("tsboxd ready", "socket", cfg.Socket, "state", cfg.StateDir, "driver", e.DriverName(), "idle", cfg.Idle)
	for _, di := range e.Drivers() {
		d.log.Info("driver", "name", di.Name, "default", di.Default, "available", di.Available, "reason", di.Error, "note", di.Note)
	}

	idle := time.NewTicker(time.Second)
	defer idle.Stop()
	var idleSince time.Time
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-d.stop:
			break loop
		case err := <-srvErr:
			d.log.Error("API", "err", err)
			break loop
		case <-idle.C:
			if cfg.Idle <= 0 || !d.idle() {
				idleSince = time.Time{}
				continue
			}
			if idleSince.IsZero() {
				idleSince = time.Now()
			} else if time.Since(idleSince) >= cfg.Idle {
				d.log.Info("idle, tsboxd exits", "after", cfg.Idle)
				break loop
			}
		}
	}
	stopStart := time.Now()
	d.log.Info("tsboxd exiting", "boxes", d.running())
	defer func() { d.log.Info("tsboxd exited", "took", time.Since(stopStart).Round(time.Millisecond)) }()
	// Diagnostics: if exiting hangs (boxes, open streams), the goroutines end up in the log (tsboxd.log).
	dump := time.AfterFunc(30*time.Second, func() {
		d.log.Error("tsboxd has not exited for 30 s, goroutines follow")
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
	})
	defer dump.Stop()
	shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = srv.Shutdown(shCtx)
	cancel()
	cancelBase()
	_ = srv.Close()
	err = e.Close(context.Background())
	d.wg.Wait()
	d.mu.Lock()
	for _, r := range d.recs {
		r.mu.Lock()
		if r.box != nil && !r.removed {
			st := r.box.Status()
			r.last = &st
			d.save(r)
		}
		r.mu.Unlock()
	}
	d.mu.Unlock()
	return err
}

// Shutdown ends Run.
func (d *Daemon) Shutdown() { d.stopOnce.Do(func() { close(d.stop) }) }

func (d *Daemon) idle() bool {
	if d.active.Load() > 0 || d.busy.Load() > 0 {
		return false
	}
	return d.running() == 0
}

func (d *Daemon) running() int {
	n := 0
	for _, r := range d.records() {
		if v := r.view(d); !terminal(v.State) {
			n++
		}
	}
	return n
}

func terminal(s tsbox.BoxState) bool {
	switch s {
	case tsbox.BoxExited, tsbox.BoxFailed, tsbox.BoxQuarantined, tsbox.BoxCreated:
		return true
	}
	return false
}

func (d *Daemon) records() []*record {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*record, 0, len(d.recs))
	for _, r := range d.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (d *Daemon) get(name string) (*record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.recs[name]
	if !ok {
		return nil, &client.Error{Status: 404, Code: "not_found", Message: fmt.Sprintf("box %q not found", name)}
	}
	return r, nil
}

// driverOf is the driver a box uses: its own, or the default for boxes stored before drivers per box.
func (d *Daemon) driverOf(spec tsbox.BoxSpec) string {
	if spec.Driver != "" && spec.Driver != "auto" {
		return spec.Driver
	}
	return d.e.DriverName()
}

// view is the view for ps/inspect.
func (r *record) view(d *Daemon) client.Box {
	r.mu.Lock()
	defer r.mu.Unlock()
	var st tsbox.BoxStatus
	switch {
	case r.box != nil:
		st = r.box.Status()
	case r.last != nil:
		st = *r.last
	default:
		drv := d.driverOf(r.spec)
		st = tsbox.BoxStatus{Name: r.name, Driver: drv, Isolated: drv != "dev", State: tsbox.BoxCreated,
			Limits: r.spec.Limits, Labels: r.spec.Labels}
		for _, s := range r.spec.Scripts {
			st.Scripts = append(st.Scripts, tsbox.ScriptStatus{Name: s.Name, State: tsbox.ScriptPending})
		}
	}
	return client.Box{BoxStatus: st, Created: r.created, AutoRemove: r.autoRemove, Desired: r.desired, Loaded: r.box != nil}
}

func (d *Daemon) onEvent(ev tsbox.Event) {
	d.evMu.Lock()
	d.ring = append(d.ring, ev)
	if len(d.ring) > eventRing {
		d.ring = d.ring[len(d.ring)-eventRing:]
	}
	for ch := range d.evSubs {
		select {
		case ch <- ev:
		default:
		}
	}
	d.evMu.Unlock()
}

// subscribeEvents returns the events since since from the ring buffer and a channel for new ones (without
// gap and without duplicates).
func (d *Daemon) subscribeEvents(since time.Time) ([]tsbox.Event, chan tsbox.Event, func()) {
	ch := make(chan tsbox.Event, 256)
	d.evMu.Lock()
	var past []tsbox.Event
	if !since.IsZero() {
		for _, ev := range d.ring {
			if !ev.At.Before(since) {
				past = append(past, ev)
			}
		}
	}
	d.evSubs[ch] = struct{}{}
	d.evMu.Unlock()
	return past, ch, func() {
		d.evMu.Lock()
		delete(d.evSubs, ch)
		d.evMu.Unlock()
	}
}

func withEnv(spec tsbox.BoxSpec, env map[string]string) tsbox.BoxSpec {
	if env == nil {
		env = map[string]string{} // never fall back to the daemon's environment
	}
	spec.SDKs = append([]tsbox.SDKUse(nil), spec.SDKs...)
	for i := range spec.SDKs {
		spec.SDKs[i].Env = env
	}
	return spec
}

func invalid(err error) error {
	var ae *client.Error
	if errors.As(err, &ae) {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "environment variable") {
		msg += " (tsboxd keeps env: secrets in memory only; \"tsbox start\" passes them again from the caller's environment)"
	}
	return &client.Error{Status: 400, Code: "invalid", Message: msg}
}

// create creates a box and builds it. The name is reserved beforehand.
func (d *Daemon) create(ctx context.Context, req client.CreateRequest, attached bool) (*record, error) {
	name := req.Spec.Name
	if !boxName.MatchString(name) {
		return nil, &client.Error{Status: 400, Code: "invalid", Message: fmt.Sprintf("invalid box name %q (allowed: a-z, 0-9, -, 1-48 characters)", name)}
	}
	r := &record{name: name, spec: req.Spec, created: time.Now(), desired: "stopped", autoRemove: req.AutoRemove,
		env: req.Env, attached: attached}
	d.mu.Lock()
	if _, ok := d.recs[name]; ok {
		d.mu.Unlock()
		return nil, &client.Error{Status: 409, Code: "conflict", Message: fmt.Sprintf("box %q already exists (tsbox rm %s)", name, name)}
	}
	r.op.Lock()
	d.recs[name] = r
	d.mu.Unlock()
	defer r.op.Unlock()
	d.busy.Add(1)
	defer d.busy.Add(-1)
	b, err := d.e.Create(ctx, withEnv(req.Spec, req.Env))
	if err != nil {
		d.mu.Lock()
		delete(d.recs, name)
		d.mu.Unlock()
		_ = d.e.Remove(context.Background(), name, true)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var be *build.Error
		if errors.As(err, &be) { // as before with tsbox run: only the build output
			return nil, &client.Error{Status: 400, Code: "build", Message: be.Error()}
		}
		return nil, invalid(err)
	}
	r.mu.Lock()
	r.box, r.spec = b, b.Spec()
	d.save(r)
	r.mu.Unlock()
	d.onEvent(tsbox.Event{At: time.Now(), Kind: client.EventBoxCreated, Box: name})
	return r, nil
}

// start starts a box (builds it if it is not loaded). A running box stays unchanged.
func (d *Daemon) start(ctx context.Context, r *record, env map[string]string) error {
	r.op.Lock()
	defer r.op.Unlock()
	d.busy.Add(1)
	defer d.busy.Add(-1)
	r.mu.Lock()
	if r.removed {
		r.mu.Unlock()
		return &client.Error{Status: 404, Code: "not_found", Message: fmt.Sprintf("box %q not found", r.name)}
	}
	if len(env) > 0 {
		if r.env == nil {
			r.env = map[string]string{}
		}
		for k, v := range env {
			r.env[k] = v
		}
	}
	b, spec, envCopy := r.box, r.spec, r.env
	r.mu.Unlock()
	if b != nil && !terminal(b.Status().State) {
		return nil
	}
	if b == nil {
		nb, err := d.e.Create(ctx, withEnv(spec, envCopy))
		if err != nil {
			r.mu.Lock()
			st := tsbox.BoxStatus{Name: r.name, Driver: d.driverOf(spec), State: tsbox.BoxFailed, Error: err.Error(), Limits: spec.Limits, Labels: spec.Labels}
			r.last = &st
			d.save(r)
			r.mu.Unlock()
			return invalid(err)
		}
		b = nb
		r.mu.Lock()
		r.box = b
		r.mu.Unlock()
	}
	err := b.Start(ctx)
	r.mu.Lock()
	r.desired = "running"
	st := b.Status()
	r.last = &st
	d.save(r)
	r.mu.Unlock()
	if err != nil {
		return &client.Error{Status: 500, Code: "internal", Message: err.Error()}
	}
	d.wg.Add(1)
	go d.watch(r, b)
	return nil
}

// watch stores the final status and removes boxes with AutoRemove.
func (d *Daemon) watch(r *record, b *tsbox.Box) {
	defer d.wg.Done()
	st, _ := b.Wait(context.Background())
	r.mu.Lock()
	if r.box != b || r.removed {
		r.mu.Unlock()
		return
	}
	r.last = &st
	d.save(r)
	auto := r.autoRemove && !r.attached && terminal(st.State)
	r.mu.Unlock()
	if auto {
		if err := d.remove(context.Background(), r.name, true); err != nil {
			d.log.Warn("AutoRemove", "box", r.name, "err", err)
		}
	}
}

func (d *Daemon) stopBox(ctx context.Context, r *record, grace time.Duration) error {
	r.op.Lock()
	defer r.op.Unlock()
	d.busy.Add(1)
	defer d.busy.Add(-1)
	r.mu.Lock()
	r.desired = "stopped"
	d.save(r)
	b := r.box
	r.mu.Unlock()
	if b == nil {
		return nil
	}
	err := b.Stop(ctx, grace)
	r.mu.Lock()
	if !r.removed {
		st := b.Status()
		r.last = &st
		d.save(r)
	}
	r.mu.Unlock()
	return err
}

func (d *Daemon) remove(ctx context.Context, name string, force bool) error {
	r, err := d.get(name)
	if err != nil {
		return err
	}
	r.op.Lock()
	defer r.op.Unlock()
	r.mu.Lock()
	b := r.box
	r.mu.Unlock()
	if b != nil && !terminal(b.Status().State) && !force {
		return &client.Error{Status: 409, Code: "conflict", Message: fmt.Sprintf("box %q is running: stop it first or use -f", name)}
	}
	r.mu.Lock()
	r.removed = true // before, so that watch does not recreate the directory
	r.mu.Unlock()
	if err := d.e.Remove(ctx, name, force); err != nil {
		r.mu.Lock()
		r.removed = false
		r.mu.Unlock()
		return err
	}
	d.mu.Lock()
	if d.recs[name] == r {
		delete(d.recs, name)
	}
	d.mu.Unlock()
	d.onEvent(tsbox.Event{At: time.Now(), Kind: client.EventBoxRemoved, Box: name})
	return nil
}

// restore loads stored boxes and starts those that should run and have a restart policy.
func (d *Daemon) restore(ctx context.Context) {
	for _, r := range d.load() {
		d.mu.Lock()
		d.recs[r.name] = r
		d.mu.Unlock()
		if r.desired != "running" || !restartWanted(r.spec) {
			continue
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			if err := d.start(ctx, r, nil); err != nil {
				d.log.Warn("box not restored", "box", r.name, "err", err)
			} else {
				d.log.Info("box restored", "box", r.name)
			}
		}()
	}
}

func restartWanted(s tsbox.BoxSpec) bool {
	if s.Restart.Mode != "" && s.Restart.Mode != tsbox.RestartNo {
		return true
	}
	for _, sc := range s.Scripts {
		if sc.Restart != nil && sc.Restart.Mode != tsbox.RestartNo {
			return true
		}
	}
	return false
}

// BuildID identifies the running binary (size and modification time); the CLI replaces an autostarted
// daemon with a different identifier as long as it runs nothing.
func BuildID() string {
	exe, err := os.Executable()
	if err != nil {
		return tsbox.Version
	}
	st, err := os.Stat(exe)
	if err != nil {
		return tsbox.Version
	}
	return fmt.Sprintf("%s-%x-%x", tsbox.Version, st.Size(), st.ModTime().UnixNano())
}
