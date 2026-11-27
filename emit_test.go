package tsbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rif223/tsbox/internal/proto"
	"github.com/rif223/tsbox/manifest"
)

// TestEmitUnit checks Box.Emit without Deno: recipients, checks and the frame towards init.
func TestEmitUnit(t *testing.T) {
	m := &manifest.Manifest{Name: "inbox", Events: map[string]manifest.Event{"message": {}}}
	b := &Box{spec: BoxSpec{Name: "b"}, state: BoxRunning, out: make(chan []byte, 4),
		scripts: map[string]*script{"a": {run: newRun(1)}, "c": {run: newRun(1)}, "d": {}},
		order:   []string{"a", "c", "d"}, sdks: map[string]*activeSDK{"inbox": {m: m}}}
	b.scripts["a"].run.subs["inbox:message"] = true
	b.scripts["c"].run.subs["inbox:message"] = true

	n, err := b.Emit("inbox", "message", map[string]string{"topic": "x"}, "")
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	f, err := proto.Read(bufio.NewReader(bytes.NewReader(<-b.out)))
	if err != nil {
		t.Fatal(err)
	}
	if f.T != proto.TEvent || f.SDK != "inbox" || f.Name != "message" || len(f.SIDs) != 2 || string(f.P) != `{"topic":"x"}` {
		t.Fatalf("frame: %+v", f)
	}
	if n, _ := b.Emit("inbox", "message", nil, "c"); n != 1 {
		t.Fatalf("one script: %d", n)
	}
	<-b.out
	if n, _ := b.Emit("inbox", "message", nil, "d"); n != 0 {
		t.Fatalf("script without subscription: %d", n)
	}
	for _, c := range []struct{ sdk, ev, script string }{{"kv", "message", ""}, {"inbox", "other", ""}, {"inbox", "message", "zz"}} {
		if _, err := b.Emit(c.sdk, c.ev, nil, c.script); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if _, err := b.Emit("inbox", "message", json.RawMessage(`{`), ""); err == nil {
		t.Error("invalid JSON accepted")
	}
	if _, err := b.Emit("inbox", "message", json.RawMessage(`"`+strings.Repeat("a", MaxEmitPayload)+`"`), ""); err == nil {
		t.Error("oversized payload accepted")
	}
	// full queue: dropped and counted, never blocking
	for range 4 {
		b.out <- nil
	}
	if n, err := b.Emit("inbox", "message", nil, ""); n != 0 || err != nil || b.droppedEvents != 1 {
		t.Fatalf("full queue: %d %v %d", n, err, b.droppedEvents)
	}
	b.state = BoxExited
	if _, err := b.Emit("inbox", "message", nil, ""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("ended box: %v", err)
	}
}
