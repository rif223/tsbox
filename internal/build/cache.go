package build

import (
	"context"
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
	"sort"
	"strings"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
)

// Build cache by input hash: a script is only rebuilt when one of its inputs has changed.
//
// The key covers the cache version, @tsbox/core, the toolchain (mode and Deno version), the import map
// (names and content of the SDK module directories), the entry relative to the context and the content of
// all files in the context (the script's directory). In the build sandbox this is complete: it sees only
// these directories. On the host (dev) a script can also import files outside. Before the build, "deno
// info" therefore determines the whole module graph (including pure type imports); every file outside the
// hashed directories is stored with its SHA-256 in the entry and checked on lookup. Hashing happens before
// the build: if a file changes meanwhile, the hash does not match next time and the script is rebuilt.
//
// Entry: <StoreDir>/cache/<xx>/<key>, first line the artifact hash, then "<sha256> <path>" for each
// additional input. A hit counts only if the artifact is still in the store and all additional inputs are
// unchanged.

const cacheVersion = "tsbox-build-cache-4" // 4: inline source maps (stack traces with file and line)

// Limits for the context: larger directories are not hashed (and not cached).
var (
	maxCacheFiles       = 5000
	maxCacheBytes int64 = 64 << 20
)

// errTooBig: context above the limits, no cache.
var errTooBig = errors.New("build context too large for the cache")

// hashTree writes the paths (relative, with /) and contents of all regular files under dir into h. Symlinks
// count with their target (deno follows them). FIFOs, sockets and devices are skipped: opening or reading
// them can block forever (a FIFO without a writer, /dev/tty) or never end (/dev/zero).
func hashTree(h io.Writer, dir string, files *int, size *int64) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			fmt.Fprintf(h, "d\x00%s\x00", rel)
			return nil
		}
		*files++
		if *files > maxCacheFiles {
			return errTooBig
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			fmt.Fprintf(h, "l\x00%s\x00%s\x00", rel, t)
			if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
				return nil // WalkDir does not follow directory links; deno only follows file links
			}
		case !d.Type().IsRegular():
			fmt.Fprintf(h, "s\x00%s\x00%s\x00", rel, d.Type())
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		fh := sha256.New()
		n, err := io.Copy(fh, f)
		if err != nil {
			return err
		}
		if *size += n; *size > maxCacheBytes {
			return errTooBig
		}
		fmt.Fprintf(h, "f\x00%s\x00%x\x00", rel, fh.Sum(nil))
		return nil
	})
}

// cacheKey computes the key; roots are the hashed directories (for the check in host mode).
func cacheKey(o Options, ctxDir, src string) (key string, roots []string, err error) {
	h := sha256.New()
	rel, err := filepath.Rel(ctxDir, src)
	if err != nil {
		return "", nil, err
	}
	mode := "host"
	if o.Runner != nil {
		mode = "sandbox"
	}
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00", cacheVersion, coreHash(), mode, o.Toolchain, filepath.ToSlash(rel))
	files, size := 0, int64(0)
	if err := hashTree(h, ctxDir, &files, &size); err != nil {
		return "", nil, err
	}
	roots = []string{ctxDir}
	names := make([]string, 0, len(o.Imports))
	for name := range o.Imports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mod, err := filepath.Abs(o.Imports[name])
		if err != nil {
			return "", nil, err
		}
		fmt.Fprintf(h, "import\x00%s\x00%s\x00", name, filepath.Base(mod))
		if err := hashTree(h, filepath.Dir(mod), &files, &size); err != nil {
			return "", nil, err
		}
		roots = append(roots, filepath.Dir(mod))
	}
	return hex.EncodeToString(h.Sum(nil)), roots, nil
}

func cachePath(storeDir, key string) string { return filepath.Join(storeDir, "cache", key[:2], key) }

// input is an additional input (file outside the hashed directories) with its hash.
type input struct{ sum, path string }

func fileSum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cacheGet returns the artifact of a key if it exists and all inputs are unchanged.
func cacheGet(storeDir, key string) (Artifact, bool) {
	b, err := os.ReadFile(cachePath(storeDir, key))
	if err != nil {
		return Artifact{}, false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	sum := strings.TrimSpace(lines[0])
	if len(sum) != 64 {
		return Artifact{}, false
	}
	for _, l := range lines[1:] {
		want, p, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok {
			return Artifact{}, false
		}
		if got, err := fileSum(p); err != nil || got != want {
			return Artifact{}, false
		}
	}
	p := filepath.Join(storeDir, "sha256", sum+".js")
	touch(cachePath(storeDir, key))
	touch(p)
	data, err := os.ReadFile(p)
	if err != nil {
		return Artifact{}, false
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != sum {
		return Artifact{}, false // tampered with: rebuild (init checks again before the start anyway)
	}
	return Artifact{Path: p, SHA256: sum, Size: int64(len(data))}, true
}

func cachePut(storeDir, key, sum string, extra []input) error {
	p := cachePath(storeDir, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(sum + "\n")
	for _, in := range extra {
		fmt.Fprintf(&sb, "%s %s\n", in.sum, in.path)
	}
	_, werr := tmp.WriteString(sb.String())
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := fsutil.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// hostGraph uses "deno info" to determine all local modules of src (code and type imports) and returns
// those outside roots with their hash. ok = false if the graph cannot be resolved completely (then no
// cache).
func hostGraph(ctx context.Context, o Options, src, coreDir string, roots []string) ([]input, bool) {
	tmp, err := os.MkdirTemp("", "tsbox-graph-")
	if err != nil {
		return nil, false
	}
	defer fsutil.RemoveAll(tmp)
	cfg, err := importMap(o, coreDir)
	if err != nil {
		return nil, false
	}
	cfgPath := filepath.Join(tmp, "import_map.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.Deno, "info", "--json", "--no-config", "--import-map", cfgPath, "--no-lock",
		"--no-remote", "--no-npm", fileURL(src)) // as a URL: deno reads a Windows path C:\… as scheme "c:"
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, false
	}
	var graph struct {
		Modules []struct {
			Local string `json:"local"`
			Error string `json:"error"`
		} `json:"modules"`
	}
	if json.Unmarshal(out, &graph) != nil || len(graph.Modules) == 0 {
		return nil, false
	}
	var extra []input
	seen := map[string]bool{}
	for _, m := range graph.Modules {
		if m.Error != "" || m.Local == "" {
			return nil, false
		}
		p := filepath.Clean(m.Local)
		if seen[p] || within(p, roots) {
			continue
		}
		seen[p] = true
		sum, err := fileSum(p)
		if err != nil {
			return nil, false
		}
		extra = append(extra, input{sum: sum, path: p})
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].path < extra[j].path })
	return extra, true
}

func within(p string, roots []string) bool {
	for _, r := range roots {
		if rel, err := filepath.Rel(r, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
