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
	"github.com/rif223/tsbox/internal/providers/extproc"
	"github.com/rif223/tsbox/internal/providers/httpp"
	kvprov "github.com/rif223/tsbox/internal/providers/kv"
	logprov "github.com/rif223/tsbox/internal/providers/log"
	"github.com/rif223/tsbox/internal/sdkreg"
	"github.com/rif223/tsbox/sdk"
)

// ErrNotIsolated: isolation required, but only the dev driver is available.
var ErrNotIsolated = errors.New("no isolation available: the dev driver does not isolate (TSBOX_REQUIRE_ISOLATION is set)")

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
}

// WithoutLogFiles turns off the per-script log files (Box.Logs then returns only new lines).
func WithoutLogFiles() Option { return func(c *config) { c.noLogFiles = true } }

// WithLogFileSize sets the size from which a log file is rotated (default 10 MiB, two files).
func WithLogFileSize(n int64) Option { return func(c *config) { c.logMax = n } }

// WithStateDir sets the state directory (artifacts, boxes, runsc state).
func WithStateDir(dir string) Option { return func(c *config) { c.stateDir = dir } }

// WithDriver selects the driver: "auto" (default), "gvisor", "firecracker" or "dev". "auto" never chooses
// firecracker.
func WithDriver(name string) Option { return func(c *config) { c.driver = name } }

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
// "provider": {"builtin": name}. Bundled are log, kv and http.
func WithProvider(name string, p sdk.Provider) Option {
	return func(c *config) {
		if c.providers == nil {
			c.providers = map[string]sdk.Provider{}
		}
		c.providers[name] = p
	}
}

// WithSDKDir adds a directory in which SDKs (<dir>/<name>/tsbox-sdk.json each) are found by name, e.g.
// the SDKs of an embedding application.
func WithSDKDir(dir string) Option { return func(c *config) { c.sdkDirs = append(c.sdkDirs, dir) } }

// Engine manages boxes via a driver.
type Engine struct {
	cfg    config
	driver driver.Driver

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
	tcMu      sync.Mutex
	toolchain map[string]string // Deno binary (path, size, mtime) → version

	logSeq atomic.Int64
	sink   chan sinkItem // log lines → files
	logwMu sync.Mutex
	logw   map[string]*logfile.Writer
}

// New creates an engine. Without WithDriver it chooses gvisor if it is usable (Linux, root, runsc,
// rootfs), otherwise dev – then Driver().Isolated() reports false.
func New(ctx context.Context, opts ...Option) (*Engine, error) {
	cfg := config{driver: "auto"}
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
	drv, err := selectDriver(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.requireIsolation && !drv.Isolated() {
		return nil, ErrNotIsolated
	}
	e := &Engine{cfg: cfg, driver: drv, boxes: map[string]*Box{}, reg: sdkreg.New(cfg.stateDir, cfg.sdkDirs...),
		ext: map[string]*extproc.Provider{}, providers: map[string]sdk.Provider{
			"log":  logprov.Provider{},
			"kv":   kvprov.New(filepath.Join(cfg.stateDir, "kv")),
			"http": httpp.Provider{},
		}}
	for name, p := range cfg.providers {
		e.providers[name] = p
	}
	e.logw = map[string]*logfile.Writer{}
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
	case "auto", "":
		if runtime.GOOS == "linux" && os.Geteuid() == 0 {
			if _, err := exec.LookPath("runsc"); err == nil {
				if d, err := gv(); err == nil {
					return d, nil
				} else if cfg.platform != "" {
					return nil, err // explicitly chosen platform: no silent switch to dev
				} else {
					cfg.logger.Warn("gVisor not usable, using dev", "reason", err)
				}
			}
		}
		return dev.New(), nil
	}
	return nil, fmt.Errorf("unknown driver %q (auto, gvisor, firecracker, dev)", cfg.driver)
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

// DriverName and Isolated describe the selected driver.
func (e *Engine) DriverName() string { return e.driver.Name() }
func (e *Engine) Isolated() bool     { return e.driver.Isolated() }

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
	spec, err := normalize(spec)
	if err != nil {
		return nil, err
	}
	if err := e.checkProfile(spec.Profile); err != nil {
		return nil, err
	}
	active, imports, err := e.resolveSDKs(spec)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if old, ok := e.boxes[spec.Name]; ok && !old.terminal() {
		e.mu.Unlock()
		return nil, fmt.Errorf("box %q already exists", spec.Name)
	}
	b := newBox(e, spec)
	b.sdks, b.imports = active, imports
	e.boxes[spec.Name] = b
	e.mu.Unlock()
	if err := b.build(ctx); err != nil {
		e.forget(spec.Name, b)
		return nil, err
	}
	return b, nil
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
	spec, err := normalize(spec)
	if err != nil {
		return nil, err
	}
	if err := e.checkProfile(spec.Profile); err != nil {
		return nil, err
	}
	active, imports, err := e.resolveSDKs(spec)
	if err != nil {
		return nil, err
	}
	b := newBox(e, spec)
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

// bundle builds a script (gvisor/firecracker: in a build sandbox without network, otherwise on the host).
func (e *Engine) bundle(ctx context.Context, entry string, imports map[string]string) (build.Artifact, error) {
	o := build.Options{Deno: e.cfg.deno, StoreDir: filepath.Join(e.cfg.stateDir, "artifacts"), Imports: imports,
		NoCache: e.cfg.noBuildCache}
	deno := e.cfg.deno
	// Under gVisor the build runs in its own sandbox without network; dev builds on the host with
	// --no-remote --no-npm.
	if sd, ok := e.driver.(sandboxBuilder); ok {
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
	return build.Bundle(ctx, o, entry)
}

// sandboxBuilder: drivers that build in a sandbox of their own (gvisor, firecracker) and use profiles.
type sandboxBuilder interface {
	EnsureBuildCache(ctx context.Context) error
	RunBuild(ctx context.Context, inv build.Invocation) ([]byte, []byte, error)
	RootDeno() string
}

func (e *Engine) usesProfiles() bool {
	_, ok := e.driver.(sandboxBuilder)
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
func (e *Engine) profileRootFS(name string) (string, string, error) {
	if err := profile.ValidName(name); err != nil {
		return "", "", err
	}
	if !e.usesProfiles() {
		return "", "", nil
	}
	dir, err := e.profiles().Resolve(name)
	if err != nil {
		return "", "", err
	}
	h, _, err := profile.Hash(dir)
	return dir, h, err
}

// checkProfile checks the profile before the build (without fingerprint).
func (e *Engine) checkProfile(name string) error {
	if err := profile.ValidName(name); err != nil {
		return err
	}
	if !e.usesProfiles() {
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
