// Package sdkreg finds and manages SDKs: bundled (embedded), installed (<state>/sdks/installed/<name>,
// recorded in lock.json with a tree hash) and local directories. An installed SDK whose files changed since
// the installation is rejected.
package sdkreg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/manifest"
	"github.com/rif223/tsbox/sdks"
)

// SDK is a found SDK.
type SDK struct {
	Manifest *manifest.Manifest
	Dir      string // on the host
	Source   string // builtin | path | git URL
	Hash     string // tree hash (sha256)
}

// ModulePath is the path of the TS module.
func (s *SDK) ModulePath() string { return filepath.Join(s.Dir, s.Manifest.Module) }

// LockEntry records an installed SDK.
type LockEntry struct {
	Version   string    `json:"version"`
	Source    string    `json:"source"`
	Hash      string    `json:"hash"`
	Installed time.Time `json:"installed"`
}

// Registry is the SDK inventory of a state directory.
type Registry struct {
	root  string   // <state>/sdks
	extra []string // further search directories (<dir>/<name>/ each)
	mu    sync.Mutex
}

// New opens the registry under <state>/sdks.
func New(stateDir string, extra ...string) *Registry {
	return &Registry{root: filepath.Join(stateDir, "sdks"), extra: extra}
}

func (r *Registry) lockPath() string { return filepath.Join(r.root, "lock.json") }

func (r *Registry) readLock() (map[string]LockEntry, error) {
	lock := map[string]LockEntry{}
	b, err := os.ReadFile(r.lockPath())
	if errors.Is(err, os.ErrNotExist) {
		return lock, nil
	}
	if err != nil {
		return nil, err
	}
	return lock, json.Unmarshal(b, &lock)
}

func (r *Registry) writeLock(lock map[string]LockEntry) error {
	b, _ := json.MarshalIndent(lock, "", "  ")
	if err := os.MkdirAll(r.root, 0o755); err != nil {
		return err
	}
	tmp := r.lockPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return fsutil.Rename(tmp, r.lockPath())
}

// IsPath recognizes references to a local directory (instead of a name).
func IsPath(ref string) bool {
	return strings.ContainsAny(ref, `/\`) || strings.HasPrefix(ref, ".")
}

// Resolve finds an SDK by name or path. Order for names: installed, further directories,
// bundled.
func (r *Registry) Resolve(ref string) (*SDK, error) {
	if IsPath(ref) {
		return load(ref, ref)
	}
	lock, err := r.readLock()
	if err != nil {
		return nil, err
	}
	if e, ok := lock[ref]; ok {
		s, err := load(filepath.Join(r.root, "installed", ref), e.Source)
		if err != nil {
			return nil, err
		}
		if s.Hash != e.Hash {
			return nil, fmt.Errorf("SDK %q was changed after the installation (hash %s != %s). Reinstall: tsbox sdk add %s",
				ref, short(s.Hash), short(e.Hash), e.Source)
		}
		return s, nil
	}
	for _, dir := range r.extra {
		p := filepath.Join(dir, ref)
		if _, err := os.Stat(filepath.Join(p, manifest.FileName)); err == nil {
			return load(p, p)
		}
	}
	for _, name := range sdks.Names {
		if name == ref {
			dir, err := r.builtinDir(name)
			if err != nil {
				return nil, err
			}
			return load(dir, "builtin")
		}
	}
	return nil, fmt.Errorf("SDK %q not found. Bundled: %s; install with: tsbox sdk add <path|git-url>",
		ref, strings.Join(sdks.Names, ", "))
}

// builtinDir stores a bundled SDK content-addressed on disk (for the build and the editor).
func (r *Registry) builtinDir(name string) (string, error) {
	sub, err := fs.Sub(sdks.FS, name)
	if err != nil {
		return "", err
	}
	h, err := hashFS(sub)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(r.root, "builtin", name+"-"+short(h))
	if _, err := os.Stat(filepath.Join(dir, manifest.FileName)); err == nil {
		return dir, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := copyFS(sub, tmp); err != nil {
		return "", err
	}
	if err := fsutil.Rename(tmp, dir); err != nil {
		if _, serr := os.Stat(dir); serr == nil {
			_ = os.RemoveAll(tmp)
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

func load(dir, source string) (*SDK, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Load(abs)
	if err != nil {
		return nil, err
	}
	h, err := hashFS(os.DirFS(abs))
	if err != nil {
		return nil, err
	}
	return &SDK{Manifest: m, Dir: abs, Source: source, Hash: h}, nil
}

// Add installs an SDK from a directory or a Git URL ("git+https://…#ref").
func (r *Registry) Add(src string) (*SDK, error) {
	dir := src
	if strings.HasPrefix(src, "git+") {
		tmp, err := os.MkdirTemp("", "tsbox-sdk-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		url, ref, _ := strings.Cut(strings.TrimPrefix(src, "git+"), "#")
		args := []string{"clone", "--quiet", "--depth", "1"}
		if ref != "" {
			args = append(args, "--branch", ref)
		}
		cmd := exec.Command("git", append(args, url, tmp)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			if i := strings.LastIndex(msg, "\n"); i >= 0 {
				msg = strings.TrimSpace(msg[i+1:]) // git ends with the actual reason ("fatal: ...")
			}
			if msg == "" {
				msg = err.Error()
			}
			return nil, fmt.Errorf("git clone %s failed: %s", url, msg)
		}
		_ = os.RemoveAll(filepath.Join(tmp, ".git"))
		dir = tmp
	} else {
		if slices.Contains(sdks.Names, src) {
			return nil, fmt.Errorf("SDK %q is bundled with tsbox; there is nothing to install (use --sdk %s)", src, src)
		}
		if abs, err := filepath.Abs(src); err == nil {
			src = abs
		}
	}
	s, err := load(dir, src)
	if err != nil {
		return nil, err
	}
	for _, b := range sdks.Names {
		if b == s.Manifest.Name {
			return nil, fmt.Errorf("SDK name %q is bundled and reserved", b)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inst := filepath.Join(r.root, "installed")
	r.cleanup(inst)
	dst := filepath.Join(inst, s.Manifest.Name)
	// Copy next to the target first, then swap: an old version stays complete until the new one is ready,
	// and a directory that cannot be replaced (files in use) leaves everything as it was.
	if err := os.MkdirAll(inst, 0o755); err != nil {
		return nil, err
	}
	tmpDst, err := os.MkdirTemp(inst, ".new-"+s.Manifest.Name+"-")
	if err != nil {
		return nil, err
	}
	// MkdirTemp creates 0700; the build sandbox (UID 65534) must be able to read the SDK.
	if err := os.Chmod(tmpDst, 0o755); err != nil {
		_ = os.RemoveAll(tmpDst)
		return nil, err
	}
	if err := copyFS(os.DirFS(dir), tmpDst); err != nil {
		_ = os.RemoveAll(tmpDst)
		return nil, err
	}
	old := ""
	if _, err := os.Stat(dst); err == nil {
		old = tmpName(inst, ".old-"+s.Manifest.Name)
		if err := rename(dst, old); err != nil {
			_ = os.RemoveAll(tmpDst)
			return nil, fmt.Errorf("replace SDK %q: %w (is it still in use?)", s.Manifest.Name, err)
		}
	}
	if err := rename(tmpDst, dst); err != nil {
		if old != "" {
			_ = rename(old, dst)
		}
		_ = os.RemoveAll(tmpDst)
		return nil, err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	lock, err := r.readLock()
	if err != nil {
		return nil, err
	}
	lock[s.Manifest.Name] = LockEntry{Version: s.Manifest.Version, Source: src, Hash: s.Hash, Installed: time.Now().UTC()}
	if err := r.writeLock(lock); err != nil {
		return nil, err
	}
	s.Dir, s.Source = dst, src
	return s, nil
}

// Remove uninstalls an SDK.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, err := r.readLock()
	if err != nil {
		return err
	}
	if _, ok := lock[name]; !ok {
		if slices.Contains(sdks.Names, name) {
			return fmt.Errorf("SDK %q is bundled with tsbox and cannot be removed", name)
		}
		return fmt.Errorf("SDK %q is not installed", name)
	}
	// Move the directory away first: if that fails (files in use), lock.json stays unchanged.
	inst := filepath.Join(r.root, "installed")
	r.cleanup(inst)
	dir := filepath.Join(inst, name)
	gone := tmpName(inst, ".rm-"+name)
	if err := rename(dir, gone); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove SDK %q: %w (is it still in use?)", name, err)
	}
	delete(lock, name)
	if err := r.writeLock(lock); err != nil {
		_ = rename(gone, dir)
		return err
	}
	_ = os.RemoveAll(gone)
	return nil
}

// InstalledDir is the directory of an installed SDK.
func (r *Registry) InstalledDir(name string) string { return filepath.Join(r.root, "installed", name) }

// cleanup removes leftovers of interrupted installs and removals (.new-*, .old-*, .rm-*).
func (r *Registry) cleanup(inst string) {
	ents, _ := os.ReadDir(inst)
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, ".new-") || strings.HasPrefix(n, ".old-") || strings.HasPrefix(n, ".rm-") {
			_ = os.RemoveAll(filepath.Join(inst, n))
		}
	}
}

// tmpName is an unused name in dir with the given prefix.
func tmpName(dir, prefix string) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()))
}

// List returns all known SDKs (bundled and installed), sorted.
func (r *Registry) List() ([]*SDK, error) {
	var out []*SDK
	for _, n := range sdks.Names {
		s, err := r.Resolve(n)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	lock, err := r.readLock()
	if err != nil {
		return nil, err
	}
	for n := range lock {
		s, err := r.Resolve(n)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out, nil
}

// hashFS builds the tree hash: sha256 over (path, content) of all files in sorted order.
// node_modules, .git and .tsbox do not count.
func hashFS(fsys fs.FS) (string, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".tsbox") {
			return fs.SkipDir
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, p := range files {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFS(fsys fs.FS, dst string) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // no symlinks and the like
		}
		src, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, src)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	})
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// rename retries for a while on Windows: virus scanners briefly keep freshly written files open, and then a
// directory cannot be renamed ("Access is denied"). A file that stays in use fails after about 3 s.
func rename(from, to string) error {
	err := os.Rename(from, to)
	if runtime.GOOS != "windows" {
		return err
	}
	for wait := 20 * time.Millisecond; err != nil && !errors.Is(err, os.ErrNotExist) && wait < 2*time.Second; wait *= 2 {
		time.Sleep(wait)
		err = os.Rename(from, to)
	}
	return err
}
