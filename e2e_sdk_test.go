package tsbox

// E2E tests of the SDK system with real Deno processes: built-in providers (kv, log, http), external
// provider (echo), rights, limits and under gVisor the build sandbox.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func sdkBox(t *testing.T, entry string, sdks ...SDKUse) BoxSpec {
	return BoxSpec{Name: boxFor(t), Driver: driverName(), Storage: "e2e-" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-"),
		SDKs: sdks, Scripts: []ScriptSpec{{Name: "main", Entry: "testdata/" + entry}}}
}

func expectLogs(t *testing.T, rec *recorder, want ...string) {
	t.Helper()
	var all string
	for i := 0; i < 60; i++ { // logs are delivered asynchronously
		all = strings.Join(rec.messages("main"), "\n")
		missing := false
		for _, w := range want {
			if !strings.Contains(all, w) {
				missing = true
			}
		}
		if !missing {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in:\n%s", w, all)
		}
	}
}

func TestSDKKVAndLog(t *testing.T) {
	e, rec := newEngine(t)
	spec := sdkBox(t, "sdk/kv.ts", SDKUse{Name: "kv", Config: map[string]string{"quota": "32Ki"}})
	spec.Storage = fmt.Sprintf("kv-%d", time.Now().UnixNano()%1e9)
	for run := 1; run <= 2; run++ {
		s := spec
		s.Name = boxFor(t)
		if ex := lastExit(t, runToEnd(t, e, s, time.Minute)); ex.Reason != ReasonExit {
			t.Fatalf("run %d: %+v\n%q", run, ex, rec.messages("main"))
		}
	}
	// The second run sees the value of the first (persistent per storage area and script).
	expectLogs(t, rec, "KV n 1 obj", "KV n 2 obj", `{"a":[1,2]}`, "keys n,obj", "KV deleted true false",
		"KV big too_large", "KV quota quota_exceeded")

	e2, rec2 := newEngine(t)
	if ex := lastExit(t, runToEnd(t, e2, sdkBox(t, "sdk/log.ts", SDKUse{Name: "log"}), time.Minute)); ex.Reason != ReasonExit {
		t.Fatal(ex)
	}
	expectLogs(t, rec2, `structured {"id":42,"ok":true}`, "warning")
}

func TestSDKHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pong")) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	params, _ := json.Marshal(map[string]string{"url": srv.URL + "/ping", "other": "http://example.com/"})
	e, rec := newEngine(t)
	spec := sdkBox(t, "sdk/http.ts", SDKUse{Name: "http", Grants: map[string][]string{"host": {u.Host}},
		Config: map[string]string{"allowHTTP": "true", "allowPrivate": "true"}})
	spec.Scripts[0].Params = params
	if ex := lastExit(t, runToEnd(t, e, spec, time.Minute)); ex.Reason != ReasonExit {
		t.Fatalf("%+v %q", ex, rec.messages("main"))
	}
	expectLogs(t, rec, "HTTP 200 pong", "HTTP denied permission_denied", "HTTP denied invalid_argument")
	if strings.Contains(strings.Join(rec.messages("main"), "\n"), "ALLOWED") {
		t.Fatal("host that was not granted is reachable")
	}
}

// TestSDKGrants: required rights, unknown rights, secrets only as file:/env:, missing dependencies.
func TestSDKGrants(t *testing.T) {
	e, _ := newEngine(t)
	cases := []struct {
		use  SDKUse
		want string
	}{
		{SDKUse{Name: "http"}, "--grant http:host="},
		{SDKUse{Name: "http", Grants: map[string][]string{"host": {"*"}}}, "host pattern"},
		{SDKUse{Name: "http", Grants: map[string][]string{"host": {"a.com"}, "port": {"1"}}}, "has no right"},
		{SDKUse{Name: "kv", Config: map[string]string{"quota": "lots"}}, "invalid size"},
		{SDKUse{Name: "doesnotexist"}, "not found"},
	}
	for _, c := range cases {
		_, err := e.Check(context.Background(), sdkBox(t, "sdk/log.ts", SDKUse{Name: "log"}, c.use))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v (expected %q)", c.use, err, c.want)
		}
	}
}

// TestSDKLimits: rate/concurrency, parameter size and the same answer for an unknown SDK/method.
func TestSDKLimits(t *testing.T) {
	e, rec := newEngine(t)
	if ex := lastExit(t, runToEnd(t, e, sdkBox(t, "sdk/limits.ts", SDKUse{Name: "kv"}), time.Minute)); ex.Reason != ReasonExit {
		t.Fatalf("%+v %q", ex, rec.messages("main"))
	}
	expectLogs(t, rec, "LIMIT rate true true", "LIMIT oracle not_found not_found", "LIMIT params too_large")
}

func TestSDKExternalEcho(t *testing.T) {
	e, rec := newEngine(t)
	spec := sdkBox(t, "../examples/echo.ts", SDKUse{Name: "echo", Grants: map[string][]string{"ticks": {"3"}}})
	if ex := lastExit(t, runToEnd(t, e, spec, time.Minute)); ex.Reason != ReasonExit {
		t.Fatalf("%+v %q", ex, rec.messages("main"))
	}
	expectLogs(t, rec, "echo: hello", "tick 1", "tick 3", "sent: 3")
	if strings.Contains(strings.Join(rec.messages("main"), "\n"), "tick 4") {
		t.Fatal("grant echo:ticks not enforced")
	}
}

// TestSDKPathInRepo: path SDK with an external provider under the repo's deno.json, which maps @tsbox/core to
// js/core. Before, deno then bundled core twice and the first SDK call hung.
func TestSDKPathInRepo(t *testing.T) {
	e, rec := newEngine(t)
	spec := sdkBox(t, "../examples/echo.ts", SDKUse{Name: "./sdks/echo", Grants: map[string][]string{"ticks": {"2"}}})
	if ex := lastExit(t, runToEnd(t, e, spec, time.Minute)); ex.Reason != ReasonExit {
		t.Fatalf("%+v %q", ex, rec.messages("main"))
	}
	expectLogs(t, rec, "echo: hello", "tick 2", "sent: 2")
}

// TestBuildSandbox (gVisor, Firecracker): the build runs without network and sees only the script's
// directory.
func TestBuildSandbox(t *testing.T) {
	needIsolation(t)
	e, _ := newEngine(t)
	if _, err := e.Check(context.Background(), sdkBox(t, "sdk/outside.ts")); err == nil ||
		!strings.Contains(err.Error(), "lib_outside") {
		t.Fatalf("import outside the context built: %v", err)
	}
	for _, entry := range []string{"sdk/kv.ts", "../examples/hello.ts"} {
		if _, err := e.Check(context.Background(), sdkBox(t, entry, SDKUse{Name: "kv"})); err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
	}
}

// TestBuildCacheEngine: the second start of the same script with the same SDKs does not rebuild (under
// gVisor this saves the build sandbox); WithoutBuildCache always builds.
func TestBuildCacheEngine(t *testing.T) {
	e, _ := newEngine(t)
	spec := sdkBox(t, "sdk/kv.ts", SDKUse{Name: "kv"})
	first, err := e.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	e.forget(spec.Name, first)
	start := time.Now()
	spec.Name = boxFor(t)
	second, err := e.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer e.forget(spec.Name, second)
	if a := second.scripts["main"].artifact; !a.Cached || a.SHA256 != first.scripts["main"].artifact.SHA256 {
		t.Fatalf("no cache hit: %+v", a)
	}
	t.Logf("Create with cache hit: %v", time.Since(start).Round(time.Millisecond))
	nc, _ := newEngine(t, WithoutBuildCache())
	spec.Name = boxFor(t)
	b, err := nc.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.forget(spec.Name, b)
	if b.scripts["main"].artifact.Cached {
		t.Fatal("WithoutBuildCache: hit")
	}
}

// The rate limit of an SDK method belongs to the script, not to the run: a restarted script does not get a
// full burst again (before, --restart always doubled the allowed rate).
func TestSDKRateLimitAcrossRestarts(t *testing.T) {
	e, rec := newEngine(t)
	use := SDKUse{Name: "http", Grants: map[string][]string{"host": {"127.0.0.1"}}}
	spec := sdkBox(t, "sdk/ratespend.ts", use)
	spec.Restart = RestartPolicy{Mode: RestartOnFailure, MaxFailures: 2, BackoffMin: 50 * time.Millisecond}
	runToEnd(t, e, spec, 2*time.Minute)
	var runs []int
	for _, m := range rec.messages("main") {
		var n int
		if _, err := fmt.Sscanf(m, "RATE passed %d", &n); err == nil {
			runs = append(runs, n)
		}
	}
	if len(runs) != 2 || runs[0] < 20 || runs[1] > 5 {
		t.Fatalf("calls past the rate limit per run: %v (want 20+, then only what refilled)", runs)
	}
}
