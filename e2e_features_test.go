package tsbox

// E2E tests of inbox (Box.Emit, script messages), cron, the extended kv, Update, tenant quotas and the SDK
// call counters, with real Deno processes.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSDKKVVersionsAndTTL(t *testing.T) {
	e, rec := newEngine(t)
	spec := sdkBox(t, "sdk/kv2.ts", SDKUse{Name: "kv"})
	spec.Storage = boxFor(t)
	if ex := lastExit(t, runToEnd(t, e, spec, time.Minute)); ex.Reason != ReasonExit {
		t.Fatalf("%+v %q", ex, rec.messages("main"))
	}
	expectLogs(t, rec, "KV2 incr 1 5", "KV2 create-only true false", "KV2 cas true false", "KV2 ttl undefined",
		"KV2 page p0,p1 true all p0,p1,p2,p3,p4")
}

func (r *recorder) find(kind EventKind) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, ev := range r.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestInboxEmitAndSend(t *testing.T) {
	e, rec := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := e.Run(ctx, sdkBox(t, "sdk/inbox.ts", SDKUse{Name: "inbox"}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background(), time.Second)
	waitFor(t, "ready", func() bool { return b.Status().Scripts[0].State == ScriptReady })
	if _, err := b.Emit("inbox", "nope", nil, ""); err == nil {
		t.Fatal("unknown event accepted")
	}
	if _, err := b.Emit("kv", "message", nil, ""); err == nil {
		t.Fatal("inactive SDK accepted")
	}
	if _, err := b.Emit("inbox", "message", nil, "ghost"); err == nil {
		t.Fatal("unknown script accepted")
	}
	if _, err := b.Emit("inbox", "message", json.RawMessage(`{"topic":`), ""); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	// The subscription may arrive shortly after ready.
	waitFor(t, "subscription", func() bool {
		n, err := b.Emit("inbox", "message", map[string]any{"topic": "ping", "data": map[string]int{"n": 41}}, "")
		if err != nil {
			t.Fatal(err)
		}
		return n == 1
	})
	expectLogs(t, rec, `INBOX got ping {"n":41}`)
	waitFor(t, "script_message", func() bool { return len(rec.find(EventScriptMessage)) > 0 })
	ev := rec.find(EventScriptMessage)[0]
	if ev.Detail != "pong" || ev.Script != "main" || string(ev.Payload) != `{"n":42}` {
		t.Fatalf("message: %+v (%s)", ev, ev.Payload)
	}
	if _, err := b.Emit("inbox", "message", map[string]any{"topic": "bye"}, "main"); err != nil {
		t.Fatal(err)
	}
	st, err := b.Wait(ctx) // the last subscription ended: the script exits
	if err != nil || lastExit(t, st).Reason != ReasonExit {
		t.Fatalf("%v %+v", err, st)
	}
	if _, err := b.Emit("inbox", "message", nil, ""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("emit to an ended box: %v", err)
	}
	calls := e.SDKCalls()
	if !slices.ContainsFunc(calls, func(c SDKCallCount) bool {
		return c.Box == b.Name() && c.SDK == "inbox" && c.Method == "send" && c.Code == "ok" && c.Count == 2
	}) {
		t.Fatalf("call counters: %+v", calls)
	}
}

func TestCronTicks(t *testing.T) {
	e, rec := newEngine(t)
	use := SDKUse{Name: "cron", Grants: map[string][]string{"schedule": {"fast=@every 1s", "nightly=0 3 * * *"}}}
	if ex := lastExit(t, runToEnd(t, e, sdkBox(t, "sdk/cron.ts", use), time.Minute)); ex.Reason != ReasonExit {
		t.Fatalf("%+v %q", ex, rec.messages("main"))
	}
	expectLogs(t, rec, "CRON schedules fast,nightly UTC", "CRON tick fast true")
	bad := SDKUse{Name: "cron", Grants: map[string][]string{"schedule": {"61 * * * *"}}}
	if _, err := e.Create(context.Background(), sdkBox(t, "sdk/cron.ts", bad)); err == nil || !strings.Contains(err.Error(), "minute") {
		t.Fatalf("invalid schedule: %v", err)
	}
}

func TestUpdate(t *testing.T) {
	e, rec := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	spec := one(t, "scripts/version.ts", func(s *BoxSpec) { s.Scripts[0].Params = json.RawMessage(`{"v":1}`) })
	b, err := e.Run(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if nb, ok := e.Box(spec.Name); ok {
			_ = nb.Stop(context.Background(), time.Second)
		}
	}()
	expectLogs(t, rec, `VERSION {"v":1}`)

	// A broken build leaves the old box running.
	broken := spec
	broken.Scripts = []ScriptSpec{{Name: "main", Entry: "testdata/scripts/broken/typeerror.ts"}}
	if _, err := e.Update(ctx, broken, time.Second); err == nil {
		t.Fatal("update with a type error accepted")
	}
	if st := b.Status(); st.State != BoxRunning {
		t.Fatalf("old box after a failed update: %s", st.State)
	}

	next := spec
	next.Scripts = []ScriptSpec{{Name: "main", Entry: spec.Scripts[0].Entry, Params: json.RawMessage(`{"v":2}`)}}
	next.Limits.Memory = 192 << 20
	nb, err := e.Update(ctx, next, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if nb == b || nb.Status().State != BoxRunning || nb.Status().Limits.Memory != 192<<20 {
		t.Fatalf("new box: %+v", nb.Status())
	}
	if b.Status().State == BoxRunning {
		t.Fatal("old box still running")
	}
	expectLogs(t, rec, `VERSION {"v":2}`)
	if cur, _ := e.Box(spec.Name); cur != nb {
		t.Fatal("engine does not know the new box")
	}
	if _, err := e.Update(ctx, one(t, "scripts/version.ts", nil), time.Second); !errors.Is(err, ErrUnknownBox) {
		t.Fatalf("update of an unknown box: %v", err)
	}
}

func TestTenantQuotas(t *testing.T) {
	e, _ := newEngine(t, WithTenantQuotas("tenant", func(tenant string) Quota {
		if tenant == "small" {
			return Quota{Boxes: 2, Memory: 300 << 20}
		}
		return Quota{}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mk := func(mem int64) BoxSpec {
		return one(t, "scripts/service.ts", func(s *BoxSpec) {
			s.Labels = map[string]string{"tenant": "small"}
			s.Limits.Memory = mem
		})
	}
	a, err := e.Run(ctx, mk(200<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background(), time.Second)
	b, err := e.Create(ctx, mk(200<<20))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("memory quota: %v", err)
	}
	if _, err := e.Create(ctx, mk(64<<20)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("box quota: %v", err)
	}
	u := e.Tenants()
	if len(u) != 1 || u[0].Boxes != 2 || u[0].Running != 1 || u[0].Limits.Memory != 200<<20 {
		t.Fatalf("usage: %+v", u)
	}
	other := one(t, "scripts/service.ts", func(s *BoxSpec) { s.Labels = map[string]string{"tenant": "big"} })
	ob, err := e.Run(ctx, other)
	if err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	_ = ob.Stop(context.Background(), time.Second)
}
