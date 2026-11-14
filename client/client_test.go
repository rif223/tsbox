package client

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/rif223/tsbox"
)

func TestSocketPath(t *testing.T) {
	cases := map[string]string{
		"unix:///run/tsbox/tsboxd.sock": "/run/tsbox/tsboxd.sock",
		"/tmp/x.sock":                   "/tmp/x.sock",
		"npipe:////./pipe/tsboxd":       `\\.\pipe\tsboxd`,
		`npipe://\\.\pipe\tsboxd`:       `\\.\pipe\tsboxd`,
	}
	for in, want := range cases {
		if got, err := SocketPath(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := SocketPath("tcp://localhost:1"); err == nil {
		t.Error("tcp:// accepted")
	}
}

func TestUserSocket(t *testing.T) {
	short := UserSocket(t.TempDir())
	long := UserSocket("/" + strings.Repeat("x", 200))
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(short, `\\.\pipe\tsboxd-`) || short == long {
			t.Fatalf("%s %s", short, long)
		}
		return
	}
	if len(long) > 100 {
		t.Fatalf("too long: %s", long)
	}
	if UserSocket("/a") != "/a/run/tsboxd.sock" {
		t.Fatal(UserSocket("/a"))
	}
}

func TestEnvRefs(t *testing.T) {
	spec := tsbox.BoxSpec{SDKs: []tsbox.SDKUse{{Name: "x", Config: map[string]string{"a": "env:TSBOX_TEST_REF", "b": "file:/x", "c": "1"}}}}
	t.Setenv("TSBOX_TEST_REF", "v")
	if refs := EnvRefs(spec); len(refs) != 1 || refs[0] != "TSBOX_TEST_REF" {
		t.Fatal(refs)
	}
	if env := LookupEnv(spec); env["TSBOX_TEST_REF"] != "v" || len(env) != 1 {
		t.Fatal(env)
	}
}

// FuzzNDJSON: daemon streams (logs, events, stats, attach) – broken lines end with an error, never with a
// crash.
func FuzzNDJSON(f *testing.F) {
	f.Add([]byte(`{"type":"log","log":{"seq":1,"msg":"x"}}` + "\n" + `{"type":"exit","status":{"name":"a"}}` + "\n"))
	f.Add([]byte(`{"type":"error","error":{"code":"build","message":"m"}}`))
	f.Add([]byte(`{"seq":1}` + "\n" + `{"kind":"box_started"`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ndjson(bytes.NewReader(b), func(AttachMsg) bool { return true })
		_, _ = ndjson(bytes.NewReader(b), func(tsbox.LogLine) bool { return true })
		_, _ = ndjson(bytes.NewReader(b), func(tsbox.Event) bool { return true })
		_, _ = ndjson(bytes.NewReader(b), func([]StatsEntry) bool { return true })
	})
}
