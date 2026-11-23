// Package build turns a TypeScript script into a single JavaScript artifact (deno bundle) and stores it
// content-addressed. Only @tsbox/core and the active SDKs are included (import map); remote and npm imports
// are disabled, type errors abort the build (--check).
//
// deno runs with --no-config and a plain import map: with --config, Deno would use the nearest deno.json
// for modules in other directories (script, path SDK). One that maps @tsbox/core (repo, tsbox types)
// bundled core a second time (second transport, SDK call hangs) and made SDKs importable that were not
// activated.
//
// With Options.Runner the build runs in a sandbox without network (gVisor, sandbox.go), otherwise on the host.
package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/js"
)

// Options of a build.
type Options struct {
	Deno     string            // the host's Deno binary
	StoreDir string            // artifact store: <StoreDir>/sha256/<hash>.js
	Imports  map[string]string // further bare imports (SDK modules), name → path
	Timeout  time.Duration     // default 2 min
	// Runner runs deno in a build sandbox (gVisor without network). nil = on the host.
	Runner Runner
	// Context is the directory the build sandbox sees of the script (default: the script's directory).
	// Imports outside of it fail in the sandbox.
	Context string
	// Toolchain identifies the building Deno (version, host or sandbox) for the build cache. Empty =
	// no cache.
	Toolchain string
	NoCache   bool // bypass the build cache
	// Slots limits concurrent builds: a real build (not a cache hit) holds one element while it runs.
	// nil = no limit.
	Slots chan struct{}
}

// Mount is a host directory mounted read-only into the build sandbox.
type Mount struct{ Host, Box string }

// Invocation is a deno call in the build sandbox (paths as seen by the sandbox).
type Invocation struct {
	Args   []string
	Mounts []Mount
}

// Runner executes an invocation and returns stdout (the bundle) and stderr (diagnostics).
type Runner func(ctx context.Context, inv Invocation) (stdout, stderr []byte, err error)

// Artifact is a built script.
type Artifact struct {
	Path   string // in the store
	SHA256 string
	Size   int64
	Cached bool // from the build cache (not rebuilt)
}

// Error holds deno's diagnostics (type errors, forbidden imports) plus a hint.
type Error struct {
	Output string
	Hint   string
}

func (e *Error) Error() string {
	s := "build failed:\n" + strings.TrimSpace(denoNoise.ReplaceAllString(e.Output, ""))
	if e.Hint != "" {
		s += "\n\nhint: " + e.Hint
	}
	return s
}

var (
	ansi       = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	remoteHint = regexp.MustCompile(`(?i)(jsr:|npm:|npm specifiers|https?://|remote module|Do not know how to load path|--allow-import|import access)`)
	// denoNoise are Deno's advice lines that do not apply to tsbox (no --no-check, no deno add, a script
	// with type errors does not run).
	denoNoise = regexp.MustCompile(`(?m)^[ \t]*(info: The program failed type-checking, but it still might work correctly\.|hint: Re-run with --no-check to skip type-checking\.|hint: If you want to use a JSR or npm package, try running .*)\r?\n?`)
	bareHint  = regexp.MustCompile(`Import "([^"]+)" not a dependency|Relative import path "([^"]+)" not prefixed`)
)

// Bundle builds src (or takes the artifact from the build cache, cache.go) and puts it into the store.
func Bundle(ctx context.Context, o Options, src string) (Artifact, error) {
	src, err := filepath.Abs(src)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := os.Stat(src); err != nil {
		return Artifact{}, fmt.Errorf("%s not found", src)
	}
	if o.NoCache || o.Toolchain == "" {
		return bundle(ctx, o, src)
	}
	ctxDir := filepath.Dir(src)
	if o.Runner != nil && o.Context != "" {
		if ctxDir, err = filepath.Abs(o.Context); err != nil {
			return Artifact{}, err
		}
	}
	key, roots, err := cacheKey(o, ctxDir, src)
	if err != nil {
		return bundle(ctx, o, src) // e.g. context too large: without cache
	}
	if a, ok := cacheGet(o.StoreDir, key); ok {
		a.Cached = true
		return a, nil
	}
	// Host: record files outside the hashed directories (including pure type imports) before the build.
	var extra []input
	if o.Runner == nil {
		coreDir := filepath.Join(o.StoreDir, "work", coreHash(), "core")
		if err := writeCore(coreDir); err != nil {
			return Artifact{}, err
		}
		var ok bool
		if extra, ok = hostGraph(ctx, o, src, coreDir, append(roots, coreDir)); !ok {
			return bundle(ctx, o, src) // graph not resolvable: without cache (the build reports the error)
		}
	}
	a, err := bundle(ctx, o, src)
	if err != nil {
		return a, err
	}
	_ = cachePut(o.StoreDir, key, a.SHA256, extra)
	return a, nil
}

func bundle(ctx context.Context, o Options, src string) (Artifact, error) {
	if o.Slots != nil {
		select {
		case o.Slots <- struct{}{}:
			defer func() { <-o.Slots }()
		case <-ctx.Done():
			return Artifact{}, ctx.Err()
		}
	}
	// Fixed work directory per core version: deno bundle writes module paths as comments into the artifact,
	// a random directory would change the hash on every build.
	work := filepath.Join(o.StoreDir, "work", coreHash())
	coreDir := filepath.Join(work, "core")
	if err := writeCore(coreDir); err != nil {
		return Artifact{}, err
	}
	if o.Runner != nil {
		return bundleSandbox(ctx, o, src, coreDir)
	}
	tmp, err := os.MkdirTemp(work, "b-")
	if err != nil {
		return Artifact{}, err
	}
	defer fsutil.RemoveAll(tmp)
	cfg, err := importMap(o, coreDir)
	if err != nil {
		return Artifact{}, err
	}
	cfgPath := filepath.Join(tmp, "import_map.json")
	// The entry path is not in the bundle; in the random directory parallel builds do not collide.
	entry := filepath.Join(tmp, "entry.ts")
	// boot first: console, stdout and heartbeat are set up before script code runs.
	entrySrc := fmt.Sprintf("import \"@tsbox/core/boot\";\nimport %q;\n", FileURL(src))
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return Artifact{}, err
	}
	if err := os.WriteFile(entry, []byte(entrySrc), 0o600); err != nil {
		return Artifact{}, err
	}

	if o.Timeout == 0 {
		o.Timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	out := filepath.Join(tmp, "out.js")
	cmd := exec.CommandContext(ctx, o.Deno, "bundle", "--quiet", "--no-config", "--import-map", cfgPath, "--no-lock",
		"--no-remote", "--no-npm", "--check", "--platform", "deno", "--sourcemap=inline", "-o", out, entry)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	var diag bytes.Buffer
	cmd.Stdout, cmd.Stderr = &diag, &diag
	// If a grandchild (esbuild) inherits the pipes, Wait would otherwise wait for them forever (Windows hang).
	cmd.WaitDelay = 5 * time.Second
	if err := runTree(cmd); err != nil && !errors.Is(err, exec.ErrWaitDelay) { // ErrWaitDelay: deno exited with 0
		if ctx.Err() != nil {
			return Artifact{}, fmt.Errorf("build aborted after %v", o.Timeout)
		}
		text := ansi.ReplaceAllString(diag.String(), "")
		if text == "" {
			return Artifact{}, fmt.Errorf("deno bundle: %w", err)
		}
		return Artifact{}, &Error{Output: strings.ReplaceAll(text, FileURL(tmp)+"/", ""), Hint: hint(text)}
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return Artifact{}, err
	}
	// Sources are relative to out.js in the random directory tmp: make them absolute (dev runs on the host).
	b = fixSourceMap(b, func(s string) string {
		if strings.Contains(s, "://") {
			return s
		}
		p := filepath.Join(tmp, filepath.FromSlash(s))
		if r, err := filepath.Rel(tmp, p); err == nil && !strings.HasPrefix(r, "..") {
			return "entry.ts" // the generated entry: no random path in the artifact
		}
		return FileURL(p)
	})
	return Store(o.StoreDir, b)
}

// Store puts b into the store content-addressed (atomically, write-protected) and returns the artifact.
func Store(dir string, b []byte) (Artifact, error) {
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	dst := filepath.Join(dir, "sha256", h+".js")
	if _, err := os.Stat(dst); err == nil {
		touch(dst)
		return Artifact{Path: dst, SHA256: h, Size: int64(len(b))}, nil
	}
	// One writer per process: on Windows renaming onto the read-only file of a parallel build with the
	// same content fails, and fsutil.Rename would retry for about 4 s.
	storeMu.Lock()
	defer storeMu.Unlock()
	if _, err := os.Stat(dst); err == nil {
		return Artifact{Path: dst, SHA256: h, Size: int64(len(b))}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Artifact{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return Artifact{}, err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return Artifact{}, fmt.Errorf("writing artifact: %v %v", werr, cerr)
	}
	_ = os.Chmod(tmp.Name(), 0o444)
	if err := fsutil.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		if _, serr := os.Stat(dst); serr != nil { // a parallel build of the same content is not an error
			return Artifact{}, err
		}
	}
	return Artifact{Path: dst, SHA256: h, Size: int64(len(b))}, nil
}

func hint(diag string) string {
	switch {
	case bareHint.MatchString(diag) && !strings.Contains(bareHint.FindString(diag), ":"):
		m := bareHint.FindStringSubmatch(diag)
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if strings.HasPrefix(name, "@tsbox/") {
			return fmt.Sprintf("Module %q is not activated. Start with --sdk %s.", name, strings.TrimPrefix(name, "@tsbox/"))
		}
		return fmt.Sprintf("Module %q is unknown. Allowed are relative imports, @tsbox/core and activated SDKs.", name)
	case remoteHint.MatchString(diag):
		return "Remote, jsr: and npm: imports are not allowed in tsbox. Use local files or an SDK (--sdk)."
	case strings.Contains(diag, "TS"):
		return "Type error in the script (tsbox always builds with type checking)."
	}
	return ""
}

// coreHash identifies the embedded version of @tsbox/core.
func coreHash() string {
	h := sha256.New()
	_ = fs.WalkDir(js.Core, "core", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := js.Core.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return "core-" + hex.EncodeToString(h.Sum(nil)[:8])
}

var (
	coreMu  sync.Mutex // see writeCore
	storeMu sync.Mutex // see Store
)

// writeCore stores @tsbox/core once per core version. Parallel builds read the directory, so it is
// written to a sibling directory and renamed atomically, never overwritten (before: rewritten on every
// build, and a parallel build saw an empty file – found by the load test).
func writeCore(dir string) error {
	done := filepath.Join(dir, ".complete")
	if _, err := os.Stat(done); err == nil {
		return nil
	}
	// One writer per process: on Windows the rename of a second writer onto the finished directory fails
	// and fsutil.Rename would retry for about 4 s while holding a build slot.
	coreMu.Lock()
	defer coreMu.Unlock()
	if _, err := os.Stat(done); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".core-")
	if err != nil {
		return err
	}
	err = fs.WalkDir(js.Core, "core", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := js.Core.ReadFile(p)
		if err != nil {
			return err
		}
		// Readable for the build sandbox (UID 65534).
		return os.WriteFile(filepath.Join(tmp, filepath.Base(p)), b, 0o644)
	})
	if err == nil {
		err = os.WriteFile(filepath.Join(tmp, ".complete"), nil, 0o644)
	}
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	if err != nil {
		fsutil.RemoveAll(tmp)
		return err
	}
	if _, serr := os.Stat(dir); serr == nil {
		if _, derr := os.Stat(done); derr == nil {
			fsutil.RemoveAll(tmp) // a parallel build was faster
			return nil
		}
		_ = fsutil.RemoveAll(dir) // directory of an older tsbox version without marker
	}
	if err := fsutil.Rename(tmp, dir); err != nil {
		fsutil.RemoveAll(tmp)
		if _, derr := os.Stat(done); derr == nil {
			return nil
		}
		return err
	}
	return nil
}

// FileURL is the escaped file: URL of an absolute path (also for Windows paths).
func FileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/… → /C:/…
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// importMap is the import map for builds on the host: @tsbox/core from coreDir and the SDK modules.
func importMap(o Options, coreDir string) ([]byte, error) {
	imports := map[string]string{
		"@tsbox/core":      FileURL(filepath.Join(coreDir, "mod.ts")),
		"@tsbox/core/boot": FileURL(filepath.Join(coreDir, "boot.ts")),
	}
	for name, p := range o.Imports {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		imports[name] = FileURL(abs)
	}
	return json.Marshal(map[string]any{"imports": imports})
}
