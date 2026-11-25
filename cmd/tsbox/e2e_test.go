package main

// E2E tests for tsboxd and the CLI: real binary, real daemon (autostart), real Deno processes. Driver via
// TSBOX_TEST_DRIVER (default dev). Every test has its own state directory and therefore its own
// daemon.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/daemon"
	"github.com/rif223/tsbox/internal/denobin"
)

var (
	testBin  string
	testDeno string
	repo     string
)

func TestMain(m *testing.M) {
	code := func() int {
		flag.Parse()
		if fz := flag.Lookup("test.fuzz"); fz != nil && fz.Value.String() != "" {
			return m.Run() // fuzzing (parsers of the CLI) needs no binary
		}
		d, err := denobin.Find()
		if err != nil {
			fmt.Println("E2E skipped:", err)
			return m.Run()
		}
		testDeno = d
		repo, _ = filepath.Abs("../..")
		dir, err := os.MkdirTemp("", "tsbox-cli-")
		if err != nil {
			panic(err)
		}
		defer removeTemp(dir)
		testBin = filepath.Join(dir, "tsbox")
		if runtime.GOOS == "windows" {
			testBin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", testBin, ".")
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

func driverName() string {
	if d := os.Getenv("TSBOX_TEST_DRIVER"); d != "" {
		return d
	}
	return "dev"
}

// cli is a CLI environment with its own state and its own daemon.
type cli struct {
	t     *testing.T
	state string
	env   []string
}

func newCLI(t *testing.T, extra ...string) *cli {
	t.Helper()
	if testDeno == "" {
		t.Skip("no Deno")
	}
	// Own directory instead of t.TempDir: runsc mounts <state>/gvisor/runsc/null-netns as a bind mount that
	// RemoveAll cannot delete.
	state, err := os.MkdirTemp("", "tsbox-cli-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			// Diagnostics: tsboxd writes its goroutines to the log after exiting hangs for 30 s.
			time.Sleep(35 * time.Second)
			if b, err := os.ReadFile(filepath.Join(state, "tsboxd.log")); err == nil {
				t.Logf("tsboxd.log:\n%s", b[max(0, len(b)-64<<10):])
			}
		}
		if runtime.GOOS == "linux" {
			_ = exec.Command("umount", "-l", filepath.Join(state, "gvisor", "runsc", "null-netns")).Run()
		}
		removeTemp(state)
	})
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "TSBOX_STATE", "TSBOX_HOST", "TSBOX_NO_AUTOSTART", "TSBOX_IDLE":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "TSBOX_STATE="+state, "TSBOX_IDLE=2m", "TSBOX_DENO="+testDeno)
	env = append(env, extra...)
	c := &cli{t: t, state: state, env: env}
	t.Cleanup(c.shutdown)
	return c
}

func (c *cli) client() *client.Client {
	cl, err := client.New(client.UserHost(c.state))
	if err != nil {
		c.t.Fatal(err)
	}
	return cl
}

// shutdown ends this test's daemon and waits until it is gone.
func (c *cli) shutdown() {
	cl := c.client()
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cl.Shutdown(ctx); err != nil {
		c.t.Logf("shutdown: %v", err)
		return
	}
	for ctx.Err() == nil {
		ictx, icancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := cl.Info(ictx)
		timeout := ictx.Err() != nil
		icancel()
		if err != nil && !timeout { // pipe or socket gone; a timeout under load does not count
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // release the lock and files (Windows)
}

// cmd builds a CLI call. "run" without --driver gets the tested driver (a box needs one; see TestRunDriver).
func (c *cli) cmd(args ...string) *exec.Cmd {
	if len(args) > 0 && args[0] == "run" && !slices.Contains(args, "--driver") && !slices.Contains(args, "--no-driver") {
		args = append([]string{"run", "--driver", driverName()}, args[1:]...)
	} else if len(args) > 0 && args[0] == "run" {
		args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--no-driver" })
	}
	cmd := exec.Command(testBin, args...)
	cmd.Dir = repo
	cmd.Env = c.env
	return cmd
}

// run runs the CLI and returns stdout, stderr and the exit code.
func (c *cli) run(args ...string) (string, string, int) {
	c.t.Helper()
	cmd := c.cmd(args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(4 * time.Minute):
		_ = cmd.Process.Kill()
		c.t.Fatalf("tsbox %v hangs\nstdout: %s\nstderr: %s", args, out.String(), errb.String())
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

func (c *cli) ok(args ...string) string {
	c.t.Helper()
	out, errs, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("tsbox %v: Exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out
}

func (c *cli) ps(all bool) []client.Box {
	c.t.Helper()
	args := []string{"ps", "--json"}
	if all {
		args = append(args, "-a")
	}
	var boxes []client.Box
	if err := json.Unmarshal([]byte(c.ok(args...)), &boxes); err != nil {
		c.t.Fatal(err)
	}
	return boxes
}

func (c *cli) box(name string) (client.Box, bool) {
	for _, b := range c.ps(true) {
		if b.Name == name {
			return b, true
		}
	}
	return client.Box{}, false
}

// waitState waits until the box reaches one of the states.
func (c *cli) waitState(name string, states ...tsbox.BoxState) client.Box {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		b, ok := c.box(name)
		for _, s := range states {
			if ok && b.State == s {
				return b
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("box %s does not reach %v: %+v", name, states, b)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestRunExitCodes(t *testing.T) {
	c := newCLI(t)
	out, errs, code := c.run("run", "examples/hello.ts")
	if code != 0 || !strings.Contains(out, "Hello from the box") {
		t.Fatalf("hello: %d %q %q", code, out, errs)
	}
	if strings.Contains(errs, "NO isolation") != (driverName() == "dev") {
		t.Fatalf("isolation warning wrong: %q", errs)
	}
	if _, _, code := c.run("run", "-q", "testdata/scripts/exit3.ts"); code != 3 {
		t.Fatalf("exit3: %d", code)
	}
	if _, errs, code := c.run("run", "-q", "--watchdog", "2s", "testdata/attack/busy.ts"); code != 137 {
		t.Fatalf("busy: %d %s", code, errs)
	}
	// A limit end is reported in exactly one line.
	if _, errs, code := c.run("run", "--watchdog", "2s", "testdata/attack/busy.ts"); code != 137 ||
		strings.Count(errs, "ended: watchdog - ") != 1 {
		t.Fatalf("busy message: %d %q", code, errs)
	}
	pf := filepath.Join(t.TempDir(), "params.json")
	os.WriteFile(pf, []byte(`{"n":2}`), 0o644)
	if out, errs, code := c.run("run", "-q", "--params-file", pf, "testdata/scripts/ticker.ts"); code != 0 || !strings.Contains(out, "tick 2") {
		t.Fatalf("params-file: %d %q %q", code, out, errs)
	}
	if _, errs, code := c.run("run", "--params", "{n:2}", "testdata/scripts/ticker.ts"); code != 2 || !strings.Contains(errs, "not valid JSON: {n:2}") {
		t.Fatalf("bad params: %d %q", code, errs)
	}
	// Files from PowerShell 5.1: UTF-8 with BOM.
	os.WriteFile(pf, append([]byte{0xEF, 0xBB, 0xBF}, `{"n":1}`...), 0o644)
	if out, errs, code := c.run("run", "-q", "--params-file", pf, "testdata/scripts/ticker.ts"); code != 0 || !strings.Contains(out, "tick 1") {
		t.Fatalf("params-file with BOM: %d %q %q", code, out, errs)
	}
	// Wrong values are found before tsboxd is asked: exit 2, named by flag.
	if _, errs, code := c.run("run", "--cpu", "0.01", "examples/hello.ts"); code != 2 || !strings.Contains(errs, "--cpu must be between") {
		t.Fatalf("--cpu 0.01: %d %q", code, errs)
	}
	// Stack traces point to the source (inline source map), not to the bundle.
	if _, errs, _ := c.run("run", "-q", "testdata/scripts/throw.ts"); !strings.Contains(errs, "throw.ts:1:7") {
		t.Fatalf("stack trace without the source position: %q", errs)
	}
	bad := filepath.Join(t.TempDir(), "broken.ts")
	os.WriteFile(bad, []byte("const x: number = \"text\";\n"), 0o644)
	if _, errs, code := c.run("run", "-q", bad); code != 125 || !strings.Contains(errs, "TS2322") {
		t.Fatalf("type error: %d %s", code, errs)
	}
	// Foreground boxes are removed.
	if boxes := c.ps(true); len(boxes) != 0 {
		t.Fatalf("boxes left over: %+v", boxes)
	}
}

func TestLifecycle(t *testing.T) {
	c := newCLI(t)
	if out := c.ok("run", "-d", "--name", "tick", "testdata/scripts/ticker.ts", "--params", `{"n":100000}`); strings.TrimSpace(out) != "tick" {
		t.Fatalf("run -d: %q", out)
	}
	b := c.waitState("tick", tsbox.BoxRunning)
	if b.Desired != "running" || len(b.Scripts) != 1 {
		t.Fatalf("%+v", b)
	}
	if out := c.ok("ps"); !strings.Contains(out, "tick") || !strings.Contains(out, "running") {
		t.Fatalf("ps: %s", out)
	}
	time.Sleep(1500 * time.Millisecond)
	var stats []client.StatsEntry
	if err := json.Unmarshal([]byte(c.ok("stats", "--no-stream", "--json")), &stats); err != nil || len(stats) != 1 || stats[0].Box != "tick" {
		t.Fatalf("stats: %v %+v", err, stats)
	}
	if out := c.ok("stats", "--no-stream"); !strings.Contains(out, "RAM/LIMIT") || !strings.Contains(out, "main") {
		t.Fatalf("stats table: %s", out)
	}
	if out := c.ok("logs", "-n", "2", "tick"); len(strings.Split(strings.TrimSpace(out), "\n")) != 2 || !strings.Contains(out, "tick ") {
		t.Fatalf("logs -n 2: %q", out)
	}
	if out := c.ok("logs", "--since", "1h", "--script", "main", "-t", "tick"); !strings.Contains(out, "tick 1\n") {
		t.Fatalf("logs --since: %q", out[:min(len(out), 200)])
	}
	// logs -f follows until the box is stopped.
	f := c.cmd("logs", "-f", "-n", "1", "tick")
	var fout bytes.Buffer
	f.Stdout = &fout
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	c.ok("stop", "tick")
	fdone := make(chan error, 1)
	go func() { fdone <- f.Wait() }()
	select {
	case <-fdone:
	case <-time.After(30 * time.Second):
		f.Process.Kill()
		t.Fatal("logs -f does not end after stop")
	}
	if n := strings.Count(fout.String(), "tick "); n < 5 {
		t.Fatalf("logs -f: %d lines", n)
	}
	b = c.waitState("tick", tsbox.BoxExited)
	if b.Desired != "stopped" || b.Scripts[0].LastExit == nil || b.Scripts[0].LastExit.Reason != tsbox.ReasonStopped {
		t.Fatalf("after stop: %+v", b)
	}
	c.ok("start", "tick")
	c.waitState("tick", tsbox.BoxRunning)
	c.ok("restart", "tick")
	c.waitState("tick", tsbox.BoxRunning)
	if _, errs, code := c.run("rm", "tick"); code == 0 || !strings.Contains(errs, "is running") {
		t.Fatalf("rm without -f: %d %s", code, errs)
	}
	var insp []client.Inspect
	if err := json.Unmarshal([]byte(c.ok("inspect", "tick")), &insp); err != nil || len(insp) != 1 || insp[0].Spec.Name != "tick" {
		t.Fatalf("inspect: %v", err)
	}
	if _, err := os.Stat(filepath.Join(insp[0].LogDir, "main.log")); err != nil {
		t.Fatalf("log file: %v", err)
	}
	c.ok("rm", "-f", "tick")
	if _, ok := c.box("tick"); ok {
		t.Fatal("box still there after rm")
	}
	if _, err := os.Stat(insp[0].Dir); !os.IsNotExist(err) {
		t.Fatalf("box directory after rm: %v", err)
	}
	if _, errs, code := c.run("logs", "tick"); code == 0 || !strings.Contains(errs, "not found") {
		t.Fatalf("logs after rm: %d %s", code, errs)
	}
	// events: from the ring buffer, including daemon events.
	ev := c.cmd("events", "--since", "1h", "--json")
	var evout bytes.Buffer
	ev.Stdout = &evout
	ev.Start()
	time.Sleep(1500 * time.Millisecond)
	ev.Process.Kill()
	ev.Wait()
	for _, kind := range []string{"box_created", "box_started", "script_ready", "script_exited", "box_removed"} {
		if !strings.Contains(evout.String(), `"kind":"`+kind+`"`) {
			t.Errorf("events: %s missing", kind)
		}
	}
}

// TestSecretsNotPersisted: env: secrets reach the provider, but never appear in the state, in inspect, in
// logs or in events (I2 for tsboxd).
func TestSecretsNotPersisted(t *testing.T) {
	const canary = "canary-7c41b9e2d05a"
	c := newCLI(t, "TSBOX_CANARY="+canary)
	c.ok("sdk", "add", "testdata/sdk-secret")
	c.ok("run", "-d", "--name", "sec", "testdata/scripts/secretlen.ts", "--sdk", "secretlen", "--sdk-config", "secretlen:token=env:TSBOX_CANARY")
	deadline := time.Now().Add(2 * time.Minute)
	for !strings.Contains(c.ok("logs", "sec"), fmt.Sprintf("secret-len %d", len(canary))) {
		if time.Now().After(deadline) {
			t.Fatal("the provider did not get the secret")
		}
		time.Sleep(300 * time.Millisecond)
	}
	insp := c.ok("inspect", "sec")
	if strings.Contains(insp, canary) || !strings.Contains(insp, "env:TSBOX_CANARY") || !strings.Contains(insp, `"***"`) {
		t.Fatalf("inspect: %s", insp)
	}
	// Daemon restart: the value is gone, the box only starts again with the caller's environment.
	c.shutdown()
	ps := c.ok("ps", "-a")
	if !strings.Contains(ps, "sec") {
		t.Fatalf("ps after restart: %s", ps)
	}
	noEnv := &cli{t: t, state: c.state, env: append([]string{}, c.env[:len(c.env)-1]...)}
	if _, errs, code := noEnv.run("start", "sec"); code == 0 || !strings.Contains(errs, "TSBOX_CANARY") {
		t.Fatalf("start without secret: %d %s", code, errs)
	}
	c.ok("start", "sec")
	c.waitState("sec", tsbox.BoxRunning)
	c.ok("stop", "sec")
	ev := c.cmd("events", "--since", "1h", "--json")
	var evout bytes.Buffer
	ev.Stdout = &evout
	ev.Start()
	time.Sleep(time.Second)
	ev.Process.Kill()
	ev.Wait()
	if strings.Contains(evout.String(), canary) {
		t.Fatal("secret in events")
	}
	err := filepath.Walk(c.state, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Size() > 64<<20 {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr == nil && bytes.Contains(b, []byte(canary)) {
			return fmt.Errorf("secret in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRestore: boxes with a restart policy run again after a restart of the daemon, others do not.
func TestRestore(t *testing.T) {
	c := newCLI(t)
	c.ok("run", "-d", "--name", "service", "--restart", "always", "testdata/scripts/service.ts")
	c.ok("run", "-d", "--name", "once", "testdata/scripts/service.ts")
	c.waitState("service", tsbox.BoxRunning)
	c.waitState("once", tsbox.BoxRunning)
	c.shutdown()
	b := c.waitState("service", tsbox.BoxRunning)
	if !b.Loaded {
		t.Fatalf("%+v", b)
	}
	e, _ := c.box("once")
	if e.State != tsbox.BoxExited || e.Loaded {
		t.Fatalf("once: %+v", e)
	}
	c.ok("start", "once")
	c.waitState("once", tsbox.BoxRunning)
}

// TestAutostartIdleAndPerms: autostart, permissions of the socket or pipe, exit after idle time,
// TSBOX_NO_AUTOSTART.
func TestAutostartIdleAndPerms(t *testing.T) {
	c := newCLI(t, "TSBOX_IDLE=3s")
	off := &cli{t: t, state: c.state, env: append(append([]string{}, c.env...), "TSBOX_NO_AUTOSTART=1")}
	if _, errs, code := off.run("ps"); code == 0 || !strings.Contains(errs, "autostart is off") {
		t.Fatalf("without autostart: %d %s", code, errs)
	}
	c.ok("run", "-q", "examples/hello.ts")
	cl := c.client()
	defer cl.Close()
	ctx := context.Background()
	info, err := cl.Info(ctx)
	if err != nil || !info.Autostarted || info.Idle != "3s" {
		t.Fatalf("Info: %+v %v", info, err)
	}
	sock, _ := client.SocketPath(client.UserHost(c.state))
	perm, err := daemon.SocketPerm(sock)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		sid, _ := client.CurrentUserSID()
		// SDDL writes some well-known accounts as aliases, e.g. the built-in administrator (RID 500, the
		// user of the GitHub runners) as "LA".
		alias := map[bool]string{true: "LA"}[strings.HasSuffix(sid, "-500")]
		if perm != "D:P(A;;FA;;;"+sid+")" && (alias == "" || perm != "D:P(A;;FA;;;"+alias+")") {
			t.Fatalf("DACL of the pipe: %s", perm)
		}
	} else if !strings.HasPrefix(perm, "0600 ") {
		t.Fatalf("socket permissions: %s", perm)
	}
	cl.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := cl.Info(ctx); err != nil {
			break
		}
		cl.Close()
		if time.Now().After(deadline) {
			t.Fatal("daemon does not exit after idle time")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if out := c.ok("doctor"); !strings.Contains(out, "isolation available") {
		t.Fatalf("doctor: %s", out)
	}
}

func TestUpDown(t *testing.T) {
	c := newCLI(t)
	dir := t.TempDir()
	script := filepath.Join(repo, "testdata", "scripts", "service.ts")
	src, _ := os.ReadFile(script)
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "a.ts"), src, 0o644)
	os.WriteFile(filepath.Join(dir, "src", "b.ts"), src, 0o644)
	os.MkdirAll(filepath.Join(dir, "other"), 0o755)
	os.WriteFile(filepath.Join(dir, "other", "a.ts"), src, 0o644)
	proj := `{
  "name": "demo",
  "boxes": {
    "one": { "limits": { "memory": "256Mi" }, "restart": "on-failure:3",
              "driver": "%[1]s",
              "scripts": { "a": { "entry": "src/a.ts" }, "b": { "entry": "src/b.ts", "limits": { "rss": "96Mi" } } } },
    "two": { "driver": "%[1]s", "sdks": { "kv": {} }, "scripts": { "main": { "entry": "other/a.ts" } } }
  }
}`
	file := filepath.Join(dir, "tsbox.json")
	os.WriteFile(file, []byte(fmt.Sprintf(proj, driverName())), 0o644)
	out := c.ok("up", "-d", "-f", file)
	if strings.Count(out, "created and started") != 2 {
		t.Fatalf("up: %s", out)
	}
	b := c.waitState("demo-one", tsbox.BoxRunning)
	if b.Labels[client.LabelProject] != "demo" || len(b.Scripts) != 2 || b.Limits.Memory != 256<<20 {
		t.Fatalf("%+v", b)
	}
	c.waitState("demo-two", tsbox.BoxRunning)
	if out := c.ok("up", "-d", "-f", file); strings.Count(out, "up to date") != 2 {
		t.Fatalf("up without change: %s", out)
	}
	os.WriteFile(filepath.Join(dir, "src", "b.ts"), append(src, []byte("\n// changed\n")...), 0o644)
	if out := c.ok("up", "-d", "-f", file); !strings.Contains(out, "demo-one: changed") || !strings.Contains(out, "demo-two: up to date") {
		t.Fatalf("up after change: %s", out)
	}
	c.waitState("demo-one", tsbox.BoxRunning)
	// Another directory with the same project name must neither take over nor remove these boxes.
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "tsbox.json"), []byte(`{"name":"demo","boxes":{"one":{"driver":"dev","scripts":{"a":{"entry":"a.ts"}}}}}`), 0o644)
	os.WriteFile(filepath.Join(dir2, "a.ts"), src, 0o644)
	file2 := filepath.Join(dir2, "tsbox.json")
	for _, args := range [][]string{{"up", "-d", "-f", file2}, {"down", "-f", file2}} {
		if _, errs, code := c.run(args...); code != 1 || !strings.Contains(errs, "already used by") {
			t.Fatalf("%s of a foreign project: %d %s", args[0], code, errs)
		}
	}
	if out := c.ok("down", "-f", file); strings.Count(out, "removed") != 2 {
		t.Fatalf("down: %s", out)
	}
	if boxes := c.ps(true); len(boxes) != 0 {
		t.Fatalf("after down: %+v", boxes)
	}
}

// TestInterrupt: Ctrl+C (SIGINT) on tsbox run ⇒ exit 130, box stopped and removed. On Windows Ctrl+C cannot
// be sent to a specific child process.
func TestInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT to child processes does not exist on Windows")
	}
	c := newCLI(t)
	cmd := c.cmd("run", "--name", "interrupted", "testdata/scripts/service.ts")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for !strings.Contains(out.String(), "service running") {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("script does not start")
		}
		time.Sleep(200 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	if cmd.ProcessState.ExitCode() != 130 {
		t.Fatalf("Exit %d (%v)", cmd.ProcessState.ExitCode(), err)
	}
	for i := 0; i < 50; i++ {
		if _, ok := c.box("interrupted"); !ok {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("box not removed after Ctrl+C")
}

// TestSDKLifecycle: an SDK from "sdk init" works without --grant, cannot be removed while a box uses it and
// can be removed and reinstalled afterwards even though tsboxd ran its provider (Windows: open files).
func TestSDKLifecycle(t *testing.T) {
	c := newCLI(t)
	dir := t.TempDir()
	c.ok("sdk", "init", "greeter", "--dir", dir)
	sdkDir := filepath.Join(dir, "greeter")
	c.ok("sdk", "add", sdkDir)
	if out, errs, code := c.run("run", "-q", filepath.Join(sdkDir, "example.ts"), "--sdk", "greeter"); code != 0 || !strings.Contains(out, "Hello, World!") {
		t.Fatalf("run without --grant: %d %q %q", code, out, errs)
	}
	c.ok("run", "-d", "--name", "g", "testdata/scripts/service.ts", "--sdk", "greeter")
	c.waitState("g", tsbox.BoxRunning)
	if _, errs, code := c.run("sdk", "rm", "greeter"); code == 0 || !strings.Contains(errs, "used by running boxes: g") {
		t.Fatalf("sdk rm while in use: %d %q", code, errs)
	}
	c.ok("rm", "-f", "g")
	c.ok("sdk", "add", sdkDir) // replace while tsboxd still knows the provider
	c.ok("sdk", "rm", "greeter")
	if out := c.ok("sdk", "ls"); strings.Contains(out, "greeter") {
		t.Fatalf("sdk ls after rm: %s", out)
	}
	if _, errs, code := c.run("sdk", "rm", "kv"); code == 0 || !strings.Contains(errs, "bundled") {
		t.Fatalf("sdk rm kv: %d %q", code, errs)
	}
}

// TestRunDriver: --driver picks the box's driver and is required, the daemon lists its drivers, and a stored
// box keeps its driver over a daemon restart.
func TestRunDriver(t *testing.T) {
	c := newCLI(t)
	c.ok("run", "-d", "--name", "pick", "--driver", "dev", "--restart", "always", "testdata/scripts/service.ts")
	if b := c.waitState("pick", tsbox.BoxRunning); b.Driver != "dev" {
		t.Fatalf("box: %+v", b)
	}
	info, err := c.client().Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var dev bool
	for _, d := range info.Drivers {
		dev = dev || (d.Name == "dev" && d.Available)
	}
	if !dev {
		t.Fatalf("drivers: %+v", info.Drivers)
	}
	if raw, err := os.ReadFile(filepath.Join(c.state, "boxes", "pick", "tsboxd.json")); err != nil || !strings.Contains(string(raw), `"driver": "dev"`) {
		t.Fatalf("stored spec without driver: %v %s", err, raw)
	}
	c.shutdown()
	if b := c.waitState("pick", tsbox.BoxRunning); b.Driver != "dev" {
		t.Fatalf("after restart: %+v", b)
	}
	// A box stored by tsbox 0.2 without the driver in its specification gets the driver of its last run.
	c.shutdown()
	stored := filepath.Join(c.state, "boxes", "pick", "tsboxd.json")
	raw, err := os.ReadFile(stored)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	delete(p["spec"].(map[string]any), "driver")
	raw, _ = json.Marshal(p)
	if err := os.WriteFile(stored, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if b := c.waitState("pick", tsbox.BoxRunning); b.Driver != "dev" {
		t.Fatalf("stored without driver: %+v", b)
	}
	if runtime.GOOS != "linux" {
		_, stderr, code := c.run("run", "-d", "--name", "nope", "--driver", "gvisor", "testdata/scripts/service.ts")
		if code == 0 || !strings.Contains(stderr, "gvisor") {
			t.Fatalf("gvisor on %s: %d %s", runtime.GOOS, code, stderr)
		}
	}
	if _, stderr, code := c.run("run", "--driver", "docker", "testdata/scripts/service.ts"); code != 2 || !strings.Contains(stderr, "docker") {
		t.Fatalf("unknown driver: %d %s", code, stderr)
	}
	// There is no default driver: without --driver (or with the old "auto") a box is refused.
	if _, stderr, code := c.run("run", "--no-driver", "testdata/scripts/service.ts"); code != 2 || !strings.Contains(stderr, "--driver is required") {
		t.Fatalf("without driver: %d %s", code, stderr)
	}
	if _, stderr, code := c.run("run", "--driver", "auto", "testdata/scripts/service.ts"); code != 2 || !strings.Contains(stderr, "auto") {
		t.Fatalf("auto: %d %s", code, stderr)
	}
	// A project box without "driver" is refused before anything starts.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tsbox.json"), []byte(`{"name":"nodrv","boxes":{"one":{"scripts":{"a":{"entry":"a.ts"}}}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "a.ts"), []byte("console.log(1)"), 0o644)
	if _, stderr, code := c.run("up", "-d", "-f", filepath.Join(dir, "tsbox.json")); code == 0 || !strings.Contains(stderr, `"driver" is missing`) {
		t.Fatalf("project without driver: %d %s", code, stderr)
	}
}

// TestDaemonStart: "tsbox daemon start" starts a permanent daemon (no idle exit, not replaced by another
// build) and leaves a running one alone; "tsbox setup --remove" without a service changes nothing.
func TestDaemonStart(t *testing.T) {
	c := newCLI(t)
	if out := c.ok("daemon", "start"); !strings.Contains(out, "started") {
		t.Fatalf("start: %s", out)
	}
	info, err := c.client().Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Autostarted || info.Idle != "" {
		t.Fatalf("not permanent: %+v", info)
	}
	if out := c.ok("daemon", "start"); !strings.Contains(out, "running") {
		t.Fatalf("second start: %s", out)
	}
	if again, _ := c.client().Info(context.Background()); again.PID != info.PID {
		t.Fatalf("restarted: %d -> %d", info.PID, again.PID)
	}
	c.ok("run", "-d", "--name", "svc", "testdata/scripts/service.ts")
	c.waitState("svc", tsbox.BoxRunning)
	c.ok("daemon", "stop", "-f")
}
