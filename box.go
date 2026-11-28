package tsbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/build"
	"github.com/rif223/tsbox/internal/driver"
	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/internal/initbox"
	"github.com/rif223/tsbox/internal/profile"
	"github.com/rif223/tsbox/internal/proto"
)

const (
	helloTimeout   = 30 * time.Second
	pingEvery      = 5 * time.Second
	pongTimeout    = 20 * time.Second
	hostPauseGap   = 3 * pingEvery // the watchdog ticks every pingEvery; a longer gap means the host was paused
	stopGrace      = 5 * time.Second
	maxViolations  = 20
	maxLogMsg      = 4096
	logRate        = 20.0 // per second and script
	logBurst       = 100.0
	writeQueue     = 1024 // frames towards init
	firstScriptUID = 10001
)

var boxName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// Box is a box with its scripts.
type Box struct {
	e      *Engine
	driver driver.Driver // runs this box (spec.Driver)
	spec   BoxSpec

	mu        sync.Mutex
	state     BoxState
	errMsg    string
	started   time.Time
	drv       driver.Box
	out       chan []byte
	scripts   map[string]*script
	order     []string
	stopping  bool
	gen       int // box generation (for timers from earlier runs)
	lastPong  time.Time
	pingID    uint64
	pingSent  map[uint64]bool
	viol      int
	oomSeen   int
	lastStats driver.Stats
	hung      bool
	cpuCores  float64
	finished  chan struct{} // closed when the box has finally ended

	sdks          map[string]*activeSDK
	imports       map[string]string // import map of the SDK modules for the build
	droppedEvents uint64
	checkOnly     bool // build only (Engine.Check), do not create a box directory
	profileHash   string
	startCancel   context.CancelFunc // ends a running Start (Stop while the box is starting)

	subMu sync.Mutex
	subs  map[*logSub]struct{} // readers with Follow
}

type script struct {
	spec     ScriptSpec
	policy   RestartPolicy
	uid      int
	artifact build.Artifact
	state    ScriptState
	started  time.Time
	restarts int
	failures []time.Time
	lastExit *ExitStatus
	tokens   float64
	tokensAt time.Time
	stats    proto.ProcStats
	lastCPU  proto.ProcStats
	lastAt   time.Time
	cpu      float64
	heap     int64
	heapOOM  bool       // V8 reported "heap out of memory" (stderr) in this run
	run      *scriptRun // SDK state of the current run
	runSeq   int
}

// Validate checks the specification as the engine does before creating the box (names, limits; zero
// values mean the defaults). SDKs and entry files are not checked.
func (s BoxSpec) Validate() error {
	s.Scripts = append([]ScriptSpec(nil), s.Scripts...) // normalize fills in defaults; the caller's spec stays
	_, err := normalize(s)
	return err
}

func normalize(s BoxSpec) (BoxSpec, error) {
	if !boxName.MatchString(s.Name) {
		return s, fmt.Errorf("invalid box name %q (allowed: a-z, 0-9, -, 1-48 characters)", s.Name)
	}
	if len(s.Scripts) == 0 {
		return s, errors.New("box without scripts")
	}
	n := int64(len(s.Scripts))
	if s.Limits.Memory == 0 {
		s.Limits.Memory = max(128<<20, (48+64*n)<<20)
	}
	if s.Limits.CPU == 0 {
		s.Limits.CPU = 0.5
	}
	if s.Limits.PIDs == 0 {
		s.Limits.PIDs = max(64, int(32+24*n))
	}
	if err := (driver.Limits{MemoryBytes: s.Limits.Memory, CPUCores: s.Limits.CPU, PIDs: s.Limits.PIDs}).Validate(); err != nil {
		return s, err
	}
	if c := s.Limits.StartCPU; c != 0 && (c < s.Limits.CPU || c > 64) {
		return s, fmt.Errorf("start CPU must be 0 (no boost) or between the box CPU (%g) and 64 (got %g)", s.Limits.CPU, c)
	}
	s.Restart = defaultPolicy(s.Restart)
	if s.Storage == "" {
		s.Storage = s.Name
	}
	if !storagePattern.MatchString(s.Storage) {
		return s, fmt.Errorf("invalid storage area %q (allowed: a-z, 0-9, -, 1-48 characters)", s.Storage)
	}
	if err := profile.ValidName(s.Profile); err != nil {
		return s, err
	}
	switch s.Driver {
	case "gvisor", "firecracker", "dev":
	case "":
		return s, ErrNoDriver
	default:
		return s, fmt.Errorf("unknown driver %q (gvisor, firecracker, dev)", s.Driver)
	}
	seen := map[string]bool{}
	for i := range s.Scripts {
		sc := &s.Scripts[i]
		if !initbox.SIDPattern.MatchString(sc.Name) {
			return s, fmt.Errorf("invalid script name %q", sc.Name)
		}
		if seen[sc.Name] {
			return s, fmt.Errorf("script name %q used twice", sc.Name)
		}
		seen[sc.Name] = true
		if sc.Entry == "" && len(sc.debugExec) == 0 {
			return s, fmt.Errorf("script %q: entry missing", sc.Name)
		}
		if len(sc.Params) > 0 {
			if !json.Valid(sc.Params) {
				return s, fmt.Errorf("script %q: params are not valid JSON", sc.Name)
			}
			if limit := maxParams(s.Driver); len(sc.Params) > limit {
				return s, fmt.Errorf("script %q: params are %d bytes, at most %s are allowed (they reach the script on its command line)",
					sc.Name, len(sc.Params), driver.FormatBytes(int64(limit)))
			}
		}
		l := &sc.Limits
		if l.Heap == 0 {
			l.Heap = 64 << 20
		}
		if l.Heap < 16<<20 {
			return s, fmt.Errorf("script %q: heap %s is below the minimum of 16Mi", sc.Name, driver.FormatBytes(l.Heap))
		}
		if l.Threads == 0 {
			l.Threads = 32
		}
		if l.Threads < 12 {
			return s, fmt.Errorf("script %q: threads %d < 12 (Deno needs about 7)", sc.Name, l.Threads)
		}
		if l.Watchdog == 0 {
			l.Watchdog = 10 * time.Second
		}
		if l.Timeout < 0 {
			return s, fmt.Errorf("script %q: negative run time limit %s", sc.Name, l.Timeout)
		}
		if sc.Restart != nil {
			p := defaultPolicy(*sc.Restart)
			sc.Restart = &p
		}
	}
	return s, nil
}

// MaxParams is the largest ScriptSpec.Params (JSON). Params reach the script as an argument of its command
// line: Linux allows 128 KiB per argument. Windows allows 32767 characters for the whole command line, and
// quoting can make a JSON string up to four times as long, so the dev driver on Windows allows
// MaxParamsWindows. Before, larger params failed with spawn_failed, and above 1 MiB the script waited in
// "starting" for ever.
const (
	MaxParams        = 64 << 10
	MaxParamsWindows = 8 << 10
)

func maxParams(drv string) int {
	if drv == "dev" && runtime.GOOS == "windows" {
		return MaxParamsWindows
	}
	return MaxParams
}

func defaultPolicy(p RestartPolicy) RestartPolicy {
	if p.Mode == "" {
		p.Mode = RestartNo
	}
	if p.MaxFailures == 0 {
		p.MaxFailures = 5
	}
	if p.Window == 0 {
		p.Window = 10 * time.Minute
	}
	if p.BackoffMin == 0 {
		p.BackoffMin = time.Second
	}
	if p.BackoffMax == 0 {
		p.BackoffMax = 60 * time.Second
	}
	return p
}

func newBox(e *Engine, spec BoxSpec, drv driver.Driver) *Box {
	b := &Box{e: e, driver: drv, spec: spec, state: BoxCreated, scripts: map[string]*script{}, finished: make(chan struct{})}
	for i, sc := range spec.Scripts {
		p := spec.Restart
		if sc.Restart != nil {
			p = *sc.Restart
		}
		b.scripts[sc.Name] = &script{spec: sc, policy: p, uid: firstScriptUID + i, state: ScriptPending}
		b.order = append(b.order, sc.Name)
	}
	return b
}

func (b *Box) dir() string { return filepath.Join(b.e.cfg.stateDir, "boxes", b.spec.Name) }

// build builds all scripts and puts a copy of every artifact into the box's artifact directory (only this
// box sees it).
func (b *Box) build(ctx context.Context) error {
	adir := filepath.Join(b.dir(), "artifacts")
	if !b.checkOnly {
		if err := fsutil.RemoveAll(adir); err != nil {
			// artifacts are content-addressed and checked by init, so stale files are only wasted space
			b.e.cfg.logger.Warn("removing old artifacts", "box", b.spec.Name, "err", err)
		}
		if err := os.MkdirAll(adir, 0o755); err != nil {
			return err
		}
	}
	// Scripts are built in parallel (deno's type check is the slow, single-threaded part); errors are
	// reported in script order.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, len(b.order))
	sem := make(chan struct{}, buildParallel())
	var wg sync.WaitGroup
	for i, name := range b.order {
		s := b.scripts[name]
		if len(s.spec.debugExec) > 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			if errs[i] = b.buildScript(ctx, s, adir); errs[i] != nil {
				errs[i] = fmt.Errorf("script %q: %w", name, errs[i])
				cancel()
			}
		}()
	}
	wg.Wait()
	var first error
	for _, err := range errs {
		if err != nil && (first == nil || errors.Is(first, context.Canceled)) {
			first = err // prefer the cause over the cancellations it triggered
		}
	}
	return first
}

// buildParallel limits parallel builds of one box; Engine.buildSlots limits real builds of all boxes.
func buildParallel() int { return max(1, min(4, runtime.GOMAXPROCS(0))) }

// buildScript builds one script and copies the artifact into adir. It writes only s.artifact.
func (b *Box) buildScript(ctx context.Context, s *script, adir string) error {
	a, err := b.e.bundle(ctx, b.driver, s.spec.Entry, b.imports)
	if err != nil {
		return err
	}
	if b.checkOnly {
		s.artifact = a
		return nil
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return err
	}
	dst := filepath.Join(adir, a.SHA256+".js")
	if err := os.WriteFile(dst, data, 0o444); err != nil && !os.IsExist(err) {
		if _, serr := os.Stat(dst); serr != nil {
			return err
		}
	}
	s.artifact = build.Artifact{Path: dst, SHA256: a.SHA256, Size: a.Size, Cached: a.Cached}
	return nil
}

// Start starts the box and all scripts. Returns as soon as init has reported in.
func (b *Box) Start(ctx context.Context) error {
	b.e.quotaMu.Lock()
	if err := b.e.checkQuota(b.spec, true); err != nil {
		b.e.quotaMu.Unlock()
		return err
	}
	b.mu.Lock()
	b.e.quotaMu.Unlock() // from here on the box counts as running (state starting)
	if b.state == BoxStarting || b.state == BoxRunning || b.state == BoxStopping {
		b.mu.Unlock()
		return fmt.Errorf("box %q is already running", b.spec.Name)
	}
	b.state, b.errMsg, b.stopping, b.hung, b.viol, b.oomSeen = BoxStarting, "", false, false, 0, 0
	b.gen++
	select { // a new channel only after the last run ended: whoever waits before the first Start keeps theirs
	case <-b.finished:
		b.finished = make(chan struct{})
	default:
	}
	for _, s := range b.scripts {
		s.state, s.failures = ScriptPending, nil
	}
	gen := b.gen
	ctx, cancelStart := context.WithCancel(ctx) // Stop cancels it while the box is still starting
	b.startCancel = cancelStart
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.gen == gen {
			b.startCancel = nil
		}
		b.mu.Unlock()
		cancelStart()
	}()
	// failStart ends a failed start: BoxFailed with msg, or BoxExited if Stop came in between (then the
	// error says so instead of err).
	failStart := func(msg string, err error) error {
		b.mu.Lock()
		stopped := b.stopping
		b.mu.Unlock()
		if stopped {
			b.fail(BoxExited, "")
			return fmt.Errorf("box %q: stopped while starting", b.spec.Name)
		}
		b.fail(BoxFailed, msg)
		return err
	}

	rootfs, phash, err := b.e.profileRootFS(b.driver, b.spec.Profile)
	if err != nil {
		return failStart(err.Error(), err)
	}
	// Opt-in: the box starts with StartCPU cores; endStartBoost sets CPU afterwards.
	cpu, boost := b.spec.Limits.CPU, b.spec.Limits.StartCPU > b.spec.Limits.CPU
	if boost {
		cpu = b.spec.Limits.StartCPU
	}
	b.mu.Lock()
	b.profileHash = phash
	b.mu.Unlock()
	baseKey := b.e.baselineKey(b.driver.Name(), phash)
	drv, err := b.driver.Start(ctx, driver.Spec{
		ID: b.spec.Name, Limits: driver.Limits{MemoryBytes: b.spec.Limits.Memory, CPUCores: cpu, PIDs: b.spec.Limits.PIDs},
		ArtifactsDir: filepath.Join(b.dir(), "artifacts"), Init: b.e.cfg.init, Deno: b.e.cfg.deno,
		Stderr: &initLog{b: b}, RootFS: rootfs, OOMScoreAdj: b.e.cfg.oomAdj, BaselineRSS: b.e.baselineRSS(baseKey),
	})
	if err != nil {
		return failStart(err.Error(), err)
	}
	b.mu.Lock()
	b.drv = drv
	b.out = make(chan []byte, writeQueue)
	b.pingSent = map[uint64]bool{}
	b.lastPong = time.Now()
	b.mu.Unlock()
	go b.writer(drv.Stdin(), b.out)

	hello := make(chan error, 1)
	go b.readLoop(drv, gen, hello, baseKey)
	select {
	case err = <-hello:
	case <-time.After(helloTimeout):
		err = errors.New("tsbox-init does not report in")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		_ = drv.Kill()
		return failStart("Start: "+err.Error(), fmt.Errorf("box %q: %w", b.spec.Name, err))
	}
	b.mu.Lock()
	if b.stopping || b.gen != gen { // Stop came while init started: end the box instead of running it
		b.mu.Unlock()
		_ = drv.Kill() // boxEnded then sets BoxExited and closes finished
		return fmt.Errorf("box %q: stopped while starting", b.spec.Name)
	}
	b.state, b.started = BoxRunning, time.Now()
	for _, name := range b.order {
		b.startScriptLocked(b.scripts[name])
	}
	b.mu.Unlock()
	b.e.emit(Event{At: time.Now(), Kind: EventBoxStarted, Box: b.spec.Name, Detail: b.driver.Name()})
	go b.watchdog(drv, gen)
	if boost {
		go b.endStartBoost(drv, gen)
	}
	return nil
}

func (b *Box) fail(state BoxState, msg string) {
	b.mu.Lock()
	b.state, b.errMsg = state, msg
	b.closeFinishedLocked()
	b.mu.Unlock()
}

func (b *Box) closeFinishedLocked() {
	select {
	case <-b.finished:
	default:
		close(b.finished)
	}
}

// writer writes frames towards init. The queue is bounded; whoever fills it (init hangs) triggers the box
// watchdog instead of blocking the engine.
func (b *Box) writer(w io.WriteCloser, q <-chan []byte) {
	defer w.Close()
	for buf := range q {
		if _, err := w.Write(buf); err != nil {
			return
		}
	}
}

func (b *Box) sendLocked(f *proto.Frame) {
	if b.out == nil {
		return
	}
	buf, err := proto.Encode(f)
	if err != nil {
		// Only frames of the host's own making get here (start, stop, SDK results are checked before); losing
		// one silently left a script in "starting" for ever.
		b.e.cfg.logger.Error("frame to init cannot be encoded", "box", b.spec.Name, "type", f.T, "script", f.SID, "err", err)
		return
	}
	select {
	case b.out <- buf:
	default:
		b.e.cfg.logger.Warn("queue to init full", "box", b.spec.Name)
		if b.drv != nil {
			_ = b.drv.Kill()
		}
	}
}

func (b *Box) startScriptLocked(s *script) {
	f := &proto.Frame{T: proto.TStart, SID: s.spec.Name}
	l := s.spec.Limits
	st := proto.Start{
		HeapMiB: int(l.Heap >> 20), RSSBytes: l.RSS, Threads: l.Threads,
		TimeoutMs: l.Timeout.Milliseconds(), Params: s.spec.Params, DebugExec: s.spec.debugExec,
	}
	if l.Watchdog > 0 {
		st.WatchdogMs = l.Watchdog.Milliseconds()
	}
	if len(s.spec.debugExec) == 0 {
		st.Artifact, st.SHA256 = b.drv.ArtifactPath(filepath.Base(s.artifact.Path)), s.artifact.SHA256
	}
	if b.drv.UIDMode() {
		st.UID = s.uid
	}
	f.P = proto.Raw(st)
	s.endRunLocked()
	s.runSeq++
	s.run = newRun(s.runSeq)
	s.state, s.started = ScriptStarting, time.Now()
	s.tokens, s.tokensAt, s.heapOOM = logBurst, time.Now(), false
	b.sendLocked(f)
	b.e.emit(Event{At: time.Now(), Kind: EventScriptStarted, Box: b.spec.Name, Script: s.spec.Name})
}

func (b *Box) readLoop(drv driver.Box, gen int, hello chan<- error, baseKey string) {
	r := bufio.NewReaderSize(drv.Stdout(), 64<<10)
	first := true
	for {
		f, err := proto.Read(r)
		if err != nil {
			if first {
				hello <- fmt.Errorf("no hello from tsbox-init: %v", err)
			} else if !errors.Is(err, io.EOF) {
				b.violation("read error: "+err.Error(), true)
			}
			break
		}
		if first {
			first = false
			var h proto.Hello
			if f.T != proto.THello || proto.Payload(f, &h) != nil || h.Proto != proto.Version {
				hello <- fmt.Errorf("invalid hello from tsbox-init (%s)", f.T)
				break
			}
			b.e.rememberBaselineRSS(baseKey, h.BaselineRSS)
			hello <- nil
			continue
		}
		if !proto.FromInit[f.T] || f.T == proto.THello {
			b.violation("forbidden frame "+f.T, false)
			continue
		}
		b.dispatch(f)
	}
	<-drv.Done()
	b.boxEnded(drv, gen)
}

func (b *Box) violation(reason string, fatal bool) {
	b.mu.Lock()
	b.viol++
	n := b.viol
	drv := b.drv
	b.mu.Unlock()
	b.e.emit(Event{At: time.Now(), Kind: EventViolation, Box: b.spec.Name, Detail: reason})
	if (fatal || n >= maxViolations) && drv != nil {
		_ = drv.Kill()
	}
}

func (b *Box) dispatch(f *proto.Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if f.T == proto.TPong {
		if b.pingSent[f.ID] {
			delete(b.pingSent, f.ID)
			b.lastPong = time.Now()
		}
		return
	}
	if f.T == proto.TPStats {
		var list []proto.ProcStats
		if proto.Payload(f, &list) == nil {
			now := time.Now()
			for _, ps := range list {
				if s, ok := b.scripts[ps.SID]; ok {
					if !s.lastAt.IsZero() && ps.CPUNs >= s.lastCPU.CPUNs {
						s.cpu = float64(ps.CPUNs-s.lastCPU.CPUNs) / float64(now.Sub(s.lastAt).Nanoseconds())
					}
					s.stats, s.lastCPU, s.lastAt = ps, ps, now
				}
			}
		}
		return
	}
	s, ok := b.scripts[f.SID]
	if !ok {
		go b.violation("unknown script "+f.SID, false)
		return
	}
	// Any frame of the script itself shows that its process and @tsbox/core are up: starting -> running.
	// init forwards the first heartbeat of every start, so this also holds for scripts that never log or
	// only log through an SDK. "ready" stays the script's own opt-in signal (ready()).
	if s.state == ScriptStarting && proto.FromScript[f.T] {
		s.state = ScriptRunning
	}
	switch f.T {
	case proto.TAlive: // only the state change above
	case proto.TReady:
		s.state = ScriptReady
		b.e.emit(Event{At: time.Now(), Kind: EventScriptReady, Box: b.spec.Name, Script: s.spec.Name})
	case proto.TLog:
		b.logLocked(s, f.Level, f.Msg)
	case proto.TStats:
		var st proto.ScriptStats
		if proto.Payload(f, &st) == nil {
			s.heap = st.HeapUsed
		}
	case proto.TCall:
		b.callLocked(s, f)
	case proto.TSub, proto.TUnsub:
		b.subLocked(s, f, f.T == proto.TSub)
	case proto.TExit:
		var e proto.Exit
		if proto.Payload(f, &e) != nil {
			go b.violation("invalid exit", false)
			return
		}
		b.scriptExitedLocked(s, e)
	}
}

var (
	heapOOM = regexp.MustCompile(`(?i)(JavaScript heap out of memory|Fatal JavaScript out of memory|Reached heap limit)`)
	// v8Noise: lines of V8's "Last few GCs" block ("<--- Last few GCs --->", "[pid:0x...]   105 ms: Mark-Compact ...",
	// a lone "#").
	v8Noise = regexp.MustCompile(`^(<--- (Last few GCs|JS stacktrace) --->|\[\d+:[0-9A-Fa-fx]+\]\s+\d+ ms: .*|#)$`)
)

func (b *Box) logLocked(s *script, level, msg string) {
	switch level {
	case "debug", "info", "warn", "error", "stderr":
	default:
		level = "info"
	}
	// Check on arrival: on Windows the message is followed by a native stack trace that often has more than
	// 20 lines (before: a ring buffer of the last 20 lines, which missed it on Windows).
	// V8's GC trace before and the native stack trace after the OOM message say nothing to script authors
	// (about 60 lines on Windows); the OOM line itself and "script ended: heap" remain.
	if level == "stderr" {
		switch {
		case s.heapOOM || v8Noise.MatchString(msg):
			return
		case heapOOM.MatchString(msg):
			s.heapOOM = true
		}
	}
	now := time.Now()
	s.tokens = min(logBurst, s.tokens+now.Sub(s.tokensAt).Seconds()*logRate)
	s.tokensAt = now
	if s.tokens < 1 {
		return
	}
	s.tokens--
	l := LogLine{Seq: b.e.nextSeq(), At: now, Box: b.spec.Name, Script: s.spec.Name, Level: level, Msg: trunc(msg, maxLogMsg)}
	b.e.log(l)
	b.publish(l)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// scriptExitedLocked evaluates the end of a script and schedules the restart if needed.
func (b *Box) scriptExitedLocked(s *script, e proto.Exit) {
	s.endRunLocked()
	ex := &ExitStatus{At: time.Now(), Code: e.Code, Signal: e.Signal, Detail: e.Detail, Reason: ExitReason(e.Reason)}
	switch {
	case ex.Reason == "stopped" && b.stopping:
		ex.Reason = ReasonStopped
	case e.Reason == "init_shutdown":
		ex.Reason = ReasonBoxShutdown
	case e.Reason != "":
	case s.heapOOM:
		ex.Reason = ReasonHeap
	case e.Signal != "" || e.Code == -1 || e.Code == 137:
		ex.Reason = ReasonKilled
		if b.drv != nil {
			if st, err := b.drv.Stats(context.Background()); err == nil && st.OOMKills > b.oomSeen {
				b.oomSeen = st.OOMKills
				ex.Reason = ReasonOOM
				b.e.emit(Event{At: time.Now(), Kind: EventOOM, Box: b.spec.Name, Script: s.spec.Name})
			}
		}
	case e.Code == 0:
		ex.Reason = ReasonExit
	default:
		ex.Reason = ReasonCrash
	}
	s.lastExit = ex
	s.state = ScriptExited
	b.e.emit(Event{At: ex.At, Kind: EventScriptExited, Box: b.spec.Name, Script: s.spec.Name, Exit: ex})
	if ex.Reason == ReasonIntegrity {
		b.e.emit(Event{At: ex.At, Kind: EventIntegrityFailed, Box: b.spec.Name, Script: s.spec.Name})
	}

	switch {
	case b.stopping || b.state != BoxRunning:
	case ex.Reason == ReasonIntegrity || ex.Reason == ReasonProtocol:
		s.state = ScriptBroken // never restart automatically
	case s.policy.Mode == RestartAlways || (s.policy.Mode == RestartOnFailure && ex.Reason.Failure()):
		if ex.Reason.Failure() {
			cut := time.Now().Add(-s.policy.Window)
			kept := s.failures[:0]
			for _, t := range s.failures {
				if t.After(cut) {
					kept = append(kept, t)
				}
			}
			s.failures = append(kept, ex.At)
			if len(s.failures) >= s.policy.MaxFailures {
				s.state = ScriptBroken
				b.e.emit(Event{At: time.Now(), Kind: EventCircuitOpen, Box: b.spec.Name, Script: s.spec.Name,
					Detail: fmt.Sprintf("%d failures in %s", len(s.failures), shortDuration(s.policy.Window))})
				break
			}
		} else {
			s.failures = nil
		}
		delay := s.policy.BackoffMin << max(len(s.failures)-1, 0)
		if delay > s.policy.BackoffMax || delay <= 0 {
			delay = s.policy.BackoffMax
		}
		s.state = ScriptBackoff
		gen := b.gen
		b.e.emit(Event{At: time.Now(), Kind: EventRestartPlanned, Box: b.spec.Name, Script: s.spec.Name, Detail: delay.String()})
		time.AfterFunc(delay, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.gen == gen && b.state == BoxRunning && !b.stopping && s.state == ScriptBackoff {
				s.restarts++
				b.startScriptLocked(s)
			}
		})
	}
	b.maybeFinishLocked()
}

// maybeFinishLocked ends the box when no script runs any more or waits for a restart.
func (b *Box) maybeFinishLocked() {
	if b.stopping || b.state != BoxRunning {
		return
	}
	for _, s := range b.scripts {
		switch s.state {
		case ScriptStarting, ScriptRunning, ScriptReady, ScriptBackoff, ScriptPending:
			return
		}
	}
	b.stopping = true
	b.state = BoxStopping
	if b.out != nil {
		close(b.out) // stdin closed: init exits
		b.out = nil
	}
}

// watchdog pings init. If the answer does not come, the box hangs (spike f): cgroup.kill and quarantine.
func (b *Box) watchdog(drv driver.Box, gen int) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	lastTick := time.Now()
	for {
		select {
		case <-drv.Done():
			return
		case <-t.C:
			now := time.Now()
			b.mu.Lock()
			if b.gen != gen || b.out == nil {
				b.mu.Unlock()
				return
			}
			var hung bool
			b.lastPong, hung = checkPong(now, lastTick, b.lastPong)
			lastTick = now
			if hung {
				b.errMsg = "box does not answer (watchdog)"
				b.mu.Unlock()
				_ = drv.Kill()
				b.markHung()
				return
			}
			b.pingID++
			b.pingSent[b.pingID] = true
			if len(b.pingSent) > 8 {
				for id := range b.pingSent {
					if id+8 < b.pingID {
						delete(b.pingSent, id)
					}
				}
			}
			b.sendLocked(&proto.Frame{T: proto.TPing, ID: b.pingID})
			stats, err := drv.Stats(context.Background())
			if err == nil {
				if !b.lastStats.At.IsZero() {
					if dt := stats.At.Sub(b.lastStats.At); dt > 0 {
						b.cpuCores = float64(stats.CPUUsage-b.lastStats.CPUUsage) / float64(dt)
					}
				}
				b.lastStats = stats
			}
			b.mu.Unlock()
		}
	}
}

// checkPong decides whether a box has stopped answering. If the watchdog itself did not run for a while,
// the host was suspended or frozen and the box could not have answered either, so it gets a new deadline
// instead of being quarantined. A box that really hangs is caught one pongTimeout later.
//
// The gap is measured on both clocks, because the monotonic clock stops during suspend on some systems.
func checkPong(now, lastTick, lastPong time.Time) (newLastPong time.Time, hung bool) {
	gap := max(now.Sub(lastTick), now.Round(0).Sub(lastTick.Round(0)))
	if gap > hostPauseGap {
		return now, false
	}
	return lastPong, now.Sub(lastPong) > pongTimeout
}

func (b *Box) markHung() {
	b.mu.Lock()
	b.hung = true
	b.mu.Unlock()
}

// boxEnded cleans up after the sandbox has ended.
func (b *Box) boxEnded(drv driver.Box, gen int) {
	err := drv.Err()
	_ = drv.Close()
	b.mu.Lock()
	if b.gen != gen {
		b.mu.Unlock()
		return
	}
	if b.out != nil {
		close(b.out)
		b.out = nil
	}
	reason := ReasonBoxDied
	now := time.Now()
	switch {
	case b.hung:
		reason = ReasonBoxHung
	case errors.Is(err, driver.ErrBoxOOM):
		reason = ReasonOOM
		b.errMsg = err.Error()
		b.e.emit(Event{At: now, Kind: EventOOM, Box: b.spec.Name, Detail: err.Error()})
	}
	for _, s := range b.scripts {
		s.endRunLocked()
		switch s.state {
		case ScriptStarting, ScriptRunning, ScriptReady:
			r := reason
			if b.stopping {
				r = ReasonBoxShutdown
			}
			s.lastExit = &ExitStatus{At: now, Reason: r, Code: -1}
			s.state = ScriptExited
			b.e.emit(Event{At: now, Kind: EventScriptExited, Box: b.spec.Name, Script: s.spec.Name, Exit: s.lastExit})
		case ScriptBackoff, ScriptPending:
			s.state = ScriptExited
		}
	}
	switch {
	case b.state == BoxFailed:
	case b.stopping && !b.hung:
		b.state = BoxExited
	default:
		b.state = BoxQuarantined
		if b.errMsg == "" {
			b.errMsg = "box ended unexpectedly"
			if err != nil {
				b.errMsg += ": " + err.Error()
			}
		}
		b.e.emit(Event{At: now, Kind: EventBoxQuarantined, Box: b.spec.Name, Detail: b.errMsg})
	}
	b.drv = nil
	b.closeFinishedLocked()
	b.mu.Unlock()
	b.e.emit(Event{At: now, Kind: EventBoxExited, Box: b.spec.Name, Detail: string(b.Status().State)})
}

// Stop ends all scripts (with deadline grace, default 5 s) and then the box.
func (b *Box) Stop(ctx context.Context, grace time.Duration) error {
	if grace <= 0 {
		grace = stopGrace
	}
	b.mu.Lock()
	if b.state != BoxRunning && b.state != BoxStarting && b.state != BoxStopping {
		b.mu.Unlock()
		return nil
	}
	starting, cancelStart := b.state == BoxStarting, b.startCancel
	b.stopping = true
	b.state = BoxStopping
	for _, s := range b.scripts {
		if s.state == ScriptBackoff || s.state == ScriptPending {
			s.state = ScriptExited
		}
		b.sendLocked(&proto.Frame{T: proto.TStop, SID: s.spec.Name})
	}
	drv, finished := b.drv, b.finished
	b.mu.Unlock()
	if starting && cancelStart != nil {
		// Start is still waiting for the driver or for init's hello: let it end the box. Before this, Stop found no
		// driver to kill, gave up after 20 s and Start set the box running afterwards.
		cancelStart()
		select {
		case <-finished:
			return nil
		case <-time.After(15 * time.Second):
			return fmt.Errorf("box %q does not end", b.spec.Name)
		}
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for !b.allExited() {
		select {
		case <-finished:
			return nil
		case <-deadline.C:
			goto hard
		case <-ctx.Done():
			goto hard
		case <-t.C:
		}
	}
	b.mu.Lock()
	if b.out != nil {
		close(b.out)
		b.out = nil
	}
	b.mu.Unlock()
	select {
	case <-finished:
		return nil
	case <-time.After(stopGrace):
	}
hard:
	if drv != nil {
		_ = drv.Kill()
	}
	select {
	case <-finished:
		return nil
	case <-time.After(15 * time.Second):
		return fmt.Errorf("box %q does not end", b.spec.Name)
	}
}

func (b *Box) allExited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.scripts {
		switch s.state {
		case ScriptStarting, ScriptRunning, ScriptReady:
			return false
		}
	}
	return true
}

func (b *Box) terminal() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BoxExited, BoxFailed, BoxQuarantined, BoxCreated:
		return true
	}
	return false
}

// Wait waits until the box ends (all scripts finished and no restart scheduled, Stop or crash) and returns
// the status.
func (b *Box) Wait(ctx context.Context) (BoxStatus, error) {
	b.mu.Lock()
	finished := b.finished
	b.mu.Unlock()
	select {
	case <-finished:
		return b.Status(), nil
	case <-ctx.Done():
		return b.Status(), ctx.Err()
	}
}

// Status returns the current state.
func (b *Box) Status() BoxStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := BoxStatus{
		Name: b.spec.Name, Driver: b.driver.Name(), Isolated: b.driver.Isolated(), State: b.state,
		Error: b.errMsg, Started: b.started, Limits: b.spec.Limits, Labels: b.spec.Labels,
		SDKs: b.sdkInfo(), DroppedEvents: b.droppedEvents,
	}
	st.Profile, st.ProfileHash = b.spec.Profile, b.profileHash
	if st.Profile == "" {
		st.Profile = profile.Default
	}
	for _, name := range b.order {
		s := b.scripts[name]
		ss := ScriptStatus{Name: name, State: s.state, Restarts: s.restarts, Started: s.started, SHA256: s.artifact.SHA256}
		if s.lastExit != nil {
			e := *s.lastExit
			ss.LastExit = &e
		}
		st.Scripts = append(st.Scripts, ss)
	}
	return st
}

// Stats returns measurements of the box (fresh from the driver) and of the scripts (last report from init).
func (b *Box) Stats(ctx context.Context) (BoxStats, error) {
	b.mu.Lock()
	drv := b.drv
	b.mu.Unlock()
	var ds driver.Stats
	var err error
	if drv != nil {
		ds, err = drv.Stats(ctx)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := BoxStats{At: time.Now(), Memory: ds.MemoryBytes, MemoryPeak: ds.MemoryPeak, PIDs: ds.PIDs,
		OOMKills: ds.OOMKills, CPUCores: b.cpuCores}
	for _, name := range b.order {
		s := b.scripts[name]
		out.Scripts = append(out.Scripts, ScriptStats{Name: name, RSS: s.stats.RSS, Threads: s.stats.Threads,
			CPUCores: s.cpu, HeapUsed: s.heap})
	}
	return out, err
}

// initLog forwards init's stderr (not the scripts') to the engine logger.
type initLog struct{ b *Box }

func (l *initLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			l.b.e.cfg.logger.Warn("tsbox-init", "box", l.b.spec.Name, "msg", trunc(line, maxLogMsg))
		}
	}
	return len(p), nil
}

// startBoostMax limits the CPU boost at start (Limits.StartCPU).
var startBoostMax = 30 * time.Second

// onStartBoostEnd reports the end of the boost to tests (nil otherwise).
var onStartBoostEnd func(box string, err error)

// endStartBoost resets the box's CPU limit to Limits.CPU as soon as all scripts are ready or have ended,
// after startBoostMax at the latest. If that fails, the box is ended: it must never run above its limit
// for long.
func (b *Box) endStartBoost(drv driver.Box, gen int) {
	deadline := time.Now().Add(startBoostMax)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(deadline) && !b.startSettled(gen) {
		select {
		case <-drv.Done():
			return
		case <-t.C:
		}
	}
	err := errors.New("driver cannot change the CPU limit")
	if cs, ok := drv.(driver.CPUSetter); ok {
		err = cs.SetCPU(b.spec.Limits.CPU)
	}
	if onStartBoostEnd != nil {
		onStartBoostEnd(b.spec.Name, err)
	}
	if err != nil {
		b.e.cfg.logger.Error("CPU limit not reset after the start, ending the box", "box", b.spec.Name, "err", err)
		b.mu.Lock()
		b.errMsg = "CPU limit not reset after the start: " + err.Error()
		b.mu.Unlock()
		_ = drv.Kill()
	}
}

// startSettled: no script is starting any more (all ready, ended, in backoff or broken), or the box has
// been restarted meanwhile.
func (b *Box) startSettled(gen int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen != gen {
		return true
	}
	for _, s := range b.scripts {
		switch s.state {
		case ScriptPending, ScriptStarting, ScriptRunning:
			return false
		}
	}
	return true
}

// shortDuration writes a duration without trailing zero units: 10m instead of 10m0s, 1h instead of 1h0m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
