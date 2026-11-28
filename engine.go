package tsbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rif223/tsbox/internal/build"
	"github.com/rif223/tsbox/internal/denobin"
	"github.com/rif223/tsbox/internal/driver"
	"github.com/rif223/tsbox/internal/driver/dev"
	"github.com/rif223/tsbox/internal/driver/firecracker"
	"github.com/rif223/tsbox/internal/driver/gvisor"
	"github.com/rif223/tsbox/internal/logfile"
	"github.com/rif223/tsbox/internal/profile"
	cronprov "github.com/rif223/tsbox/internal/providers/cron"
	"github.com/rif223/tsbox/internal/providers/extproc"
	"github.com/rif223/tsbox/internal/providers/httpp"
	kvprov "github.com/rif223/tsbox/internal/providers/kv"
	logprov "github.com/rif223/tsbox/internal/providers/log"
	"github.com/rif223/tsbox/internal/sdkreg"
	"github.com/rif223/tsbox/kvstore"
	"github.com/rif223/tsbox/sdk"
)

// ErrNotIsolated: isolation required, but a box asks for the dev driver.
var ErrNotIsolated = errors.New("no isolation available: the dev driver does not isolate (TSBOX_REQUIRE_ISOLATION is set)")

// ErrNoDriver: a box names no driver. Every box chooses its own; there is no default.
var ErrNoDriver = errors.New("no driver: every box needs one (gvisor, firecracker, dev)")

// Option configures an engine.
type Option func(*config)

type config struct {
	stateDir         string
	driver           string
	requireIsolation bool
	deno             string
	init             string
	rootfs           string
	profilesDir      string
	oomAdj           int
	oomAdjSet        bool
	noBuildCache     bool
	platform         string
	runsc            string
	firecracker      string
	fcKernel         string
	fcJailer         string
	onEvent          func(Event)
	onLog            func(LogLine)
	logger           *slog.Logger
	providers        map[string]sdk.Provider
	sdkDirs          []string
	noLogFiles       bool
	logMax           int64
	kvStore          kvstore.Store
	quotaLabel       string
	quotaFn          func(tenant string) Quota
}

// WithoutLogFiles turns off the per-script log files (Box.Logs then returns only new lines).
func WithoutLogFiles() Option { return func(c *config) { c.noLogFiles = true } }

// WithLogFileSize sets the size from which a log file is rotated (default 10 MiB, two files).
func WithLogFileSize(n int64) Option { return func(c *config) { c.logMax = n } }

// WithStateDir sets the state directory (artifacts, boxes, runsc state).
func WithStateDir(dir string) Option { return func(c *config) { c.stateDir = dir } }

// WithRequireIsolation forbids the dev driver (also via TSBOX_REQUIRE_ISOLATION=1).
func WithRequireIsolation() Option { return func(c *config) { c.requireIsolation = true } }

// WithDeno sets the host's Deno binary (build, dev driver). Default: search via denobin.
func WithDeno(path string) Option { return func(c *config) { c.deno = path } }

// WithInit sets the binary that runs "__init" (default: the own one, see Init).
func WithInit(path string) Option { return func(c *config) { c.init = path } }

// WithRootFS sets the rootfs for gVisor (default: TSBOX_ROOTFS or /var/lib/tsbox/rootfs).
func WithRootFS(dir string) Option { return func(c *config) { c.rootfs = dir } }

// WithProfilesDir sets the directory of named profiles (default: TSBOX_PROFILES or /var/lib/tsbox/profiles).
// Profile <name> is the rootfs <dir>/<name>; "default" is the rootfs from WithRootFS.
func WithProfilesDir(dir string) Option { return func(c *config) { c.profilesDir = dir } }

// WithoutBuildCache turns off the build cache: every script is rebuilt on every start.
func WithoutBuildCache() Option { return func(c *config) { c.noBuildCache = true } }

// WithGVisorPlatform selects the gVisor platform: "systrap" (default, everywhere) or "kvm" (needs /dev/kvm,
// faster for system calls). Default: TSBOX_PLATFORM.
func WithGVisorPlatform(p string) Option { return func(c *config) { c.platform = p } }

// WithFirecracker sets the Firecracker binary (default: TSBOX_FIRECRACKER or firecracker from PATH).
func WithFirecracker(path string) Option { return func(c *config) { c.firecracker = path } }

// WithFirecrackerKernel sets the guest kernel of the firecracker driver (default: TSBOX_FC_KERNEL or
// DefaultFirecrackerKernel).
func WithFirecrackerKernel(path string) Option { return func(c *config) { c.fcKernel = path } }

// WithFirecrackerJailer runs every VMM under Firecracker's jailer (chroot, own UID/GID per VM, own mount
// namespace). path is the jailer binary; "" = without jailer (default: TSBOX_FC_JAILER; "1" means jailer from
// PATH).
func WithFirecrackerJailer(path string) Option { return func(c *config) { c.fcJailer = path } }

// DefaultFirecrackerKernel is where "tsbox setup --firecracker" puts the guest kernel.
const DefaultFirecrackerKernel = "/var/lib/tsbox/firecracker/vmlinux"

// WithOOMScoreAdj sets oom_score_adj of the host processes of every box (gVisor: all runsc processes; dev
// on Linux: init and scripts). Default 500: when the host runs out of memory, the kernel ends boxes first,
// not tsboxd or other services. 0 leaves the value unchanged.
func WithOOMScoreAdj(n int) Option { return func(c *config) { c.oomAdj, c.oomAdjSet = n, true } }

// WithEvents registers an event receiver. It runs in its own goroutine; if its queue (1,024) is full,
// events are discarded and counted.
func WithEvents(fn func(Event)) Option { return func(c *config) { c.onEvent = fn } }

// WithLogs registers a receiver for script logs (same rules as WithEvents).
func WithLogs(fn func(LogLine)) Option { return func(c *config) { c.onLog = fn } }

// WithLogger sets the logger for messages of the engine itself.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithProvider registers a built-in provider (Go) under the name manifests use in
// "provider": {"builtin": name}. Bundled are log, kv, http, inbox and cron.
func WithProvider(name string, p sdk.Provider) Option {
	return func(c *config) {
		if c.providers == nil {
			c.providers = map[string]sdk.Provider{}
		}
		c.providers[name] = p
	}
}

// WithKVStore replaces the storage of @tsbox/kv (default: kvstore.FileStore below <state>/kv), e.g. with
// a store backed by PostgreSQL. Quotas, key and value limits stay those of the SDK's configuration.
func WithKVStore(s kvstore.Store) Option { return func(c *config) { c.kvStore = s } }

// WithSDKDir adds a directory in which SDKs (<dir>/<name>/tsbox-sdk.json each) are found by name, e.g.
// the SDKs of an embedding application.
func WithSDKDir(dir string) Option { return func(c *config) { c.sdkDirs = append(c.sdkDirs, dir) } }

// Engine manages boxes via drivers, each loaded when the first box asks for it.
type Engine struct {
	cfg config

	drvMu   sync.Mutex
	drivers map[string]driver.Driver
	drvErr  map[string]driverErr // failed loads, retried after driverRetry

	events  chan Event
	logs    chan LogLine
	dropped atomic.Uint64
	chMu    sync.RWMutex
	closed  bool
	wg      sync.WaitGroup

	mu    sync.Mutex
	boxes map[string]*Box

	reg       *sdkreg.Registry
	providers map[string]sdk.Provider
	extMu     sync.Mutex
	ext       map[string]*extproc.Provider
	cacheMu   sync.Mutex // build cache of the build sandbox
	base      baselines  // RSS baseline of an empty Deno per rootfs (gVisor), see baseline.go
	phMu      sync.Mutex
	phCache   map[string]profileHashEntry // rootfs directory → fingerprint (profileHash)
	tcMu      sync.Mutex
	toolchain map[string]string // Deno binary (path, size, mtime) → version

	logSeq atomic.Int64
	sink   chan sinkItem // log lines → files
	logwMu sync.Mutex
	logw   map[string]*logfile.Writer

	calls      callCounter            // finished SDK calls (SDKCalls)
	quotaMu    sync.Mutex             // serializes quota check and start, so two starts cannot both fit
	buildSlots map[bool]chan struct{} // real builds at the same time, per isolating (true) or not
	pruned     atomic.Int64           // UnixNano of the last cleanup of the artifact store
}

// New creates an engine. It loads no driver yet: every box names its own (BoxSpec.Driver).
func New(ctx context.Context, opts ...Option) (*Engine, error) {
	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}
	if os.Getenv("TSBOX_REQUIRE_ISOLATION") == "1" {
		cfg.requireIsolation = true
	}
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	if cfg.stateDir == "" {
		cfg.stateDir = defaultStateDir()
	}
	if err := os.MkdirAll(cfg.stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("state directory %s: %w", cfg.stateDir, err)
	}
	if cfg.deno == "" {
		d, err := denobin.Find()
		if err != nil {
			return nil, err
		}
		cfg.deno = d
	}
	if cfg.init == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cfg.init = exe
	}
	if cfg.rootfs == "" {
		cfg.rootfs = os.Getenv("TSBOX_ROOTFS")
		if cfg.rootfs == "" {
			cfg.rootfs = "/var/lib/tsbox/rootfs"
		}
	}
	if cfg.platform == "" {
		cfg.platform = os.Getenv("TSBOX_PLATFORM")
	}
	if cfg.profilesDir == "" {
		cfg.profilesDir = os.Getenv("TSBOX_PROFILES")
		if cfg.profilesDir == "" {
			cfg.profilesDir = "/var/lib/tsbox/profiles"
		}
	}
	if cfg.firecracker == "" {
		cfg.firecracker = os.Getenv("TSBOX_FIRECRACKER")
	}
	if cfg.fcKernel == "" {
		cfg.fcKernel = os.Getenv("TSBOX_FC_KERNEL")
		if cfg.fcKernel == "" {
			cfg.fcKernel = DefaultFirecrackerKernel
		}
	}
	if cfg.fcJailer == "" {
		cfg.fcJailer = os.Getenv("TSBOX_FC_JAILER")
	}
	if cfg.fcJailer == "1" {
		cfg.fcJailer = "jailer"
	}
	if !cfg.oomAdjSet {
		cfg.oomAdj = DefaultOOMScoreAdj
	}
	if cfg.oomAdj < -1000 || cfg.oomAdj > 1000 {
		return nil, fmt.Errorf("oom_score_adj %d outside [-1000, 1000]", cfg.oomAdj)
	}
	e := &Engine{cfg: cfg, drivers: map[string]driver.Driver{}, drvErr: map[string]driverErr{}, boxes: map[string]*Box{}, reg: sdkreg.New(cfg.stateDir, cfg.sdkDirs...),
		ext: map[string]*extproc.Provider{}, providers: map[string]sdk.Provider{
			"log":  logprov.Provider{},
			"kv":   kvProvider(cfg),
			"http": httpp.Provider{},
		}}
	e.providers["inbox"] = inboxProvider{e}
	e.providers["cron"] = cronprov.New()
	for name, p := range cfg.providers {
		e.providers[name] = p
	}
	e.logw = map[string]*logfile.Writer{}
	e.buildSlots = map[bool]chan struct{}{true: make(chan struct{}, buildSlots(true)), false: make(chan struct{}, buildSlots(false))}
	if !cfg.noLogFiles {
		e.sink = make(chan sinkItem, 4096)
		e.wg.Add(1)
		go e.sinkLoop()
	}
	if cfg.onEvent != nil {
		e.events = make(chan Event, 1024)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			for ev := range e.events {
				cfg.onEvent(ev)
			}
		}()
	}
	if cfg.onLog != nil {
		e.logs = make(chan LogLine, 1024)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			for l := range e.logs {
				cfg.onLog(l)
			}
		}()
	}
	return e, nil
}

func kvProvider(cfg config) sdk.Provider {
	if cfg.kvStore != nil {
		return kvprov.NewWithStore(cfg.kvStore)
	}
	return kvprov.New(filepath.Join(cfg.stateDir, "kv"))
}

func selectDriver(cfg config) (driver.Driver, error) {
	gv := func() (driver.Driver, error) {
		return gvisor.New(gvisor.Config{Runsc: cfg.runsc, RootFS: cfg.rootfs, StateDir: filepath.Join(cfg.stateDir, "gvisor"),
			Platform: cfg.platform})
	}
	switch cfg.driver {
	case "gvisor":
		return gv()
	case "firecracker":
		d, err := firecracker.New(firecracker.Config{Firecracker: cfg.firecracker, Kernel: cfg.fcKernel,
			RootFS: cfg.rootfs, StateDir: filepath.Join(cfg.stateDir, "firecracker"), Jailer: cfg.fcJailer})
		if err != nil {
			return nil, err
		}
		d.SetInit(cfg.init)
		return d, nil
	case "dev":
		return dev.New(), nil
	case "":
		return nil, ErrNoDriver
	}
	return nil, fmt.Errorf("unknown driver %q (gvisor, firecracker, dev)", cfg.driver)
}

// DefaultStateDir is the state directory without WithStateDir (TSBOX_STATE, otherwise system-dependent).
func DefaultStateDir() string { return defaultStateDir() }

func defaultStateDir() string {
	if v := os.Getenv("TSBOX_STATE"); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "tsbox")
		}
	case "linux":
		if os.Geteuid() == 0 {
			return "/var/lib/tsbox"
		}
		if v := os.Getenv("XDG_STATE_HOME"); v != "" {
			return filepath.Join(v, "tsbox")
		}
	}
	if d, err := os.UserConfigDir(); err == nil && runtime.GOOS == "darwin" {
		return filepath.Join(d, "tsbox")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tsbox")
}

// DriverNames are the drivers tsbox knows on this platform.
func DriverNames() []string {
	if runtime.GOOS == "linux" {
		return []string{"gvisor", "firecracker", "dev"}
	}
	return []string{"dev"}
}

// DriverInfo describes a driver of the engine.
type DriverInfo struct {
	Name      string `json:"name"`
	Isolated  bool   `json:"isolated"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"` // why it is not available
	Note      string `json:"note,omitempty"`  // e.g. why gVisor uses the kvm platform
}

// Drivers reports which drivers boxes can use. A driver that is not loaded yet is loaded to find out.
func (e *Engine) Drivers() []DriverInfo {
	var out []DriverInfo
	for _, name := range DriverNames() {
		di := DriverInfo{Name: name, Isolated: name != "dev"}
		d, err := e.driverFor(name)
		if err != nil {
			di.Error = err.Error()
		} else {
			di.Available = true
			if n, ok := d.(interface{ Note() string }); ok {
				di.Note = n.Note()
			}
		}
		out = append(out, di)
	}
	return out
}

type driverErr struct {
	err error
	at  time.Time
}

// driverRetry: a driver that could not be loaded is tried again after this time (it may have been set up
// in the meantime).
const driverRetry = 30 * time.Second

// driverFor returns a driver by name, loading it on first use.
func (e *Engine) driverFor(name string) (driver.Driver, error) {
	if name == "" {
		return nil, ErrNoDriver
	}
	e.drvMu.Lock()
	defer e.drvMu.Unlock()
	if d, ok := e.drivers[name]; ok {
		return d, nil
	}
	if f, ok := e.drvErr[name]; ok && time.Since(f.at) < driverRetry {
		return nil, f.err
	}
	if !slices.Contains(DriverNames(), name) {
		return nil, fmt.Errorf("driver %q is not available on %s", name, runtime.GOOS)
	}
	cfg := e.cfg
	cfg.driver = name
	d, err := selectDriver(cfg)
	if err == nil && cfg.requireIsolation && !d.Isolated() {
		err = ErrNotIsolated
	}
	if err != nil {
		if !strings.HasPrefix(err.Error(), name+": ") {
			err = fmt.Errorf("%s: %w", name, err)
		}
		e.drvErr[name] = driverErr{err, time.Now()}
		return nil, err
	}
	delete(e.drvErr, name)
	e.drivers[name] = d
	return d, nil
}

// DroppedEvents counts discarded events and logs (full queue).
func (e *Engine) DroppedEvents() uint64 { return e.dropped.Load() }

// Run builds all scripts, starts the box and returns as soon as init is ready.
func (e *Engine) Run(ctx context.Context, spec BoxSpec) (*Box, error) {
	b, err := e.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := b.Start(ctx); err != nil {
		e.forget(b.spec.Name, b)
		return nil, err
	}
	return b, nil
}

// Create validates the specification and builds all scripts, but does not start the box yet.
func (e *Engine) Create(ctx context.Context, spec BoxSpec) (*Box, error) {
	spec, drv, active, imports, err := e.prepareSpec(spec)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if old, ok := e.boxes[spec.Name]; ok && !old.terminal() {
		e.mu.Unlock()
		return nil, fmt.Errorf("box %q already exists", spec.Name)
	}
	if err := e.boxQuotaLocked(spec); err != nil { // under e.mu: two Creates cannot both take the last slot
		e.mu.Unlock()
		return nil, err
	}
	b := newBox(e, spec, drv)
	b.sdks, b.imports = active, imports
	e.boxes[spec.Name] = b
	e.mu.Unlock()
	if err := b.build(ctx); err != nil {
		e.forget(spec.Name, b)
		return nil, err
	}
	return b, nil
}

// prepareSpec validates a specification and resolves its driver, profile and SDKs.
func (e *Engine) prepareSpec(spec BoxSpec) (BoxSpec, driver.Driver, map[string]*activeSDK, map[string]string, error) {
	spec, err := normalize(spec)
	if err != nil {
		return spec, nil, nil, nil, err
	}
	drv, err := e.driverFor(spec.Driver)
	if err != nil {
		return spec, nil, nil, nil, err
	}
	if err := e.checkProfile(drv, spec.Profile); err != nil {
		return spec, nil, nil, nil, err
	}
	active, imports, err := e.resolveSDKs(spec)
	return spec, drv, active, imports, err
}

func (e *Engine) forget(name string, b *Box) {
	e.mu.Lock()
	if e.boxes[name] == b {
		delete(e.boxes, name)
	}
	e.mu.Unlock()
}

// Box returns a box by name.
func (e *Engine) Box(name string) (*Box, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.boxes[name]
	return b, ok
}

// List returns the status of all boxes, sorted by name.
func (e *Engine) List() []BoxStatus {
	e.mu.Lock()
	list := make([]*Box, 0, len(e.boxes))
	for _, b := range e.boxes {
		list = append(list, b)
	}
	e.mu.Unlock()
	out := make([]BoxStatus, 0, len(list))
	for _, b := range list {
		out = append(out, b.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close stops all boxes and ends the event delivery.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	list := make([]*Box, 0, len(e.boxes))
	for _, b := range e.boxes {
		list = append(list, b)
	}
	e.mu.Unlock()
	var errs []error
	for _, b := range list {
		if err := b.Stop(ctx, 0); err != nil {
			errs = append(errs, err)
		}
	}
	e.chMu.Lock()
	if !e.closed {
		e.closed = true
		if e.events != nil {
			close(e.events)
		}
		if e.logs != nil {
			close(e.logs)
		}
		if e.sink != nil {
			close(e.sink)
		}
	}
	e.chMu.Unlock()
	e.wg.Wait()
	e.logwMu.Lock()
	for name, w := range e.logw {
		_ = w.Close()
		delete(e.logw, name)
	}
	e.logwMu.Unlock()
	e.extMu.Lock()
	for _, p := range e.ext {
		_ = p.Close()
	}
	e.ext = map[string]*extproc.Provider{}
	e.extMu.Unlock()
	e.drvMu.Lock()
	drivers := make([]driver.Driver, 0, len(e.drivers))
	for _, d := range e.drivers {
		drivers = append(drivers, d)
	}
	e.drvMu.Unlock()
	for _, d := range drivers {
		if c, ok := d.(driver.Closer); ok {
			_ = c.Close()
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) emit(ev Event) {
	e.chMu.RLock()
	defer e.chMu.RUnlock()
	if e.events == nil || e.closed {
		return
	}
	select {
	case e.events <- ev:
	default:
		e.dropped.Add(1)
	}
}

func (e *Engine) log(l LogLine) {
	e.toFile(l)
	e.chMu.RLock()
	defer e.chMu.RUnlock()
	if e.logs == nil || e.closed {
		return
	}
	select {
	case e.logs <- l:
	default:
		e.dropped.Add(1)
	}
}

// Check fully validates a box specification (SDKs, rights, configuration) and builds all scripts (with
// type checking) without starting anything. Returns the artifact hashes per script.
func (e *Engine) Check(ctx context.Context, spec BoxSpec) (map[string]string, error) {
	spec, drv, active, imports, err := e.prepareSpec(spec)
	if err != nil {
		return nil, err
	}
	b := newBox(e, spec, drv)
	b.sdks, b.imports = active, imports
	b.checkOnly = true
	if err := b.build(ctx); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, s := range b.scripts {
		out[name] = s.artifact.SHA256
	}
	return out, nil
}

// bundle builds a script with the driver of its box (gvisor/firecracker: in a build sandbox without network,
// otherwise on the host).
func (e *Engine) bundle(ctx context.Context, drv driver.Driver, entry string, imports map[string]string) (build.Artifact, error) {
	o := build.Options{Deno: e.cfg.deno, StoreDir: filepath.Join(e.cfg.stateDir, "artifacts"), Imports: imports,
		NoCache: e.cfg.noBuildCache, Slots: e.buildSlots[drv.Isolated()]}
	deno := e.cfg.deno
	// Under gVisor the build runs in its own sandbox without network; dev builds on the host with
	// --no-remote --no-npm.
	if sd, ok := drv.(sandboxBuilder); ok {
		e.cacheMu.Lock()
		err := sd.EnsureBuildCache(ctx)
		e.cacheMu.Unlock()
		if err != nil {
			return build.Artifact{}, err
		}
		o.Runner = sd.RunBuild
		deno = sd.RootDeno()
	}
	o.Toolchain = e.toolchainID(ctx, deno)
	a, err := build.Bundle(ctx, o, entry)
	e.pruneStore(o.StoreDir) // after the build: what it just used counts as fresh
	return a, err
}

// buildSlots is the number of builds an engine runs at the same time, for isolating drivers and for the
// others separately. A build in a sandbox or microVM may use up to 768 MiB, so isolating drivers run fewer.
func buildSlots(isolated bool) int {
	if isolated {
		return min(2, max(1, runtime.GOMAXPROCS(0)))
	}
	return max(1, min(4, runtime.GOMAXPROCS(0)))
}

// pruneStore removes unused entries of the artifact store, at most once per hour and engine.
func (e *Engine) pruneStore(dir string) {
	now, last := time.Now().UnixNano(), e.pruned.Load()
	if now-last < int64(time.Hour) || !e.pruned.CompareAndSwap(last, now) {
		return
	}
	if n := build.Prune(dir, build.PruneAge); n > 0 {
		e.cfg.logger.Info("artifact store cleaned up", "removed", n)
	}
}

// sandboxBuilder: drivers that build in a sandbox of their own (gvisor, firecracker) and use profiles.
type sandboxBuilder interface {
	EnsureBuildCache(ctx context.Context) error
	RunBuild(ctx context.Context, inv build.Invocation) ([]byte, []byte, error)
	RootDeno() string
}

func usesProfiles(drv driver.Driver) bool {
	_, ok := drv.(sandboxBuilder)
	return ok
}

// StateDir is the engine's state directory.
func (e *Engine) StateDir() string { return e.cfg.stateDir }

// DefaultOOMScoreAdj is the oom_score_adj of the box processes without WithOOMScoreAdj.
const DefaultOOMScoreAdj = 500

// ProfileInfo describes a named profile (rootfs for gVisor).
type ProfileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	Entries int    `json:"entries"`
	Debian  string `json:"debian,omitempty"`
}

func (e *Engine) profiles() profile.Set {
	return profile.Set{RootFS: e.cfg.rootfs, Dir: e.cfg.profilesDir}
}

// Profiles returns all existing profiles ("default" first). Only the gVisor driver uses them.
func (e *Engine) Profiles() []ProfileInfo {
	var out []ProfileInfo
	for _, p := range e.profiles().List() {
		out = append(out, ProfileInfo(p))
	}
	return out
}

// profileRootFS resolves the profile of a box: rootfs and fingerprint under gVisor, otherwise empty.
func (e *Engine) profileRootFS(drv driver.Driver, name string) (string, string, error) {
	if err := profile.ValidName(name); err != nil {
		return "", "", err
	}
	if !usesProfiles(drv) {
		return "", "", nil
	}
	dir, err := e.profiles().Resolve(name)
	if err != nil {
		return "", "", err
	}
	h, err := e.profileHash(dir)
	return dir, h, err
}

// profileHashTTL: how long a rootfs fingerprint is reused. Walking a rootfs (about 7000 entries) took about
// 50 ms of a gVisor start of about 250 ms. The fingerprint is shown in the status and keys the RSS baseline;
// a change of the rootfs shows within a minute, at once if the directory itself changed (replaced rootfs).
const profileHashTTL = time.Minute

type profileHashEntry struct {
	hash  string
	mtime time.Time
	at    time.Time
}

// profileHash returns profile.Hash(dir), reused for profileHashTTL while the directory's mtime is unchanged.
func (e *Engine) profileHash(dir string) (string, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	e.phMu.Lock()
	c, ok := e.phCache[dir]
	e.phMu.Unlock()
	if ok && time.Since(c.at) < profileHashTTL && c.mtime.Equal(st.ModTime()) {
		return c.hash, nil
	}
	h, _, err := profile.Hash(dir)
	if err != nil {
		return "", err
	}
	e.phMu.Lock()
	if e.phCache == nil {
		e.phCache = map[string]profileHashEntry{}
	}
	e.phCache[dir] = profileHashEntry{hash: h, mtime: st.ModTime(), at: time.Now()}
	e.phMu.Unlock()
	return h, nil
}

// checkProfile checks the profile before the build (without fingerprint).
func (e *Engine) checkProfile(drv driver.Driver, name string) error {
	if err := profile.ValidName(name); err != nil {
		return err
	}
	if !usesProfiles(drv) {
		return nil
	}
	_, err := e.profiles().Resolve(name)
	return err
}

// toolchainID identifies a Deno binary for the build cache: version according to "deno --version",
// remembered per path, size and modification time. Empty (no cache) if the binary does not answer.
func (e *Engine) toolchainID(ctx context.Context, deno string) string {
	st, err := os.Stat(deno)
	if err != nil {
		return ""
	}
	k := fmt.Sprintf("%s|%d|%d", deno, st.Size(), st.ModTime().UnixNano())
	e.tcMu.Lock()
	defer e.tcMu.Unlock()
	if v, ok := e.toolchain[k]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, deno, "--version")
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "DENO_NO_UPDATE_CHECK=1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	v := strings.Join(strings.Fields(string(out)), " ")
	if e.toolchain == nil {
		e.toolchain = map[string]string{}
	}
	e.toolchain[k] = v
	return v
}
