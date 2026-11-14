package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rif223/tsbox/internal/denobin"
	"github.com/rif223/tsbox/internal/sdkreg"
	"github.com/rif223/tsbox/internal/sdktest"
	"github.com/rif223/tsbox/sdk"
)

// TestEcho: conformance of the sample provider (Deno) – handshake, call, events, errors, restart.
func TestEcho(t *testing.T) {
	deno, err := denobin.Find()
	if err != nil {
		t.Skip(err)
	}
	s, err := sdkreg.New(t.TempDir()).Resolve("echo")
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Name: "echo", Version: "0.1.0", Dir: s.Dir, Exec: s.Manifest.Provider.Exec, Deno: deno,
		Settings: map[string]string{"prefix": "> "}})
	defer p.Close()
	sess := &sdktest.Session{G: sdk.Grants{"ticks": {"2"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := p.Open(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Call(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || string(r.(json.RawMessage)) != `{"text":"> hi"}` {
		t.Fatalf("%s %v", r, err)
	}
	r, err = h.Call(ctx, "tick", json.RawMessage(`{"n":5}`))
	if err != nil || string(r.(json.RawMessage)) != `{"sent":2}` || len(sess.Events) != 2 {
		t.Fatalf("tick: %s %v events=%v", r, err, sess.Events)
	}
	var se *sdk.Error
	if _, err := h.Call(ctx, "nope", nil); !errors.As(err, &se) || se.Code != sdk.NotFound {
		t.Fatalf("unknown method: %v", err)
	}
	// Crash: open calls fail with unavailable, the next Open restarts.
	p.proc.cmd.Process.Kill()
	<-p.proc.done
	if _, err := h.Call(ctx, "echo", json.RawMessage(`{"text":"x"}`)); !errors.As(err, &se) || se.Code != sdk.Unavailable {
		t.Fatalf("after crash: %v", err)
	}
	time.Sleep(restartDelay)
	h2, err := p.Open(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h2.Call(ctx, "echo", json.RawMessage(`{"text":"x"}`)); err != nil {
		t.Fatal(err)
	}
}
