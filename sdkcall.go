package tsbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/proto"
	"github.com/rif223/tsbox/internal/providers/extproc"
	"github.com/rif223/tsbox/internal/sdkreg"
	"github.com/rif223/tsbox/manifest"
	"github.com/rif223/tsbox/sdk"
)

// SDKUse activates an SDK for a box: name (bundled/installed) or path to an SDK directory, granted rights
// and operator configuration (secrets as file:/env:).
type SDKUse struct {
	Name   string              `json:"name"`
	Grants map[string][]string `json:"grants,omitempty"`
	Config map[string]string   `json:"config,omitempty"`
	// Env replaces the process environment for "env:<NAME>" secrets (nil = os.LookupEnv). Never serialized;
	// tsboxd keeps the environment of the calling CLI process here, in memory.
	Env map[string]string `json:"-"`
}

// knownCodes are the error codes counted by name in SDKCalls.
var knownCodes = map[string]bool{sdk.InvalidArgument: true, sdk.PermissionDenied: true, sdk.NotFound: true,
	sdk.RateLimited: true, sdk.TooLarge: true, sdk.Timeout: true, sdk.Unavailable: true, sdk.QuotaExceeded: true,
	sdk.Internal: true}

// maxInflightPerScript limits all SDK calls of a script together.
const maxInflightPerScript = 16

var storagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// activeSDK is an SDK activated and validated for a box.
type activeSDK struct {
	reg      *sdkreg.SDK
	m        *manifest.Manifest
	provider sdk.Provider
	grants   sdk.Grants
	config   sdk.Config
}

// resolveSDKs finds, validates and binds all SDKs of a box and returns the import map for the build.
func (e *Engine) resolveSDKs(spec BoxSpec) (map[string]*activeSDK, map[string]string, error) {
	active := map[string]*activeSDK{}
	imports := map[string]string{}
	for _, use := range spec.SDKs {
		s, err := e.reg.Resolve(use.Name)
		if err != nil {
			return nil, nil, err
		}
		m := s.Manifest
		if _, dup := active[m.Name]; dup {
			return nil, nil, fmt.Errorf("SDK %q activated twice", m.Name)
		}
		if other, dup := imports[m.Import]; dup {
			return nil, nil, fmt.Errorf("SDKs %q and %q have the same import %q", m.Name, other, m.Import)
		}
		if err := m.CheckGrants(manifest.Grants(use.Grants)); err != nil {
			return nil, nil, err
		}
		lookup := os.LookupEnv
		if use.Env != nil {
			env := use.Env
			lookup = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
		}
		cfg, err := m.ResolveConfigEnv(use.Config, lookup)
		if err != nil {
			return nil, nil, err
		}
		p, err := e.providerFor(s, cfg)
		if err != nil {
			return nil, nil, err
		}
		if v, ok := p.(sdk.GrantValidator); ok {
			if err := v.ValidateGrants(sdk.Grants(use.Grants), sdk.Config(cfg)); err != nil {
				return nil, nil, fmt.Errorf("SDK %q: %w", m.Name, err)
			}
		}
		active[m.Name] = &activeSDK{reg: s, m: m, provider: p, grants: sdk.Grants(use.Grants), config: sdk.Config(cfg)}
		imports[m.Import] = s.ModulePath()
	}
	for _, a := range active {
		for _, req := range a.m.Requires {
			if _, ok := active[req]; !ok {
				return nil, nil, fmt.Errorf("SDK %q needs SDK %q: also start with --sdk %s", a.m.Name, req, req)
			}
		}
	}
	return active, imports, nil
}

func (e *Engine) providerFor(s *sdkreg.SDK, cfg map[string]string) (sdk.Provider, error) {
	m := s.Manifest
	if name := m.Provider.Builtin; name != "" {
		p, ok := e.providers[name]
		if !ok {
			return nil, fmt.Errorf("SDK %q needs the built-in provider %q, which this application has not registered", m.Name, name)
		}
		return p, nil
	}
	b, _ := json.Marshal(cfg)
	key := filepath.Clean(s.Dir) + "\x00" + s.Hash + "\x00" + string(b)
	e.extMu.Lock()
	defer e.extMu.Unlock()
	if p, ok := e.ext[key]; ok {
		return p, nil
	}
	p := extproc.New(extproc.Config{Name: m.Name, Version: m.Version, Dir: s.Dir, Exec: m.Provider.Exec,
		Deno: e.cfg.deno, Settings: cfg, Logger: e.cfg.logger})
	e.ext[key] = p
	return p, nil
}

// scriptRun is the SDK state of a script run (new on every start).
type scriptRun struct {
	id       int
	ctx      context.Context
	cancel   context.CancelFunc
	sessions map[string]*session
	subs     map[string]bool // "sdk:event"
	inflight map[string]int  // "sdk.method"
	total    int
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) take(rate float64, burst int) bool {
	now := time.Now()
	if b.at.IsZero() {
		b.tokens = float64(burst)
	} else {
		b.tokens = min(float64(burst), b.tokens+now.Sub(b.at).Seconds()*rate)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func newRun(id int) *scriptRun {
	ctx, cancel := context.WithCancel(context.Background())
	return &scriptRun{id: id, ctx: ctx, cancel: cancel, sessions: map[string]*session{}, subs: map[string]bool{},
		inflight: map[string]int{}}
}

// endRunLocked ends all sessions of a run (Handler.Close in its own goroutine).
func (s *script) endRunLocked() {
	r := s.run
	if r == nil {
		return
	}
	s.run = nil
	r.cancel()
	for _, sess := range r.sessions {
		go sess.close()
	}
}

// session connects a script run with a provider (implements sdk.Session).
type session struct {
	b   *Box
	s   *script
	run *scriptRun
	a   *activeSDK

	once    sync.Once
	handler sdk.Handler
	err     error
	closed  bool
	mu      sync.Mutex
}

func (ss *session) open() (sdk.Handler, error) {
	ss.once.Do(func() {
		ctx, cancel := context.WithTimeout(ss.run.ctx, 30*time.Second)
		defer cancel()
		ss.handler, ss.err = ss.a.provider.Open(ctx, ss)
		ss.mu.Lock()
		closed := ss.closed
		ss.mu.Unlock()
		if closed && ss.handler != nil {
			_ = ss.handler.Close()
		}
	})
	if ss.handler == nil && ss.err == nil {
		return nil, sdk.Errorf(sdk.Unavailable, "script ended")
	}
	return ss.handler, ss.err
}

func (ss *session) close() {
	ss.mu.Lock()
	ss.closed = true
	ss.mu.Unlock()
	// If Open has already run, close; if it is still running, open() closes itself.
	done := make(chan struct{})
	go func() { ss.once.Do(func() {}); close(done) }()
	select {
	case <-done:
		if ss.handler != nil {
			_ = ss.handler.Close()
		}
	case <-time.After(30 * time.Second):
	}
}

func (ss *session) Box() sdk.BoxRef {
	return sdk.BoxRef{Name: ss.b.spec.Name, Storage: ss.b.spec.Storage, Labels: ss.b.spec.Labels}
}
func (ss *session) Script() sdk.ScriptRef {
	return sdk.ScriptRef{Name: ss.s.spec.Name, SHA256: ss.s.artifact.SHA256}
}
func (ss *session) Grants() sdk.Grants { return ss.a.grants }
func (ss *session) Config() sdk.Config { return ss.a.config }
func (ss *session) Logger() *slog.Logger {
	return ss.b.e.cfg.logger.With("sdk", ss.a.m.Name, "box", ss.b.spec.Name, "script", ss.s.spec.Name)
}

func (ss *session) ScriptLog(level, msg string) {
	ss.b.mu.Lock()
	defer ss.b.mu.Unlock()
	if ss.s.run == ss.run {
		ss.b.logLocked(ss.s, level, msg)
	}
}

// Emit delivers an event if the script has subscribed to it. Never blocks.
func (ss *session) Emit(event string, payload any) error {
	if _, ok := ss.a.m.Events[event]; !ok {
		return sdk.ErrUnknownEvent
	}
	raw, ok := payload.(json.RawMessage)
	if !ok {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	ss.b.mu.Lock()
	defer ss.b.mu.Unlock()
	if ss.s.run != ss.run || !ss.run.subs[ss.a.m.Name+":"+event] {
		return sdk.ErrNotSubscribe
	}
	f := &proto.Frame{T: proto.TEvent, SIDs: []string{ss.s.spec.Name}, SDK: ss.a.m.Name, Name: event, P: raw}
	if !ss.b.sendEventLocked(f) {
		return sdk.ErrQueueFull
	}
	return nil
}

func (s *script) sessionLocked(b *Box, a *activeSDK) *session {
	ss, ok := s.run.sessions[a.m.Name]
	if !ok {
		ss = &session{b: b, s: s, run: s.run, a: a}
		s.run.sessions[a.m.Name] = ss
	}
	return ss
}

// callLocked checks an SDK call and runs it asynchronously. Unknown SDK and unknown method give the same
// answer (no oracle about installed SDKs).
func (b *Box) callLocked(s *script, f *proto.Frame) {
	a := b.sdks[f.SDK]
	var m *manifest.Method
	if a != nil {
		m = a.m.Methods[f.M]
	}
	reply := func(code, msg string) {
		sdkName, method := "_unknown", ""
		if a != nil && m != nil {
			sdkName, method = a.m.Name, f.M
		}
		b.e.calls.add(b.spec.Name, sdkName, method, code)
		b.sendLocked(&proto.Frame{T: proto.TRet, SID: s.spec.Name, ID: f.ID, Err: &proto.Error{Code: code, Msg: msg}})
	}
	if a == nil || m == nil || s.run == nil {
		reply(sdk.NotFound, fmt.Sprintf("%s.%s is not available (SDK not activated or method unknown)", trunc(f.SDK, 32), trunc(f.M, 32)))
		return
	}
	if m.Permission != "" && len(a.grants[m.Permission]) == 0 {
		reply(sdk.PermissionDenied, fmt.Sprintf("right %q not granted (--grant %s:%s=...)", m.Permission, a.m.Name, m.Permission))
		return
	}
	if int64(len(f.P)) > m.MaxParams {
		reply(sdk.TooLarge, fmt.Sprintf("parameters larger than %s", manifest.FormatBytes(m.MaxParams)))
		return
	}
	key := a.m.Name + "." + f.M
	// The buckets belong to the script, not to the run: a script that spends its burst and exits must not get a
	// full burst again with every restart (--restart always restarts after one second).
	if s.buckets == nil {
		s.buckets = map[string]*bucket{}
	}
	bk := s.buckets[key]
	if bk == nil {
		bk = &bucket{}
		s.buckets[key] = bk
	}
	if !bk.take(m.Rate, m.Burst) {
		reply(sdk.RateLimited, fmt.Sprintf("rate limit of %s (%g/s)", key, m.Rate))
		return
	}
	if s.run.inflight[key] >= m.Concurrency || s.run.total >= maxInflightPerScript {
		reply(sdk.RateLimited, "too many concurrent calls")
		return
	}
	s.run.inflight[key]++
	s.run.total++
	run, ss, id, params := s.run, s.sessionLocked(b, a), f.ID, f.P
	go b.execCall(s, run, ss, m, key, id, f.M, params)
}

func (b *Box) execCall(s *script, run *scriptRun, ss *session, m *manifest.Method, key string, id uint64, method string, params json.RawMessage) {
	var payload json.RawMessage
	var perr *proto.Error
	func() {
		defer func() {
			if r := recover(); r != nil {
				b.e.cfg.logger.Error("provider panic", "sdk", ss.a.m.Name, "panic", r, "stack", string(debug.Stack()))
				perr = &proto.Error{Code: sdk.Internal, Msg: "internal error"}
			}
		}()
		h, err := ss.open()
		if err != nil {
			perr = toProtoError(b, ss, err)
			return
		}
		ctx, cancel := context.WithTimeout(run.ctx, m.Timeout)
		defer cancel()
		res, err := h.Call(ctx, method, params)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = sdk.Errorf(sdk.Timeout, "no answer after %v", m.Timeout)
			}
			perr = toProtoError(b, ss, err)
			return
		}
		if raw, ok := res.(json.RawMessage); ok {
			payload = raw
		} else if payload, err = json.Marshal(res); err != nil {
			perr = toProtoError(b, ss, err)
			return
		}
		if int64(len(payload)) > m.MaxResult {
			payload, perr = nil, &proto.Error{Code: sdk.TooLarge, Msg: "result larger than " + manifest.FormatBytes(m.MaxResult)}
		}
	}()
	code := "ok"
	if perr != nil {
		code = perr.Code
		if !knownCodes[code] { // codes of external providers are not bounded
			code = "other"
		}
	}
	b.e.calls.add(b.spec.Name, ss.a.m.Name, method, code)
	b.mu.Lock()
	defer b.mu.Unlock()
	run.inflight[key]--
	run.total--
	if s.run == run {
		b.sendLocked(&proto.Frame{T: proto.TRet, SID: s.spec.Name, ID: id, P: payload, Err: perr})
	}
}

func toProtoError(b *Box, ss *session, err error) *proto.Error {
	var se *sdk.Error
	if errors.As(err, &se) {
		return &proto.Error{Code: se.Code, Msg: trunc(se.Msg, 512)}
	}
	if errors.Is(err, context.Canceled) {
		return &proto.Error{Code: sdk.Unavailable, Msg: "canceled"}
	}
	b.e.cfg.logger.Warn("SDK error", "sdk", ss.a.m.Name, "box", b.spec.Name, "script", ss.s.spec.Name, "err", err)
	return &proto.Error{Code: sdk.Internal, Msg: "internal error"}
}

// subLocked manages event subscriptions. Only events from the manifest of an active SDK count; the
// subscription opens the session so that the provider can send.
func (b *Box) subLocked(s *script, f *proto.Frame, on bool) {
	a := b.sdks[f.SDK]
	if a == nil || s.run == nil || len(f.Events) > 64 {
		return
	}
	for _, ev := range f.Events {
		if _, ok := a.m.Events[ev]; !ok {
			continue
		}
		if on {
			s.run.subs[a.m.Name+":"+ev] = true
		} else {
			delete(s.run.subs, a.m.Name+":"+ev)
		}
	}
	if on {
		ss := s.sessionLocked(b, a)
		go func() { _, _ = ss.open() }()
	}
}

// sendEventLocked queues an event; with a full queue it is discarded (counted), never blocking.
func (b *Box) sendEventLocked(f *proto.Frame) bool {
	if b.out == nil {
		return false
	}
	buf, err := proto.Encode(f)
	if err != nil {
		return false
	}
	select {
	case b.out <- buf:
		return true
	default:
		b.droppedEvents++
		return false
	}
}

// SDKInfo describes an active SDK of a box (without secrets).
type SDKInfo struct {
	Name    string              `json:"name"`
	Version string              `json:"version"`
	Source  string              `json:"source"`
	Hash    string              `json:"hash"`
	Grants  map[string][]string `json:"grants,omitempty"`
	Config  map[string]string   `json:"config,omitempty"` // secrets as "***"
}

func (b *Box) sdkInfo() []SDKInfo {
	var out []SDKInfo
	for _, a := range b.sdks {
		cfg := map[string]string{}
		for k, v := range a.config {
			cfg[k] = v
		}
		for _, k := range a.m.SecretKeys() {
			if _, ok := cfg[k]; ok {
				cfg[k] = "***"
			}
		}
		out = append(out, SDKInfo{Name: a.m.Name, Version: a.m.Version, Source: a.reg.Source, Hash: a.reg.Hash,
			Grants: a.grants, Config: cfg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ErrSDKInUse: ReleaseSDK refuses because a running box uses the SDK.
var ErrSDKInUse = errors.New("SDK in use")

// ReleaseSDK ends the provider processes of an installed SDK so that its files can be replaced or removed
// (tsbox sdk add/rm; on Windows open files cannot be deleted). It refuses while a running box uses the SDK.
func (e *Engine) ReleaseSDK(name string) error {
	dir := filepath.Clean(e.reg.InstalledDir(name))
	var users []string
	e.mu.Lock()
	for n, b := range e.boxes {
		if b.terminal() {
			continue
		}
		for _, a := range b.sdks {
			if a.reg != nil && filepath.Clean(a.reg.Dir) == dir {
				users = append(users, n)
				break
			}
		}
	}
	e.mu.Unlock()
	if len(users) > 0 {
		sort.Strings(users)
		return &sdkInUseError{name: name, boxes: users}
	}
	e.extMu.Lock()
	defer e.extMu.Unlock()
	for k, p := range e.ext {
		if strings.HasPrefix(k, dir+"\x00") {
			_ = p.Close()
			delete(e.ext, k)
		}
	}
	return nil
}

type sdkInUseError struct {
	name  string
	boxes []string
}

func (e *sdkInUseError) Error() string {
	return fmt.Sprintf("SDK %q is used by running boxes: %s", e.name, strings.Join(e.boxes, ", "))
}

func (e *sdkInUseError) Is(target error) bool { return target == ErrSDKInUse }
