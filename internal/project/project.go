// Package project reads the project file tsbox.json (boxes, scripts, SDKs, grants, limits; similar to
// docker-compose.yml) and translates it into box specifications.
package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/manifest"
)

// FileName is the default name of the project file.
const FileName = "tsbox.json"

// File is the content of tsbox.json.
type File struct {
	Name  string         `json:"name,omitempty"` // default: name of the directory
	Boxes map[string]Box `json:"boxes"`
}

// Box describes a box of the project.
type Box struct {
	Limits  *Limits           `json:"limits,omitempty"`
	Restart string            `json:"restart,omitempty"` // no | on-failure[:n] | always
	Storage string            `json:"storage,omitempty"`
	Profile string            `json:"profile,omitempty"` // rootfs under gVisor (default: default)
	Driver  string            `json:"driver,omitempty"`  // gvisor | firecracker | dev (required)
	Labels  map[string]string `json:"labels,omitempty"`
	SDKs    map[string]SDK    `json:"sdks,omitempty"`
	Scripts map[string]Script `json:"scripts"`
}

// Limits of the box.
type Limits struct {
	Memory   Size    `json:"memory,omitempty"`
	CPU      float64 `json:"cpu,omitempty"`
	StartCPU float64 `json:"startCpu,omitempty"` // CPU boost at start (opt-in)
	PIDs     int     `json:"pids,omitempty"`
}

// SDK activates an SDK: Name (bundled/installed) or Path to an SDK directory (relative to the file).
type SDK struct {
	Path   string              `json:"path,omitempty"`
	Grants map[string][]string `json:"grants,omitempty"`
	Config map[string]string   `json:"config,omitempty"` // secrets only as file:<path> or env:<NAME>
}

// Script of a box.
type Script struct {
	Entry   string          `json:"entry"`
	Params  json.RawMessage `json:"params,omitempty"`
	Restart string          `json:"restart,omitempty"`
	Limits  *ScriptLimits   `json:"limits,omitempty"`
}

// ScriptLimits of a script. Watchdog "0" turns it off.
type ScriptLimits struct {
	Heap     Size     `json:"heap,omitempty"`
	RSS      Size     `json:"rss,omitempty"`
	Threads  int      `json:"threads,omitempty"`
	Timeout  Duration `json:"timeout,omitempty"`
	Watchdog Duration `json:"watchdog,omitempty"`
}

// Size is a size in bytes, as a number or text ("128Mi", "1Gi").
type Size int64

func (s *Size) UnmarshalJSON(b []byte) error {
	var n int64
	if json.Unmarshal(b, &n) == nil {
		*s = Size(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return errors.New("size expected as number or text (e.g. \"128Mi\")")
	}
	v, err := ParseBytes(str)
	*s = Size(v)
	return err
}

// Duration is a duration as text ("30s", "5m", "0").
type Duration struct {
	time.Duration
	Set bool
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return errors.New("duration expected as text (e.g. \"30s\")")
	}
	if str == "0" {
		*d = Duration{Set: true}
		return nil
	}
	v, err := time.ParseDuration(str)
	if err != nil {
		return fmt.Errorf("invalid duration %q (a number with a unit, e.g. 500ms, 30s, 5m)", str)
	}
	*d = Duration{Duration: v, Set: true}
	return nil
}

// ParseBytes understands 128Mi, 1Gi, 512Ki, 100M, 1G and plain byte counts.
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	for _, u := range []string{"KB", "MB", "GB"} {
		if strings.HasSuffix(t, u) { // decimal, like K, M, G
			t = strings.TrimSuffix(t, "B")
			break
		}
	}
	v, err := manifest.ParseBytes(t)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q (examples: 128Mi, 1Gi)", s)
	}
	return v, nil
}

// ParseRestart understands no | on-failure[:max] | always.
func ParseRestart(s string) (tsbox.RestartPolicy, error) {
	mode, maxStr, _ := strings.Cut(s, ":")
	p := tsbox.RestartPolicy{Mode: tsbox.RestartMode(mode)}
	switch p.Mode {
	case tsbox.RestartNo, tsbox.RestartAlways, tsbox.RestartOnFailure:
	default:
		return p, fmt.Errorf("restart policy %q unknown (no, on-failure[:max], always)", s)
	}
	if maxStr != "" {
		n, err := strconv.Atoi(maxStr)
		if err != nil || n < 1 {
			return p, fmt.Errorf("invalid maximum %q", maxStr)
		}
		p.MaxFailures = n
	}
	return p, nil
}

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// Project is a loaded project file.
type Project struct {
	Name  string
	Dir   string
	Boxes []Resolved // sorted by key
}

// Resolved is a box with a finished specification (name "<project>-<box>", absolute paths).
type Resolved struct {
	Key  string
	Spec tsbox.BoxSpec
	Hash string // changes with the specification or the sources
}

// Load reads and validates a project file strictly (unknown fields are errors).
func Load(path string) (*Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no project file %s (create it or name another one with -f <file>)", abs)
	}
	if err != nil {
		return nil, err
	}
	if raw, err = fsutil.DecodeText(raw); err != nil {
		return nil, fmt.Errorf("%s is %w", path, err)
	}
	p, err := Parse(raw, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse validates the content of a project file; relative paths are relative to dir.
func Parse(raw []byte, dir string) (*Project, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, jsonErr(raw, dec.InputOffset(), err)
	}
	if dec.More() {
		return nil, errors.New("data after the JSON object")
	}
	name := f.Name
	if name == "" {
		name = sanitize(filepath.Base(dir))
	}
	if !keyPattern.MatchString(name) {
		return nil, fmt.Errorf("project name %q: allowed are a-z, 0-9, - (1-31 characters)", name)
	}
	if len(f.Boxes) == 0 {
		return nil, errors.New("no boxes (\"boxes\")")
	}
	p := &Project{Name: name, Dir: dir}
	keys := make([]string, 0, len(f.Boxes))
	for k := range f.Boxes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !keyPattern.MatchString(k) {
			return nil, fmt.Errorf("box %q: allowed are a-z, 0-9, - (1-31 characters)", k)
		}
		spec, err := f.Boxes[k].spec(name+"-"+k, dir)
		if err != nil {
			return nil, fmt.Errorf("box %q: %w", k, err)
		}
		h, err := hashSpec(spec)
		if err != nil {
			return nil, err
		}
		p.Boxes = append(p.Boxes, Resolved{Key: k, Spec: spec, Hash: h})
	}
	return p, nil
}

func (b Box) spec(name, dir string) (tsbox.BoxSpec, error) {
	s := tsbox.BoxSpec{Name: name, Storage: b.Storage, Profile: b.Profile, Driver: b.Driver, Labels: map[string]string{}}
	switch b.Driver {
	case "gvisor", "firecracker", "dev":
	case "":
		return s, errors.New("\"driver\" is missing: gvisor, firecracker or dev")
	default:
		return s, fmt.Errorf("driver %q: allowed are gvisor, firecracker and dev", b.Driver)
	}
	for k, v := range b.Labels {
		if strings.HasPrefix(k, "tsbox.") {
			return s, fmt.Errorf("label %q: the prefix tsbox. is reserved", k)
		}
		s.Labels[k] = v
	}
	if b.Limits != nil {
		s.Limits = tsbox.Limits{Memory: int64(b.Limits.Memory), CPU: b.Limits.CPU, PIDs: b.Limits.PIDs, StartCPU: b.Limits.StartCPU}
	}
	if b.Restart != "" {
		p, err := ParseRestart(b.Restart)
		if err != nil {
			return s, err
		}
		s.Restart = p
	}
	sdkNames := make([]string, 0, len(b.SDKs))
	for n := range b.SDKs {
		sdkNames = append(sdkNames, n)
	}
	sort.Strings(sdkNames)
	for _, n := range sdkNames {
		u := b.SDKs[n]
		ref := n
		if u.Path != "" {
			ref = abs(dir, u.Path)
		}
		cfg := map[string]string{}
		for k, v := range u.Config {
			if p, ok := strings.CutPrefix(v, "file:"); ok {
				v = "file:" + abs(dir, p)
			}
			cfg[k] = v
		}
		s.SDKs = append(s.SDKs, tsbox.SDKUse{Name: ref, Grants: u.Grants, Config: cfg})
	}
	if len(b.Scripts) == 0 {
		return s, errors.New("no scripts (\"scripts\")")
	}
	names := make([]string, 0, len(b.Scripts))
	for n := range b.Scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sc := b.Scripts[n]
		if sc.Entry == "" {
			return s, fmt.Errorf("script %q: entry missing", n)
		}
		if len(sc.Params) > 0 && !json.Valid(sc.Params) {
			return s, fmt.Errorf("script %q: params is not JSON", n)
		}
		ss := tsbox.ScriptSpec{Name: n, Entry: abs(dir, sc.Entry), Params: sc.Params}
		if sc.Restart != "" {
			p, err := ParseRestart(sc.Restart)
			if err != nil {
				return s, fmt.Errorf("script %q: %w", n, err)
			}
			ss.Restart = &p
		}
		if l := sc.Limits; l != nil {
			ss.Limits = tsbox.ScriptLimits{Heap: int64(l.Heap), RSS: int64(l.RSS), Threads: l.Threads, Timeout: l.Timeout.Duration}
			if l.Watchdog.Set {
				ss.Limits.Watchdog = l.Watchdog.Duration
				if l.Watchdog.Duration == 0 {
					ss.Limits.Watchdog = -1
				}
			}
		}
		if st, err := os.Stat(ss.Entry); err != nil {
			return s, fmt.Errorf("script %q: entry %s not found", n, sc.Entry)
		} else if st.IsDir() {
			return s, fmt.Errorf("script %q: entry %s is a directory, not a script", n, sc.Entry)
		}
		s.Scripts = append(s.Scripts, ss)
	}
	return s, nil
}

func abs(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, filepath.FromSlash(p))
}

var unsafe = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitize(s string) string {
	s = strings.Trim(unsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 31 {
		s = strings.Trim(s[:31], "-")
	}
	if s == "" {
		s = "projekt"
	}
	return s
}

// hashSpec combines the specification and the sources: if either changes, "up" recreates the box.
// It reads .ts/.tsx/.js/.mjs/.json files (≤ 1 MiB, at most 5000) under the directory of every entry and
// of every local SDK.
func hashSpec(s tsbox.BoxSpec) (string, error) {
	h := sha256.New()
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	h.Write(b)
	dirs := map[string]bool{}
	for _, sc := range s.Scripts {
		dirs[filepath.Dir(sc.Entry)] = true
	}
	for _, u := range s.SDKs {
		if filepath.IsAbs(u.Name) {
			dirs[u.Name] = true
		}
	}
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	sort.Strings(list)
	n := 0
	for _, d := range list {
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() {
				switch e.Name() {
				case ".git", "node_modules", ".tsbox":
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(p) {
			case ".ts", ".tsx", ".js", ".mjs", ".json":
			default:
				return nil
			}
			if n++; n > 5000 {
				return filepath.SkipAll
			}
			// os.Stat follows symlinks; FIFOs and devices are skipped (reading them can block).
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			fmt.Fprintf(h, "\x00%s\x00%d\x00", p, len(data))
			h.Write(data)
			return nil
		})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// jsonErr makes the errors of encoding/json readable: position as line and column, without "json:".
func jsonErr(raw []byte, off int64, err error) error {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		off = se.Offset
	case errors.As(err, &te):
		off = te.Offset
		where := te.Field
		if where == "" {
			where = "value"
		}
		want := te.Type.Kind().String()
		switch te.Type.Kind() {
		case reflect.Float32, reflect.Float64, reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64:
			want = "a number"
		case reflect.String:
			want = "a string"
		case reflect.Bool:
			want = "true or false"
		case reflect.Map, reflect.Struct:
			want = "an object"
		case reflect.Slice:
			want = "a list"
		}
		return fmt.Errorf("%s: %s expected, got %s (%s)", where, want, te.Value, position(raw, off))
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return errors.New("invalid JSON: the file ends too early (missing } or \"?)")
	}
	msg := strings.TrimPrefix(err.Error(), "json: ")
	if se != nil {
		msg = "invalid JSON: " + strings.Replace(msg, "invalid character", "unexpected character", 1)
		if strings.Contains(msg, "looking for beginning of object key") {
			msg += " (a comma before } or ]?)"
		}
	}
	return fmt.Errorf("%s (%s)", msg, position(raw, off))
}

// position converts a byte offset into "line L, column C".
func position(raw []byte, off int64) string {
	off = min(max(off, 0), int64(len(raw)))
	line, col := 1, 1
	for _, c := range raw[:off] {
		if c == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return fmt.Sprintf("line %d, column %d", line, col)
}
