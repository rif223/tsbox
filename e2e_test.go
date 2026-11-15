package tsbox

// E2E tests with real Deno processes and the real tsbox-init. Driver via TSBOX_TEST_DRIVER (dev or gvisor;
// default dev). gVisor needs root, runsc and a rootfs (TSBOX_ROOTFS).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rif223/tsbox/internal/denobin"
)

var (
	testInit  string // built tsbox binary (init)
	testState string // shared state (artifact cache)
	testDeno  string
)

func TestMain(m *testing.M) {
	code := func() int {
		d, err := denobin.Find()
		if err != nil {
			fmt.Println("E2E skipped:", err)
			return m.Run()
		}
		testDeno = d
		dir, err := os.MkdirTemp("", "tsbox-e2e-")
		if err != nil {
			panic(err)
		}
		testState = filepath.Join(dir, "state")
		defer func() {
			// runsc mounts <state>/gvisor/runsc/null-netns as a bind mount; without umount the directory stays.
			if runtime.GOOS == "linux" {
				_ = exec.Command("umount", "-l", filepath.Join(testState, "gvisor", "runsc", "null-netns")).Run()
			}
			removeTemp(dir)
		}()
		testInit = filepath.Join(dir, "tsbox")
		if runtime.GOOS == "windows" {
			testInit += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", testInit, "./cmd/tsbox")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("building tsbox: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// removeTemp deletes a test directory. On Windows tsbox.exe and lock files are still in use shortly after
// daemon and boxes ended; without retries the directories would stay in %TEMP%.
func removeTemp(dir string) {
	var err error
	for i := 0; i < 50; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "test directory stays: %v\n", err)
}

// noMeasurement: the dev driver on macOS measures neither RAM nor RSS nor threads (README, limits table), so
// these limits do not apply there.
func noMeasurement() bool { return runtime.GOOS == "darwin" && driverName() == "dev" }

func driverName() string {
	if d := os.Getenv("TSBOX_TEST_DRIVER"); d != "" {
		return d
	}
	return "dev"
}

func needGVisor(t *testing.T) {
	if driverName() != "gvisor" {
		t.Skip("only with TSBOX_TEST_DRIVER=gvisor")
	}
}

// needIsolation: tests of the isolating drivers (gvisor, firecracker).
func needIsolation(t *testing.T) {
	if !isolatedDriver() {
		t.Skip("only with TSBOX_TEST_DRIVER=gvisor or firecracker")
	}
}

func isolatedDriver() bool { return driverName() == "gvisor" || driverName() == "firecracker" }

type recorder struct {
	mu     sync.Mutex
	logs   []LogLine
	events []Event
}

func (r *recorder) log(l LogLine) { r.mu.Lock(); r.logs = append(r.logs, l); r.mu.Unlock() }
func (r *recorder) ev(e Event)    { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }

func (r *recorder) messages(script string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, l := range r.logs {
		if script == "" || l.Script == script {
			out = append(out, l.Msg)
		}
	}
	return out
}

func (r *recorder) has(kind EventKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.events, func(e Event) bool { return e.Kind == kind })
}

func newEngine(t testing.TB, opts ...Option) (*Engine, *recorder) {
	t.Helper()
	if testDeno == "" {
		t.Skip("no Deno")
	}
	rec := &recorder{}
	e, err := New(context.Background(), append([]Option{WithDriver(driverName()), WithDeno(testDeno), WithInit(testInit),
		WithStateDir(testState), WithLogs(rec.log), WithEvents(rec.ev)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e, rec
}

var nameSeq struct {
	sync.Mutex
	n int
}

func boxFor(t testing.TB) string {
	nameSeq.Lock()
	defer nameSeq.Unlock()
	nameSeq.n++
	return fmt.Sprintf("t%d-%d", os.Getpid()%10000, nameSeq.n)
}

func one(t testing.TB, entry string, mod func(*BoxSpec)) BoxSpec {
	s := BoxSpec{Name: boxFor(t), Scripts: []ScriptSpec{{Name: "main", Entry: filepath.Join("testdata", entry)}}}
	if mod != nil {
		mod(&s)
	}
	return s
}

// runToEnd starts the box, waits for its end and returns the status.
func runToEnd(t *testing.T, e *Engine, spec BoxSpec, limit time.Duration) BoxStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	b, err := e.Run(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	st, err := b.Wait(ctx)
	if err != nil {
		_ = b.Stop(context.Background(), time.Second)
		t.Fatalf("box does not end (%v): %+v", err, b.Status())
	}
	return st
}

func lastExit(t *testing.T, st BoxStatus) ExitStatus {
	t.Helper()
	if len(st.Scripts) == 0 || st.Scripts[0].LastExit == nil {
		t.Fatalf("no exit: %+v", st)
	}
	return *st.Scripts[0].LastExit
}

func TestHello(t *testing.T) {
	e, rec := newEngine(t)
	st := runToEnd(t, e, one(t, "../examples/hello.ts", nil), time.Minute)
	if ex := lastExit(t, st); ex.Reason != ReasonExit || ex.Code != 0 {
		t.Fatalf("Exit %+v", ex)
	}
	if st.State != BoxExited {
		t.Fatalf("box state %s", st.State)
	}
	if msgs := rec.messages("main"); len(msgs) == 0 || !strings.Contains(msgs[0], "Hello from the box! Script: main") {
		t.Fatalf("Logs: %q", msgs)
	}
}

func TestExitCodesAndErrors(t *testing.T) {
	e, rec := newEngine(t)
	cases := []struct {
		entry  string
		reason ExitReason
		code   int
	}{
		{"scripts/exit3.ts", ReasonCrash, 3},
		{"scripts/throw.ts", ReasonCrash, 1},
		{"scripts/later.ts", ReasonExit, 0}, // ends by itself despite open stdin (spike g)
		{"scripts/call.ts", ReasonExit, 0},
		{"scripts/stdout.ts", ReasonExit, 0},
	}
	for _, c := range cases {
		t.Run(c.entry, func(t *testing.T) {
			ex := lastExit(t, runToEnd(t, e, one(t, c.entry, nil), time.Minute))
			if ex.Reason != c.reason || ex.Code != c.code {
				t.Fatalf("exit %+v, expected %s/%d", ex, c.reason, c.code)
			}
		})
	}
	all := strings.Join(rec.messages(""), "\n")
	for _, want := range []string{"later", "CALL not_found", "A-writeSync", "B-write", "C-node", "on purpose"} {
		if !strings.Contains(all, want) {
			t.Errorf("log missing: %q", want)
		}
	}
}

// TestEscape: invariant I4 – the script may use neither env, file system, network, processes, FFI nor sys,
// not even via node: modules, dynamic imports or workers.
func TestEscape(t *testing.T) {
	e, rec := newEngine(t)
	ex := lastExit(t, runToEnd(t, e, one(t, "attack/escape.ts", nil), 2*time.Minute))
	if ex.Reason != ReasonExit {
		t.Fatalf("Exit %+v", ex)
	}
	n := 0
	for _, m := range rec.messages("main") {
		if strings.HasPrefix(m, "PROBE ") {
			n++
			if strings.HasSuffix(m, " ALLOWED") {
				t.Errorf("escape possible: %s", m)
			}
		}
	}
	if n < 15 || !slices.Contains(rec.messages("main"), "PROBES DONE") {
		t.Fatalf("probes incomplete (%d): %q", n, rec.messages("main"))
	}
}

// TestLimits: invariant I6 per script.
func TestLimits(t *testing.T) {
	e, _ := newEngine(t)
	cases := []struct {
		entry  string
		limits ScriptLimits
		box    Limits
		want   ExitReason
	}{
		{"attack/rss.ts", ScriptLimits{RSS: 64 << 20}, Limits{Memory: 512 << 20}, ReasonRSS},
		{"attack/busy.ts", ScriptLimits{Watchdog: 2 * time.Second}, Limits{}, ReasonWatchdog},
		{"attack/sleep.ts", ScriptLimits{Timeout: 2 * time.Second}, Limits{}, ReasonTimeout},
		// Watchdog off: shortly before the heap limit the GC blocks the event loop, under load longer than 10 s.
		{"attack/heap.ts", ScriptLimits{Heap: 32 << 20, Watchdog: -1}, Limits{Memory: 512 << 20}, ReasonHeap},
		{"attack/rawstdout.ts", ScriptLimits{}, Limits{}, ReasonProtocol},
	}
	for _, c := range cases {
		t.Run(c.entry, func(t *testing.T) {
			if c.want == ReasonRSS && noMeasurement() {
				t.Skip("no RSS measurement with the dev driver on macOS")
			}
			spec := one(t, c.entry, func(s *BoxSpec) { s.Limits = c.box; s.Scripts[0].Limits = c.limits })
			ex := lastExit(t, runToEnd(t, e, spec, 90*time.Second))
			if ex.Reason != c.want {
				t.Fatalf("reason %s (%+v), expected %s", ex.Reason, ex, c.want)
			}
		})
	}
}

// TestThreads: with RLIMIT_NPROC (gVisor) creating workers fails catchably; in the dev driver (Linux) init
// ends the script. On Windows init does not count threads.
func TestThreads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("threads per script are not enforced on Windows")
	}
	if noMeasurement() {
		t.Skip("no thread measurement with the dev driver on macOS")
	}
	e, rec := newEngine(t)
	spec := one(t, "attack/threads.ts", func(s *BoxSpec) {
		s.Limits.Memory = 1 << 30
		s.Scripts[0].Limits.Threads = 16
	})
	ex := lastExit(t, runToEnd(t, e, spec, 2*time.Minute))
	logs := strings.Join(rec.messages("main"), "\n")
	switch {
	case ex.Reason == ReasonThreads:
	case ex.Reason == ReasonExit && strings.Contains(logs, "WORKERS") && !strings.Contains(logs, "WORKERS 200"):
	default:
		t.Fatalf("thread limit does not apply: %+v\n%s", ex, logs)
	}
}

// TestBoxOOM: box memory full ⇒ reason oom (dev: the whole box; gVisor: the largest script).
func TestBoxOOM(t *testing.T) {
	if noMeasurement() {
		t.Skip("no RAM watchdog with the dev driver on macOS")
	}
	e, rec := newEngine(t)
	spec := one(t, "attack/rss.ts", func(s *BoxSpec) { s.Limits.Memory = 192 << 20 })
	ex := lastExit(t, runToEnd(t, e, spec, 90*time.Second))
	if ex.Reason != ReasonOOM || !rec.eventually(EventOOM) {
		t.Fatalf("reason %+v, OOM event %v", ex, rec.has(EventOOM))
	}
}

func TestRestartAndBreaker(t *testing.T) {
	e, rec := newEngine(t)
	spec := one(t, "scripts/crash.ts", func(s *BoxSpec) {
		s.Restart = RestartPolicy{Mode: RestartOnFailure, MaxFailures: 3, BackoffMin: 50 * time.Millisecond}
	})
	st := runToEnd(t, e, spec, 2*time.Minute)
	sc := st.Scripts[0]
	if sc.State != ScriptBroken || sc.Restarts != 2 || !rec.eventually(EventCircuitOpen) {
		t.Fatalf("Breaker: %+v circuit=%v", sc, rec.has(EventCircuitOpen))
	}
	if n := strings.Count(strings.Join(rec.messages("main"), "\n"), "start"); n != 3 {
		t.Fatalf("%d runs, expected 3", n)
	}
}

// TestIntegrity: an artifact changed after the build does not start (I9) and is not restarted.
func TestIntegrity(t *testing.T) {
	e, rec := newEngine(t)
	spec := one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Restart.Mode = RestartAlways })
	b, err := e.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	p := b.scripts["main"].artifact.Path
	_ = os.Chmod(p, 0o644)
	if err := os.WriteFile(p, []byte("console.log('manipuliert')"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := b.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ex := lastExit(t, st); ex.Reason != ReasonIntegrity || st.Scripts[0].State != ScriptBroken {
		t.Fatalf("%+v", st.Scripts[0])
	}
	if !rec.eventually(EventIntegrityFailed) || slices.Contains(rec.messages(""), "manipuliert") {
		t.Fatal("tampered artifact ran or no event")
	}
}

// TestIdentityAndStop: two scripts, logs belong to the right script; Stop ends both cleanly.
func TestIdentityAndStop(t *testing.T) {
	e, rec := newEngine(t)
	spec := BoxSpec{Name: boxFor(t), Scripts: []ScriptSpec{
		{Name: "alpha", Entry: "testdata/scripts/service.ts"},
		{Name: "beta", Entry: "testdata/scripts/service.ts"},
	}}
	b, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		st := b.Status()
		if st.Scripts[0].State == ScriptReady && st.Scripts[1].State == ScriptReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// service.ts logs after ready(), and logs arrive asynchronously: wait briefly for both lines.
	for wait := time.Now(); len(rec.messages("alpha")) < 1 || len(rec.messages("beta")) < 1; time.Sleep(50 * time.Millisecond) {
		if time.Since(wait) > 5*time.Second {
			break
		}
	}
	if len(rec.messages("alpha")) != 1 || len(rec.messages("beta")) != 1 {
		t.Fatalf("logs assigned to the wrong script: %+v", rec.logs)
	}
	stats, err := b.Stats(context.Background())
	if err != nil || len(stats.Scripts) != 2 {
		t.Fatalf("Stats: %v %+v", err, stats)
	}
	// init sends the first measurement right after the start (not only after up to 5 s); macOS dev measures nothing.
	for measured := time.Now(); !noMeasurement(); time.Sleep(100 * time.Millisecond) {
		stats, _ = b.Stats(context.Background())
		if s := stats.Scripts; s[0].RSS+int64(s[0].Threads) > 0 && s[1].RSS+int64(s[1].Threads) > 0 {
			break
		}
		if time.Since(measured) > 2*time.Second {
			t.Fatalf("2 s after ready still no measurements: %+v", stats.Scripts)
		}
	}
	if err := b.Stop(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	st := b.Status()
	if st.State != BoxExited {
		t.Fatalf("Box %s (%s)", st.State, st.Error)
	}
	for _, s := range st.Scripts {
		if s.LastExit == nil || (s.LastExit.Reason != ReasonStopped && s.LastExit.Reason != ReasonBoxShutdown) {
			t.Fatalf("%s: %+v", s.Name, s.LastExit)
		}
	}
}

// TestSiblingsAndNetwork (gVisor, Firecracker): a simulated escaped script process (native code, its own
// UID) reaches neither siblings (I11) nor init nor the network (I3) and finds no writable path (I8). In the
// Firecracker guest /proc has hidepid=2: the attacker does not even see its siblings.
func TestSiblingsAndNetwork(t *testing.T) {
	needIsolation(t)
	e, rec := newEngine(t)
	spec := BoxSpec{Name: boxFor(t), Scripts: []ScriptSpec{
		{Name: "victim", Entry: "testdata/scripts/service.ts"},
		{Name: "attacker", debugExec: []string{"/.tsbox/tsbox", "__probe"}},
	}}
	b, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background(), time.Second)
	deadline := time.Now().Add(time.Minute)
	for !slices.Contains(rec.messages("attacker"), "PROBE done DENIED") {
		if time.Now().After(deadline) {
			t.Fatalf("probe does not end: %q", rec.messages("attacker"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	probes := rec.messages("attacker")
	victims := 0
	for _, m := range probes {
		if strings.HasSuffix(m, " ALLOWED") {
			t.Errorf("allowed: %s", m)
		}
		if strings.HasPrefix(m, "PROBE ptrace-") {
			victims++
		}
	}
	if victims == 0 && driverName() != "firecracker" {
		t.Fatalf("no sibling process found: %q", probes)
	}
}

// eventually waits briefly for an event (events are delivered asynchronously).
func (r *recorder) eventually(kind EventKind) bool {
	for i := 0; i < 40; i++ {
		if r.has(kind) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestProfileNames: profile names are checked; the dev driver ignores profiles (the status shows them).
func TestProfileNames(t *testing.T) {
	e, _ := newEngine(t)
	if _, err := e.Check(context.Background(), one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Profile = "../rootfs" })); err == nil ||
		!strings.Contains(err.Error(), "profile name") {
		t.Fatalf("invalid profile name: %v", err)
	}
	if driverName() != "dev" {
		return
	}
	st := runToEnd(t, e, one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Profile = "anything" }), time.Minute)
	if st.Profile != "anything" || st.ProfileHash != "" || lastExit(t, st).Reason != ReasonExit {
		t.Fatalf("%+v", st)
	}
}

// TestStartCPU: the CPU boost at start ends as soon as all scripts are ready, otherwise after the deadline;
// under gVisor the actual limit is back in cpu.max afterwards.
func TestStartCPU(t *testing.T) {
	e, _ := newEngine(t)
	if _, err := e.Check(context.Background(), one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Limits.CPU, s.Limits.StartCPU = 1, 0.5 })); err == nil {
		t.Fatal("StartCPU below CPU accepted")
	}
	ended := make(chan string, 4)
	onStartBoostEnd = func(box string, err error) {
		if err != nil {
			t.Errorf("%s: %v", box, err)
		}
		ended <- box
	}
	oldMax := startBoostMax
	startBoostMax = 3 * time.Second
	t.Cleanup(func() { onStartBoostEnd, startBoostMax = nil, oldMax })

	for _, c := range []struct {
		entry    string
		min, max time.Duration // time until the end of the boost
	}{{"scripts/service.ts", 0, 2500 * time.Millisecond}, {"scripts/noready.ts", 2500 * time.Millisecond, 10 * time.Second}} {
		spec := one(t, c.entry, func(s *BoxSpec) { s.Limits.CPU, s.Limits.StartCPU = 0.5, 2 })
		b, err := e.Run(context.Background(), spec) // builds first; measured from the start of the scripts
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		select {
		case box := <-ended:
			if d := time.Since(t0); box != spec.Name || d < c.min || d > c.max {
				t.Errorf("%s: boost ended after %v (%s), expected %v–%v", c.entry, d, box, c.min, c.max)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: boost does not end", c.entry)
		}
		if isolatedDriver() {
			if v, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup/tsbox", spec.Name, "cpu.max")); strings.TrimSpace(string(v)) != "50000 100000" {
				t.Errorf("cpu.max after the start: %q", v)
			}
		}
		if st := b.Status(); st.State != BoxRunning {
			t.Errorf("%s: Box %s (%s)", c.entry, st.State, st.Error)
		}
		_ = b.Stop(context.Background(), time.Second)
	}
}
