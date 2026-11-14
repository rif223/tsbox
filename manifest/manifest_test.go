package manifest

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const httpManifest = `{
  "name": "http", "version": "0.1.0", "tsbox": "^0.1",
  "provider": {"builtin": "http"},
  "config": {"maxBody": {"type": "bytes", "default": "1Mi"}, "token": {"type": "secret"}},
  "permissions": {"host": {"type": "host-pattern", "multiple": true, "required": true, "description": "Hosts"}},
  "methods": {"fetch": {"permission": "host", "rate": "10/s", "burst": 20, "concurrency": 4,
    "maxParams": "256Ki", "maxResult": "512Ki", "timeout": "10s"}, "ping": {}},
  "events": {"tick": {}}
}`

func TestParse(t *testing.T) {
	m, err := Parse([]byte(httpManifest))
	if err != nil {
		t.Fatal(err)
	}
	f := m.Methods["fetch"]
	if m.Import != "@tsbox/http" || m.Module != "mod.ts" || f.Rate != 10 || f.Burst != 20 || f.MaxParams != 256<<10 ||
		f.MaxResult != 512<<10 || f.Timeout != 10*time.Second || f.Concurrency != 4 {
		t.Fatalf("%+v %+v", m, f)
	}
	if p := m.Methods["ping"]; p.Rate != defaultMethod.Rate || p.Timeout != defaultMethod.Timeout {
		t.Fatalf("defaults missing: %+v", p)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":       `{"name":"x","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x"},"methods":{"a":{}},"evil":1}`,
		"Name":                `{"name":"X!","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x"},"methods":{"a":{}}}`,
		"Version":             `{"name":"x","version":"1","tsbox":"^0.1","provider":{"builtin":"x"},"methods":{"a":{}}}`,
		"API":                 `{"name":"x","version":"0.1.0","tsbox":"^9.0","provider":{"builtin":"x"},"methods":{"a":{}}}`,
		"two providers":       `{"name":"x","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x","exec":["a"]},"methods":{"a":{}}}`,
		"no methods":          `{"name":"x","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x"},"methods":{}}`,
		"permission missing":  `{"name":"x","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x"},"methods":{"a":{"permission":"p"}}}`,
		"maxParams too large": `{"name":"x","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x"},"methods":{"a":{"maxParams":"8Mi"}}}`,
		"timeout too long":    `{"name":"x","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x"},"methods":{"a":{"timeout":"1h"}}}`,
		"module with ..":      `{"name":"x","version":"0.1.0","tsbox":"^0.1","module":"../x.ts","provider":{"builtin":"x"},"methods":{"a":{}}}`,
		"Import @tsbox/core":  `{"name":"x","version":"0.1.0","tsbox":"^0.1","import":"@tsbox/core","provider":{"builtin":"x"},"methods":{"a":{}}}`,
		"secret with default": `{"name":"x","version":"0.1.0","tsbox":"^0.1","provider":{"builtin":"x"},"config":{"s":{"type":"secret","default":"x"}},"methods":{"a":{}}}`,
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGrants(t *testing.T) {
	m, _ := Parse([]byte(httpManifest))
	if err := m.CheckGrants(Grants{"host": {"api.github.com", "*.example.org", "127.0.0.1:8080"}}); err != nil {
		t.Fatal(err)
	}
	bad := map[string]Grants{
		"missing":         {},
		"unknown":         {"host": {"a.com"}, "port": {"80"}},
		"wildcard":        {"host": {"*"}},
		"wildcard middle": {"host": {"a.*.com"}},
		"path":            {"host": {"a.com/x"}},
		"scheme":          {"host": {"https://a.com"}},
	}
	for name, g := range bad {
		if err := m.CheckGrants(g); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	err := m.CheckGrants(Grants{})
	if err == nil || !strings.Contains(err.Error(), "--grant http:host=") {
		t.Fatalf("message without example: %v", err)
	}
	if _, _, _, err := ParseGrant("http-host=x"); err == nil {
		t.Fatal("ParseGrant without ':' accepted")
	}
	sdk, perm, val, err := ParseGrant("http:host=api.github.com")
	if err != nil || sdk != "http" || perm != "host" || val != "api.github.com" {
		t.Fatal(sdk, perm, val, err)
	}
}

func TestConfigAndSecrets(t *testing.T) {
	m, _ := Parse([]byte(httpManifest))
	if _, err := m.ResolveConfig(map[string]string{"token": "geheim"}); err == nil {
		t.Fatal("secret as literal accepted")
	}
	p := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(p, []byte("s3cr3t\n"), 0o600)
	t.Setenv("TSBOX_TEST_TOKEN", "envtok")
	c, err := m.ResolveConfig(map[string]string{"token": "file:" + p})
	if err != nil || c["token"] != "s3cr3t" || c["maxBody"] != "1Mi" {
		t.Fatal(c, err)
	}
	if c, err := m.ResolveConfig(map[string]string{"token": "env:TSBOX_TEST_TOKEN"}); err != nil || c["token"] != "envtok" {
		t.Fatal(c, err)
	}
	if _, err := m.ResolveConfig(map[string]string{"maxBody": "viel"}); err == nil {
		t.Fatal("invalid size accepted")
	}
	if _, err := m.ResolveConfig(map[string]string{"nope": "1"}); err == nil {
		t.Fatal("unknown setting accepted")
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(httpManifest))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Parse(b)
		if err == nil {
			_ = m.CheckGrants(Grants{"host": {"a.com"}})
		}
	})
}

// FuzzScalars: sizes, rates, grants and host patterns from manifests and the command line. Accepted
// values are valid (size ≥ 0, rate finite and > 0).
func FuzzScalars(f *testing.F) {
	for _, s := range []string{"128Mi", "1.5Gi", "0", "NaN", "Inf", "1e30Gi", "10/s", "600/m", "http:host=a.com", "*.a.com:443", "-0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if n, err := ParseBytes(s); err == nil && n < 0 {
			t.Fatalf("ParseBytes(%q) = %d", s, n)
		}
		if r, err := ParseRate(s); err == nil && (r <= 0 || math.IsInf(r, 0) || math.IsNaN(r)) {
			t.Fatalf("ParseRate(%q) = %v", s, r)
		}
		if a, b, c, err := ParseGrant(s); err == nil && (a == "" || b == "" || c == "") {
			t.Fatalf("ParseGrant(%q) = %q %q %q", s, a, b, c)
		}
		if CheckHostPattern(s) == nil && (strings.ContainsAny(s, "/@ ") || s == "") {
			t.Fatalf("host pattern %q accepted", s)
		}
	})
}

// FuzzGrantsAndConfig: values for rights and settings of all types are checked without crashing.
func FuzzGrantsAndConfig(f *testing.F) {
	m, err := Parse([]byte(`{"name":"f","version":"0.1.0","tsbox":"^0.1","import":"@x/f","module":"mod.ts",
	  "provider":{"builtin":"f"},
	  "config":{"s":{"type":"string"},"i":{"type":"int"},"b":{"type":"bool"},"y":{"type":"bytes"},"d":{"type":"duration"},"k":{"type":"secret"}},
	  "permissions":{"h":{"type":"host-pattern","multiple":true},"n":{"type":"int"},"z":{"type":"bytes"},
	    "e":{"type":"enum","values":["a","b"]},"t":{"type":"string"}},
	  "methods":{"m":{}}}`))
	if err != nil {
		f.Fatal(err)
	}
	f.Add("h", "a.com", "y", "1Mi")
	f.Add("e", "c", "k", "env:X")
	f.Add("n", "-1", "d", "5x")
	f.Fuzz(func(t *testing.T, perm, val, key, cval string) {
		_ = m.CheckGrants(Grants{perm: {val}})
		_, _ = m.ResolveConfigEnv(map[string]string{key: cval}, func(string) (string, bool) { return "v", true })
	})
}
