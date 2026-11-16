package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFS() *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.String("mem", "128Mi", "RAM")
	fs.Float64("cpu", 0.5, "CPU")
	fs.Int("pids", 64, "PIDs")
	fs.Duration("timeout", 0, "time limit")
	fs.Bool("q", false, "quiet")
	usageFor(fs, "run <script.ts> [flags]")
	return fs
}

// captureStderr runs fn and returns what it wrote to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestParsePos(t *testing.T) {
	cases := []struct {
		args     []string
		min, max int
		ok       bool
		pos      string
		msg      string
	}{
		{[]string{"a.ts", "-q"}, 1, 1, true, "a.ts", ""},
		{[]string{"-q", "a.ts", "--mem", "1Gi"}, 1, 1, true, "a.ts", ""},
		{[]string{"--", "-x.ts"}, 1, 1, true, "-x.ts", ""},
		{[]string{}, 1, 1, false, "", "tsbox run: missing argument"},
		{[]string{"a.ts", "b.ts"}, 1, 1, false, "", `too many arguments (unexpected "b.ts")`},
		{[]string{"x"}, 0, 0, false, "", `takes no arguments (got "x")`},
		{[]string{"--memory", "1Gi", "a.ts"}, 1, 1, false, "", "unknown flag --memory (did you mean --mem?)"},
		{[]string{"--cpu", "0,5", "a.ts"}, 1, 1, false, "", `--cpu: invalid value "0,5" (a number with a dot`},
		{[]string{"--timeout", "5", "a.ts"}, 1, 1, false, "", `--timeout: invalid value "5" (a duration with a unit`},
		{[]string{"--pids", "x", "a.ts"}, 1, 1, false, "", "(a whole number)"},
		{[]string{"a.ts", "--mem"}, 1, 1, false, "", "--mem needs a value"},
	}
	for _, c := range cases {
		var pos []string
		var ok bool
		msg := captureStderr(t, func() { pos, ok = parsePos(newFS(), c.args, c.min, c.max) })
		if ok != c.ok || (ok && strings.Join(pos, " ") != c.pos) {
			t.Errorf("%q: ok=%v pos=%q", c.args, ok, pos)
		}
		if c.msg != "" && !strings.Contains(msg, c.msg) {
			t.Errorf("%q: message %q, expected %q", c.args, msg, c.msg)
		}
		if !ok && !strings.Contains(msg, `Run "tsbox run -h" for help.`) {
			t.Errorf("%q: hint missing in %q", c.args, msg)
		}
		if strings.Count(msg, "\n") > 3 {
			t.Errorf("%q: error output too long (the flag list belongs to -h): %q", c.args, msg)
		}
	}
}

func TestHelp(t *testing.T) {
	helpShown = false
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	_, ok := parsePos(newFS(), []string{"-h"}, 1, 1)
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)
	if ok || !helpShown {
		t.Fatalf("ok=%v helpShown=%v", ok, helpShown)
	}
	helpShown = false
	for _, want := range []string{"Usage: tsbox run <script.ts> [flags]", "--mem string", "-q ", `(default "128Mi")`, "(default 0.5)"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("help without %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "(default 0s)") || strings.Contains(string(out), "(default false)") {
		t.Errorf("help shows zero defaults:\n%s", out)
	}
}

func TestSuggest(t *testing.T) {
	if s := suggestCmd("lgos"); s != "logs" {
		t.Errorf("lgos: %q", s)
	}
	if s := suggestCmd("frobnicate"); s != "" {
		t.Errorf("frobnicate: %q", s)
	}
	if s := suggestFlag(newFS(), "memory"); s != "mem" {
		t.Errorf("memory: %q", s)
	}
}

func TestNotFound(t *testing.T) {
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir("../..") // repository root
	if msg := notFound("hello.ts"); !strings.Contains(msg, "did you mean examples") || !strings.Contains(msg, "hello.ts?") {
		t.Errorf("hello.ts: %q", msg)
	}
	if msg := notFound("does-not-exist.ts"); strings.Contains(msg, "did you mean") {
		t.Errorf("does-not-exist.ts: %q", msg)
	}
}

func TestHintPath(t *testing.T) {
	cases := map[string]string{
		"greet":                 "./greet",
		"greet/example.ts":      "./greet/example.ts",
		"./greet":               "./greet",
		"../x/greet":            "../x/greet",
		"sub dir/greet":         `"./sub dir/greet"`,
		filepath.Join("a", "b"): "./a/b",
	}
	for in, want := range cases {
		if got := hintPath(in); got != want {
			t.Errorf("hintPath(%q) = %q, want %q", in, got, want)
		}
	}
	abs, _ := filepath.Abs("x y")
	if got := hintPath(abs); got != `"`+filepath.ToSlash(abs)+`"` {
		t.Errorf("absolute: %q", got)
	}
}

func TestSecondsFlag(t *testing.T) {
	for in, want := range map[string]time.Duration{"10": 10 * time.Second, "0": 0, "1500ms": 1500 * time.Millisecond, "2m": 2 * time.Minute} {
		var s seconds
		if err := s.Set(in); err != nil || time.Duration(s) != want {
			t.Errorf("%q: %v %v", in, time.Duration(s), err)
		}
	}
	for _, in := range []string{"x", "-1", "-5s", ""} {
		var s seconds
		if s.Set(in) == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
