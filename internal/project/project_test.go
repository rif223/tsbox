package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rif223/tsbox"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sample = `{
  "name": "demo",
  "boxes": {
    "bot": {
      "limits": { "memory": "256Mi", "cpu": 0.5, "startCpu": 2, "pids": 64 },
      "restart": "on-failure:5", "profile": "deno-next",
      "labels": { "team": "a" },
      "sdks": {
        "kv": {},
        "http": { "grants": { "host": ["api.github.com"] }, "config": { "token": "env:GITHUB_TOKEN", "file": "file:secret.txt" } }
      },
      "scripts": {
        "main": { "entry": "src/bot.ts", "limits": { "rss": "96Mi", "watchdog": "0", "timeout": "30s" }, "params": { "x": 1 } },
        "cron": { "entry": "src/cron.ts", "restart": "always" }
      }
    },
    "web": { "scripts": { "main": { "entry": "src/bot.ts" } } }
  }
}`

func TestParse(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "src/bot.ts", "console.log(1)")
	write(t, dir, "src/cron.ts", "console.log(2)")
	p, err := Parse([]byte(sample), dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo" || len(p.Boxes) != 2 || p.Boxes[0].Key != "bot" {
		t.Fatalf("%+v", p)
	}
	s := p.Boxes[0].Spec
	if s.Name != "demo-bot" || s.Profile != "deno-next" || s.Limits.StartCPU != 2 || s.Limits.Memory != 256<<20 || s.Restart.Mode != tsbox.RestartOnFailure || s.Restart.MaxFailures != 5 {
		t.Fatalf("Box: %+v", s)
	}
	if len(s.Scripts) != 2 || s.Scripts[0].Name != "cron" || s.Scripts[0].Restart.Mode != tsbox.RestartAlways {
		t.Fatalf("Scripts: %+v", s.Scripts)
	}
	m := s.Scripts[1]
	if m.Entry != filepath.Join(dir, "src", "bot.ts") || m.Limits.RSS != 96<<20 || m.Limits.Watchdog != -1 || m.Limits.Timeout != 30*time.Second {
		t.Fatalf("main: %+v", m)
	}
	if s.SDKs[0].Name != "http" || s.SDKs[0].Config["token"] != "env:GITHUB_TOKEN" ||
		s.SDKs[0].Config["file"] != "file:"+filepath.Join(dir, "secret.txt") || s.SDKs[1].Name != "kv" {
		t.Fatalf("SDKs: %+v", s.SDKs)
	}
	h1 := p.Boxes[0].Hash
	write(t, dir, "src/cron.ts", "console.log(3)")
	p2, _ := Parse([]byte(sample), dir)
	if p2.Boxes[0].Hash == h1 {
		t.Fatal("hash does not change with the source")
	}
}

func TestParseErrors(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.ts", "")
	cases := map[string]string{
		`{"boxes":{"x":{"scripts":{"m":{"entry":"a.ts"}}}},"extra":1}`: "unknown field",
		`{"boxes":{}}`: "no boxes",
		`{"boxes":{"X":{"scripts":{"m":{"entry":"a.ts"}}}}}`:                                "allowed",
		`{"boxes":{"x":{"scripts":{}}}}`:                                                    "no scripts",
		`{"boxes":{"x":{"scripts":{"m":{"entry":"missing.ts"}}}}}`:                          "missing.ts",
		`{"boxes":{"x":{"restart":"soon","scripts":{"m":{"entry":"a.ts"}}}}}`:               "unknown",
		`{"boxes":{"x":{"limits":{"memory":"lots"},"scripts":{"m":{"entry":"a.ts"}}}}}`:     "size",
		`{"boxes":{"x":{"labels":{"tsbox.project":"y"},"scripts":{"m":{"entry":"a.ts"}}}}}`: "reserved",
		`{"boxes":{"x":{"scripts":{"m":{"entry":"a.ts","limits":{"timeout":"soon"}}}}}}`:    "duration",
		`{"boxes":{"x":{"driver":"docker","scripts":{"m":{"entry":"a.ts"}}}}}`:              "driver",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in), dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (expected %q)", in, err, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(sample))
	f.Add([]byte(`{"boxes":{"x":{"scripts":{"m":{"entry":"a.ts"}}}}}`))
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Parse(b, dir)
	})
}

// FuzzScalars: sizes and restart policies from tsbox.json and the command line.
func FuzzScalars(f *testing.F) {
	for _, s := range []string{"128Mi", "1Gi", "NaN", "Inf", "1e19", "on-failure:5", "on-failure:-1", "always", "no:3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if n, err := ParseBytes(s); err == nil && n < 0 {
			t.Fatalf("ParseBytes(%q) = %d", s, n)
		}
		if p, err := ParseRestart(s); err == nil && p.MaxFailures < 0 {
			t.Fatalf("ParseRestart(%q) = %+v", s, p)
		}
	})
}

// TestLoadBOM: tsbox.json written by PowerShell 5.1 (UTF-8 with BOM, UTF-16 LE) loads; ANSI is refused with
// a hint.
func TestLoadBOM(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.ts", "console.log(1)")
	const doc = `{"boxes":{"a":{"scripts":{"m":{"entry":"a.ts"}}}}}`
	utf16 := []byte{0xFF, 0xFE}
	for _, r := range doc {
		utf16 = append(utf16, byte(r), 0)
	}
	for name, raw := range map[string][]byte{"bom.json": append([]byte{0xEF, 0xBB, 0xBF}, doc...), "u16.json": utf16} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := filepath.Join(dir, "ansi.json")
	os.WriteFile(p, []byte(`{"name":"`+"\xfc"+`","boxes":{}}`), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "not UTF-8") {
		t.Errorf("ANSI: %v", err)
	}
}

func TestParseDriver(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.ts", "")
	in := `{"boxes":{"x":{"scripts":{"m":{"entry":"a.ts"}}},"y":{"driver":"dev","scripts":{"m":{"entry":"a.ts"}}}}}`
	p, err := Parse([]byte(in), dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Boxes[0].Spec.Driver != "" || p.Boxes[1].Spec.Driver != "dev" {
		t.Fatalf("%+v %+v", p.Boxes[0].Spec, p.Boxes[1].Spec)
	}
	if p.Boxes[0].Hash == p.Boxes[1].Hash {
		t.Fatal("hash does not depend on the driver")
	}
}
