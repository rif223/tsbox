// Package manifest reads and validates an SDK's manifest (tsbox-sdk.json) and the rights (grants) an
// operator grants at start (--grant http:host=api.github.com).
//
// A manifest describes: name, version, TS module and import name, provider (built-in or external
// process), operator configuration (including secrets), rights for the script, methods with limits
// (rate, concurrency, size, time) and events.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the SDK interface of this tsbox. Manifests name it in "tsbox" ("^0.1" or "0.1").
const APIVersion = "0.1"

// FileName is the file name of the manifest in an SDK directory.
const FileName = "tsbox-sdk.json"

// Upper limits of the core. A manifest may stay below them, not above.
const (
	MaxParamBytes  = 1<<20 - 4096 // fits into a frame
	MaxResultBytes = 1<<20 - 4096
	MaxTimeout     = 5 * time.Minute
	MaxConcurrency = 64
)

// Defaults for methods without their own settings.
var defaultMethod = Method{
	Rate: 50, Burst: 100, Concurrency: 8, MaxParams: 64 << 10, MaxResult: 1 << 20 >> 1, Timeout: 30 * time.Second,
}

var (
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyPattern    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,31}$`)
	importPattern = regexp.MustCompile(`^@[a-z0-9-]+/[a-z0-9][a-z0-9._-]*$`)
	semver        = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

// Manifest is the content of tsbox-sdk.json.
type Manifest struct {
	Name        string                `json:"name"`
	Version     string                `json:"version"`
	TSBox       string                `json:"tsbox"`
	Description string                `json:"description,omitempty"`
	Import      string                `json:"import,omitempty"` // default @tsbox/<name>
	Module      string                `json:"module,omitempty"` // default mod.ts
	Requires    []string              `json:"requires,omitempty"`
	Provider    Provider              `json:"provider"`
	Config      map[string]ConfigItem `json:"config,omitempty"`
	Permissions map[string]Permission `json:"permissions,omitempty"`
	Methods     map[string]*Method    `json:"methods"`
	Events      map[string]Event      `json:"events,omitempty"`
}

// Provider: either Builtin (Go, registered in the engine) or Exec (external process, frames over stdio).
type Provider struct {
	Builtin string   `json:"builtin,omitempty"`
	Exec    []string `json:"exec,omitempty"` // relative to the SDK directory; "deno" = the host's Deno
}

// ConfigItem is an operator setting (never visible to the script).
type ConfigItem struct {
	Type        string `json:"type"` // string, int, bool, bytes, duration, secret
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// Permission is a right the script only gets with an explicit grant.
type Permission struct {
	Type        string   `json:"type"` // string, host-pattern, int, bytes, enum
	Values      []string `json:"values,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
}

// Method is a method with its limits. In JSON, sizes are written as "64Ki", rates as "10/s",
// durations as "10s".
type Method struct {
	Permission  string        `json:"-"`
	Rate        float64       `json:"-"` // per second and script
	Burst       int           `json:"-"`
	Concurrency int           `json:"-"`
	MaxParams   int64         `json:"-"`
	MaxResult   int64         `json:"-"`
	Timeout     time.Duration `json:"-"`
	Description string        `json:"-"`
}

type rawMethod struct {
	Permission  string `json:"permission,omitempty"`
	Rate        string `json:"rate,omitempty"`
	Burst       int    `json:"burst,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
	MaxParams   string `json:"maxParams,omitempty"`
	MaxResult   string `json:"maxResult,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Description string `json:"description,omitempty"`
}

func (m *Method) UnmarshalJSON(b []byte) error {
	var r rawMethod
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return err
	}
	*m = defaultMethod
	m.Permission, m.Description = r.Permission, r.Description
	var err error
	if r.Rate != "" {
		if m.Rate, err = ParseRate(r.Rate); err != nil {
			return err
		}
	}
	if r.Burst != 0 {
		m.Burst = r.Burst
	}
	if r.Concurrency != 0 {
		m.Concurrency = r.Concurrency
	}
	if r.MaxParams != "" {
		if m.MaxParams, err = ParseBytes(r.MaxParams); err != nil {
			return err
		}
	}
	if r.MaxResult != "" {
		if m.MaxResult, err = ParseBytes(r.MaxResult); err != nil {
			return err
		}
	}
	if r.Timeout != "" {
		if m.Timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return err
		}
	}
	return nil
}

func (m Method) MarshalJSON() ([]byte, error) {
	return json.Marshal(rawMethod{
		Permission: m.Permission, Rate: strconv.FormatFloat(m.Rate, 'f', -1, 64) + "/s", Burst: m.Burst,
		Concurrency: m.Concurrency, MaxParams: FormatBytes(m.MaxParams), MaxResult: FormatBytes(m.MaxResult),
		Timeout: m.Timeout.String(), Description: m.Description,
	})
}

// Event is an event the provider may send to subscribed scripts.
type Event struct {
	Description string `json:"description,omitempty"`
}

// Load reads the manifest from an SDK directory and validates it.
func Load(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no SDK in %s (%s missing)", dir, FileName)
	}
	if err != nil {
		return nil, fmt.Errorf("SDK %s: %w", dir, err)
	}
	m, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("SDK %s: %w", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, m.Module)); err != nil {
		return nil, fmt.Errorf("SDK %s: module %s missing", m.Name, m.Module)
	}
	return m, nil
}

// Parse decodes and validates a manifest (strict: unknown fields are errors).
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if err := m.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &m, nil
}

func (m *Manifest) normalize() error {
	switch {
	case !namePattern.MatchString(m.Name):
		return fmt.Errorf("name %q invalid (a-z, 0-9, -, at most 32 characters)", m.Name)
	case !semver.MatchString(m.Version):
		return fmt.Errorf("version %q is not a SemVer version", m.Version)
	case strings.TrimPrefix(m.TSBox, "^") != APIVersion:
		return fmt.Errorf("tsbox %q does not match this tsbox version (expected ^%s)", m.TSBox, APIVersion)
	case (m.Provider.Builtin == "") == (len(m.Provider.Exec) == 0):
		return errors.New("provider needs exactly one of builtin or exec")
	case len(m.Methods) == 0:
		return errors.New("methods is empty")
	}
	if m.Import == "" {
		m.Import = "@tsbox/" + m.Name
	}
	if !importPattern.MatchString(m.Import) || m.Import == "@tsbox/core" || strings.HasPrefix(m.Import, "@tsbox/core/") {
		return fmt.Errorf("import %q invalid", m.Import)
	}
	if m.Module == "" {
		m.Module = "mod.ts"
	}
	if filepath.IsAbs(m.Module) || strings.Contains(filepath.ToSlash(m.Module), "..") {
		return fmt.Errorf("module %q must be a relative path inside the SDK", m.Module)
	}
	for k, c := range m.Config {
		if !keyPattern.MatchString(k) {
			return fmt.Errorf("config %q: invalid name", k)
		}
		switch c.Type {
		case "string", "int", "bool", "bytes", "duration":
			if c.Default != "" {
				if err := checkValue(c.Type, nil, c.Default); err != nil {
					return fmt.Errorf("config %q: default: %w", k, err)
				}
			}
		case "secret":
			if c.Default != "" {
				return fmt.Errorf("config %q: secrets have no default value", k)
			}
		default:
			return fmt.Errorf("config %q: unknown type %q", k, c.Type)
		}
	}
	for k, p := range m.Permissions {
		if !keyPattern.MatchString(k) {
			return fmt.Errorf("permission %q: invalid name", k)
		}
		switch p.Type {
		case "string", "host-pattern", "int", "bytes":
		case "enum":
			if len(p.Values) == 0 {
				return fmt.Errorf("permission %q: enum without values", k)
			}
		default:
			return fmt.Errorf("permission %q: unknown type %q", k, p.Type)
		}
	}
	for name, meth := range m.Methods {
		if !keyPattern.MatchString(name) {
			return fmt.Errorf("method %q: invalid name", name)
		}
		if meth == nil {
			d := defaultMethod
			meth = &d
			m.Methods[name] = meth
		}
		if meth.Permission != "" {
			if _, ok := m.Permissions[meth.Permission]; !ok {
				return fmt.Errorf("method %q: permission %q not declared", name, meth.Permission)
			}
		}
		switch {
		case meth.Rate <= 0 || meth.Burst < 1:
			return fmt.Errorf("method %q: rate/burst must be positive", name)
		case meth.Concurrency < 1 || meth.Concurrency > MaxConcurrency:
			return fmt.Errorf("method %q: concurrency outside [1, %d]", name, MaxConcurrency)
		case meth.MaxParams < 2 || meth.MaxParams > MaxParamBytes:
			return fmt.Errorf("method %q: maxParams outside [2, %s]", name, FormatBytes(MaxParamBytes))
		case meth.MaxResult < 2 || meth.MaxResult > MaxResultBytes:
			return fmt.Errorf("method %q: maxResult outside [2, %s]", name, FormatBytes(MaxResultBytes))
		case meth.Timeout <= 0 || meth.Timeout > MaxTimeout:
			return fmt.Errorf("method %q: timeout outside (0, %v]", name, MaxTimeout)
		}
	}
	for name := range m.Events {
		if !keyPattern.MatchString(name) && !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`).MatchString(name) {
			return fmt.Errorf("event %q: invalid name", name)
		}
	}
	for _, r := range m.Requires {
		if !namePattern.MatchString(r) || r == m.Name {
			return fmt.Errorf("requires %q invalid", r)
		}
	}
	return nil
}

// Grants are granted rights: right → values.
type Grants map[string][]string

// ParseGrant splits "sdk:right=value".
func ParseGrant(s string) (sdk, perm, value string, err error) {
	head, value, ok := strings.Cut(s, "=")
	sdk, perm, ok2 := strings.Cut(head, ":")
	if !ok || !ok2 || sdk == "" || perm == "" || value == "" {
		return "", "", "", fmt.Errorf("grant %q: expected <sdk>:<right>=<value>, e.g. http:host=api.github.com", s)
	}
	return sdk, perm, value, nil
}

// CheckGrants checks grants against the manifest: right declared, value matches the type, required rights set.
func (m *Manifest) CheckGrants(g Grants) error {
	for perm, vals := range g {
		p, ok := m.Permissions[perm]
		if !ok {
			return fmt.Errorf("SDK %q has no right %q (known: %s)", m.Name, perm, keys(m.Permissions))
		}
		if len(vals) > 1 && !p.Multiple {
			return fmt.Errorf("SDK %q: right %q allows only one value", m.Name, perm)
		}
		for _, v := range vals {
			if err := checkValue(p.Type, p.Values, v); err != nil {
				return fmt.Errorf("SDK %q: %s=%s: %w", m.Name, perm, v, err)
			}
		}
	}
	for perm, p := range m.Permissions {
		if p.Required && len(g[perm]) == 0 {
			ex := "<value>"
			if p.Type == "host-pattern" {
				ex = "api.example.com"
			}
			return fmt.Errorf("SDK %q needs at least one grant %q: %s. Example: --grant %s:%s=%s",
				m.Name, perm, p.Description, m.Name, perm, ex)
		}
	}
	return nil
}

// ResolveConfig validates the operator configuration, sets defaults and resolves secrets
// (only "file:<path>" or "env:<NAME>"). The resolved values never leave the host.
func (m *Manifest) ResolveConfig(raw map[string]string) (map[string]string, error) {
	return m.ResolveConfigEnv(raw, os.LookupEnv)
}

// ResolveConfigEnv is ResolveConfig with its own source for "env:<NAME>". The daemon uses it to apply the
// environment of the calling CLI process, which it keeps in memory only.
func (m *Manifest) ResolveConfigEnv(raw map[string]string, lookup func(string) (string, bool)) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range raw {
		c, ok := m.Config[k]
		if !ok {
			return nil, fmt.Errorf("SDK %q has no setting %q (known: %s)", m.Name, k, keys(m.Config))
		}
		if c.Type == "secret" {
			s, err := resolveSecret(v, lookup)
			if err != nil {
				return nil, fmt.Errorf("SDK %q: %s: %w", m.Name, k, err)
			}
			out[k] = s
			continue
		}
		if err := checkValue(c.Type, nil, v); err != nil {
			return nil, fmt.Errorf("SDK %q: %s=%s: %w", m.Name, k, v, err)
		}
		out[k] = v
	}
	for k, c := range m.Config {
		if _, ok := out[k]; ok {
			continue
		}
		if c.Required {
			return nil, fmt.Errorf("SDK %q needs the setting %q (%s): --sdk-config %s:%s=...", m.Name, k, c.Description, m.Name, k)
		}
		if c.Default != "" {
			out[k] = c.Default
		}
	}
	return out, nil
}

// SecretKeys names the settings that must never be displayed.
func (m *Manifest) SecretKeys() []string {
	var out []string
	for k, c := range m.Config {
		if c.Type == "secret" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func resolveSecret(v string, lookup func(string) (string, bool)) (string, error) {
	switch {
	case strings.HasPrefix(v, "file:"):
		b, err := os.ReadFile(strings.TrimPrefix(v, "file:"))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	case strings.HasPrefix(v, "env:"):
		s, ok := lookup(strings.TrimPrefix(v, "env:"))
		if !ok {
			return "", fmt.Errorf("environment variable %s missing", strings.TrimPrefix(v, "env:"))
		}
		return s, nil
	}
	return "", errors.New("secrets only as file:<path> or env:<NAME>, never directly")
}

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func checkValue(typ string, values []string, v string) error {
	switch typ {
	case "string":
		if len(v) > 512 {
			return errors.New("too long (at most 512 characters)")
		}
	case "int":
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return errors.New("not an integer")
		}
	case "bool":
		if _, err := strconv.ParseBool(v); err != nil {
			return errors.New("not a boolean")
		}
	case "bytes":
		if _, err := ParseBytes(v); err != nil {
			return err
		}
	case "duration":
		if _, err := time.ParseDuration(v); err != nil {
			return errors.New("not a duration (e.g. 10s)")
		}
	case "enum":
		for _, ok := range values {
			if v == ok {
				return nil
			}
		}
		return fmt.Errorf("allowed: %s", strings.Join(values, ", "))
	case "host-pattern":
		return CheckHostPattern(v)
	default:
		return fmt.Errorf("unknown type %q", typ)
	}
	return nil
}

// CheckHostPattern: "example.com", "*.example.com", "127.0.0.1", optionally with ":port".
func CheckHostPattern(p string) error {
	host := strings.ToLower(p)
	if strings.Contains(host, "://") || strings.Contains(host, "/") {
		return errors.New("expected a host name, not a URL (e.g. api.github.com or *.example.org)")
	}
	if h, port, ok := strings.Cut(host, ":"); ok && !strings.Contains(port, ":") {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("invalid port")
		}
		host = h
	}
	host = strings.TrimPrefix(host, "*.")
	if host == "" || strings.Contains(host, "*") || len(host) > 253 {
		return errors.New("invalid host pattern (e.g. api.github.com or *.example.org)")
	}
	for _, label := range strings.Split(host, ".") {
		if !hostLabel.MatchString(label) {
			return fmt.Errorf("invalid host %q", p)
		}
	}
	return nil
}

func keys[V any](m map[string]V) string {
	var k []string
	for name := range m {
		k = append(k, name)
	}
	sort.Strings(k)
	if len(k) == 0 {
		return "none"
	}
	return strings.Join(k, ", ")
}

// ParseBytes understands 128Mi, 1Gi, 512Ki, 100M, 1G, 64K and plain byte counts.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"B", 1}}
	num, mult := s, int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	// NaN, Inf and values from 2^63 would turn into negative sizes when converted (fuzz finding, phase 4).
	if err != nil || !(v >= 0) || v*float64(mult) >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q (examples: 64Ki, 1Mi)", s)
	}
	return int64(v * float64(mult)), nil
}

// FormatBytes is the inverse for round values.
func FormatBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGi", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMi", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKi", n>>10)
	}
	return strconv.FormatInt(n, 10)
}

// ParseRate understands "10/s", "600/m", "5".
func ParseRate(s string) (float64, error) {
	num, unit, _ := strings.Cut(s, "/")
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || !(v > 0) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("invalid rate %q (e.g. 10/s)", s)
	}
	switch unit {
	case "", "s":
		return v, nil
	case "m":
		return v / 60, nil
	case "h":
		return v / 3600, nil
	}
	return 0, fmt.Errorf("invalid rate %q (unit s, m or h)", s)
}
