// Command tsbox runs TypeScript code in isolation.
//
//	tsbox run hello.ts
//	tsbox run bot.ts --sdk kv --sdk http --grant http:host=api.github.com --mem 128Mi --cpu 0.5
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/build"
	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/internal/project"
	"github.com/rif223/tsbox/internal/sdkreg"
	"github.com/rif223/tsbox/manifest"
)

func main() {
	tsbox.Init() // roles in the box (__init, __exec, __probe)
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "run":
		code = run(os.Args[2:])
	case "check":
		code = check(os.Args[2:])
	case "types":
		code = types(os.Args[2:])
	case "sdk":
		code = sdkCmd(os.Args[2:])
	case "ps":
		code = psCmd(os.Args[2:])
	case "logs":
		code = logsCmd(os.Args[2:])
	case "stats":
		code = statsCmd(os.Args[2:])
	case "stop", "start", "restart", "rm":
		code = lifecycleCmd(os.Args[1], os.Args[2:])
	case "inspect":
		code = inspectCmd(os.Args[2:])
	case "events":
		code = eventsCmd(os.Args[2:])
	case "up":
		code = upCmd(os.Args[2:])
	case "down":
		code = downCmd(os.Args[2:])
	case "daemon":
		code = daemonCmd(os.Args[2:])
	case "doctor":
		code = doctorCmd(os.Args[2:])
	case "setup":
		code = setupCmd(os.Args[2:])
	case "version", "--version", "-v":
		if len(os.Args) > 2 {
			fmt.Fprintf(os.Stderr, "tsbox version: takes no arguments (got %q)\n", strings.Join(os.Args[2:], " "))
			os.Exit(2)
		}
		fmt.Println("tsbox", tsbox.Version, "(SDK-API", manifest.APIVersion+")")
	case "help", "-h", "--help":
		if len(os.Args) > 2 && os.Args[1] == "help" { // "tsbox help run" = "tsbox run -h"
			cmd := os.Args[2]
			switch {
			case !slices.Contains(commands, cmd):
				fmt.Fprintf(os.Stderr, "tsbox help: unknown command %q\nRun \"tsbox help\" to see all commands.\n", cmd)
				os.Exit(2)
			case cmd != "help" && cmd != "version":
				os.Args = []string{os.Args[0], cmd, "-h"}
				main()
				return
			}
		}
		usage(os.Stdout)
	default:
		msg := fmt.Sprintf("tsbox: unknown command %q", os.Args[1])
		if sug := suggestCmd(os.Args[1]); sug != "" {
			msg += fmt.Sprintf(" (did you mean %q?)", sug)
		}
		fmt.Fprintln(os.Stderr, msg+"\nRun \"tsbox help\" to see all commands.")
		code = 2
	}
	if helpShown {
		code = 0
	}
	os.Exit(code)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `tsbox - run TypeScript in isolation

Commands:
  run <script.ts> [flags]        start a script in a new box (foreground; -d in the background)
  ps [-a] [--json]               list boxes
  logs [-f] [--since] <box>      logs of a box (from the log files)
  stats [--no-stream] [box]      live table: RAM, CPU, PIDs, restarts
  stop|start|restart <box>       stop, start, restart a box
  rm [-f] <box>                  remove a box
  inspect <box>                  specification and status (without secrets)
  events [--since]               follow events
  up [-d] / down                 start / remove a project from tsbox.json
  daemon [start|stop]            tsboxd in the foreground / start it in the background / end it
  doctor / setup [--remove]      check the environment / run tsboxd permanently (Linux: rootfs, systemd)
  check <script.ts> [--sdk ...]  type check and build with the given SDKs, without starting
  types [--sdk ...]              types for the editor (deno.json + .tsbox/types)
  sdk ls|info|add|rm|init|test   manage and develop SDKs
  version                        show the version

Examples (in the repository):
  tsbox run examples/hello.ts --mem 128Mi --cpu 0.5 --rss 96Mi --timeout 30s
  tsbox run examples/fetch.ts --sdk http --grant http:host=api.github.com
  tsbox run -d --name tick testdata/scripts/ticker.ts
  tsbox logs -f tick

"tsbox <command> -h" shows all flags.
`)
}

// multi is a repeatable flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// sdkFlags are --sdk, --grant and --sdk-config (run, check, types).
type sdkFlags struct{ sdks, grants, configs multi }

func (f *sdkFlags) register(fs *flag.FlagSet) {
	fs.Var(&f.sdks, "sdk", "activate an SDK (name or path; repeatable)")
	fs.Var(&f.grants, "grant", "grant a right: <sdk>:<right>=<value> (repeatable)")
	fs.Var(&f.configs, "sdk-config", "setting: <sdk>:<name>=<value>; secrets as file:<path> or env:<NAME>")
}

// uses builds the SDK list. Grants and config must name an activated SDK (the name from the manifest or the
// name under which it was activated).
func (f *sdkFlags) uses() ([]tsbox.SDKUse, error) {
	var out []tsbox.SDKUse
	idx := map[string]int{}
	for _, ref := range f.sdks {
		name := strings.TrimSuffix(filepath.Base(filepath.Clean(ref)), string(filepath.Separator))
		if m, err := manifest.Load(ref); err == nil {
			name = m.Name
		}
		if _, dup := idx[name]; dup {
			return nil, fmt.Errorf("--sdk %s given twice", name)
		}
		idx[name] = len(out)
		out = append(out, tsbox.SDKUse{Name: ref, Grants: map[string][]string{}, Config: map[string]string{}})
	}
	for _, g := range f.grants {
		sdk, perm, val, err := manifest.ParseGrant(g)
		if err != nil {
			return nil, err
		}
		i, ok := idx[sdk]
		if !ok {
			return nil, fmt.Errorf("--grant %s: SDK %q is not activated (--sdk %s)", g, sdk, sdk)
		}
		out[i].Grants[perm] = append(out[i].Grants[perm], val)
	}
	for _, c := range f.configs {
		sdk, key, val, err := manifest.ParseGrant(c)
		if err != nil {
			return nil, fmt.Errorf("--sdk-config %s: expected <sdk>:<name>=<value>", c)
		}
		i, ok := idx[sdk]
		if !ok {
			return nil, fmt.Errorf("--sdk-config %s: SDK %q is not activated (--sdk %s)", c, sdk, sdk)
		}
		out[i].Config[key] = val
	}
	return out, nil
}

func printBuildErr(err error) int {
	var be *build.Error
	if errors.As(err, &be) {
		fmt.Fprintln(os.Stderr, be.Error())
	} else {
		fmt.Fprintln(os.Stderr, "tsbox:", err)
	}
	return exitTsboxError
}

// Exit codes of tsbox run (like docker run): the script's code, otherwise:
const (
	exitTsboxError = 125 // build, start, configuration
	exitRefused    = 126 // integrity, protocol
	exitKilled     = 137 // limit exceeded, watchdog, OOM
	exitInterrupt  = 130
)

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	name := fs.String("name", "", "name of the box (default: from the file name)")
	mem := fs.String("mem", "128Mi", "RAM of the box, at least 32Mi (e.g. 128Mi, 1Gi)")
	cpu := fs.Float64("cpu", 0.5, "CPU cores of the box (0.1 to 64)")
	startCPU := fs.Float64("start-cpu", 0, "CPU cores at start until all scripts are ready (at most 30 s; 0 = no boost)")
	pids := fs.Int("pids", 64, "processes and threads of the box (8 to 4096)")
	heap := fs.String("heap", "64Mi", "V8 heap of the script")
	rss := fs.String("rss", "0", "private memory of the script (0 = only the box limit)")
	threads := fs.Int("threads", 32, "threads of the script")
	timeout := fs.Duration("timeout", 0, "run time limit (e.g. 30s; 0 = unlimited)")
	watchdog := fs.Duration("watchdog", 10*time.Second, "end a blocked event loop after (0 = off)")
	restart := fs.String("restart", "no", "restart policy: no | on-failure[:max] | always")
	drv := fs.String("driver", "auto", "driver of the box: auto (the daemon's default) | gvisor | firecracker | dev")
	detach := fs.Bool("d", false, "start in the background (tsboxd), print the box name")
	autoRm := fs.Bool("rm", false, "with -d: remove the box after it ends")
	rootfs := fs.String("rootfs", "", "not supported: the rootfs is set for tsboxd (TSBOX_ROOTFS or tsbox daemon --rootfs)")
	params := fs.String("params", "", "JSON parameters for the script (not secret)")
	paramsFile := fs.String("params-file", "", "file with the JSON parameters (instead of --params)")
	quiet := fs.Bool("q", false, "no status messages from tsbox (the warning about missing isolation stays)")
	storage := fs.String("storage", "", "storage area for kv (default: the script's file name)")
	prof := fs.String("profile", "", "profile (rootfs under gVisor, default: default; tsbox doctor lists them)")
	var sf sdkFlags
	sf.register(fs)
	usageFor(fs, "run <script.ts> [flags]")
	entry, ok := parseArgs(fs, args, true)
	if !ok {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "tsbox: "+format+"\n", a...)
		return exitTsboxError
	}
	bad := func(format string, a ...any) int { // wrong input, found before tsboxd is asked: exit 2
		fmt.Fprintf(os.Stderr, "tsbox run: "+format+"\n", a...)
		return 2
	}
	switch *drv {
	case "auto", "gvisor", "firecracker", "dev":
	default:
		return bad("--driver %q: allowed are auto, gvisor, firecracker and dev", *drv)
	}
	if *timeout < 0 {
		return bad("--timeout %s: must not be negative (0 = unlimited)", *timeout)
	}
	if *watchdog < 0 {
		return bad("--watchdog %s: must not be negative (0 = off)", *watchdog)
	}
	if st, err := os.Stat(entry); err == nil && st.IsDir() {
		return bad("%s is a directory, not a script", entry)
	} else if err != nil {
		return bad("%s", notFound(entry))
	}
	limits := tsbox.Limits{CPU: *cpu, PIDs: *pids, StartCPU: *startCPU}
	var sl tsbox.ScriptLimits
	var err error
	if limits.Memory, err = parseBytes(*mem); err != nil {
		return bad("--mem: %v", err)
	}
	if sl.Heap, err = parseBytes(*heap); err != nil {
		return bad("--heap: %v", err)
	}
	if sl.RSS, err = parseBytes(*rss); err != nil {
		return bad("--rss: %v", err)
	}
	sl.Threads, sl.Timeout, sl.Watchdog = *threads, *timeout, *watchdog
	if *watchdog == 0 {
		sl.Watchdog = -1
	}
	policy, err := parseRestart(*restart)
	if err != nil {
		return bad("--restart: %v", err)
	}
	if *paramsFile != "" {
		if *params != "" {
			return bad("--params and --params-file exclude each other")
		}
		b, err := fsutil.ReadText(*paramsFile)
		if errors.Is(err, fsutil.ErrNotUTF8) {
			return bad("--params-file: %s is %v", *paramsFile, err)
		} else if err != nil {
			return bad("--params-file: %v", err)
		}
		if !jsonValid(string(b)) {
			return bad("--params-file: %s is not valid JSON", *paramsFile)
		}
		*params = strings.TrimSpace(string(b))
	}
	var rawParams []byte
	if *params != "" {
		if !jsonValid(*params) {
			msg := fmt.Sprintf("--params is not valid JSON: %s", *params)
			if runtime.GOOS == "windows" && !strings.Contains(*params, `"`) {
				msg += "\n  (the shell removed the quotes: in PowerShell write '{\\\"n\\\":600}', in cmd \"{\\\"n\\\":600}\", or use --params-file)"
			}
			return bad("%s", msg)
		}
		rawParams = []byte(*params)
	}
	if *name == "" {
		*name = defaultName(entry)
	}
	if *storage == "" {
		*storage = baseName(entry)
	}
	uses, err := sf.uses()
	if err != nil {
		return bad("%v", err)
	}

	if entry, err = filepath.Abs(entry); err != nil {
		return fail("%v", err)
	}
	absRefs(uses)
	spec := tsbox.BoxSpec{
		Name: *name, Limits: limits, Restart: policy, SDKs: uses, Storage: *storage, Profile: *prof, Driver: *drv,
		Scripts: []tsbox.ScriptSpec{{Name: "main", Entry: entry, Limits: sl, Params: rawParams}},
	}
	if err := spec.Validate(); err != nil {
		return bad("%s", flagNames.Replace(err.Error()))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c, info, err := connect(ctx)
	if err != nil {
		return fail("%v", err)
	}
	defer c.Close()
	if err := checkDriver(info, *drv, *rootfs); err != nil {
		return fail("%v", err)
	}
	if len(info.Drivers) == 0 {
		spec.Driver = "" // older daemon: it rejects the field and has checked to be this driver anyway
	}
	if boxDriver(info, *drv) == "dev" {
		fmt.Fprintln(os.Stderr, "WARNING: driver \"dev\": NO isolation. Only for your own, trusted code.")
		if *prof != "" && *prof != "default" && !*quiet {
			fmt.Fprintf(os.Stderr, "tsbox: --profile %s has no effect with the driver dev (profiles are rootfs images for gVisor)\n", *prof)
		}
	}
	req := client.CreateRequest{Spec: spec, Start: true, AutoRemove: !*detach || *autoRm, Env: client.LookupEnv(spec)}
	if *detach {
		if _, err := c.Create(ctx, req); err != nil {
			return printAPIErr(err)
		}
		fmt.Println(spec.Name)
		return 0
	}
	var final *tsbox.BoxStatus
	exitShown := false // the last script end was already reported as an event
	// The first build (type check, under gVisor also the build sandbox) can take many seconds: say so
	// instead of staying silent.
	slow := time.AfterFunc(3*time.Second, func() {
		if !*quiet {
			fmt.Fprintln(os.Stderr, "tsbox: building and starting the box ...")
		}
	})
	defer slow.Stop()
	for m, err := range c.Attach(ctx, req) {
		slow.Stop()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return printAPIErr(err)
		}
		switch m.Type {
		case "log":
			printLog(*m.Log)
		case "event":
			if !*quiet {
				shown := printEvent(*m.Event)
				if m.Event.Kind == tsbox.EventScriptExited {
					exitShown = shown
				}
			}
		case "exit":
			final = m.Status
		}
	}
	if ctx.Err() != nil { // Ctrl+C: stop the box (the daemon removes it afterwards)
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.Stop(sctx, spec.Name, 3*time.Second)
		return exitInterrupt
	}
	if final == nil {
		return fail("connection to tsboxd lost")
	}
	return exitCode(*final, *quiet || exitShown)
}

// flagNames turns the engine's words for limits into the flags of tsbox run.
var flagNames = strings.NewReplacer(
	"box memory", "--mem", "box CPU", "--cpu", "box PIDs", "--pids", "start CPU", "--start-cpu",
	`script "main": heap`, "--heap", `script "main": threads`, "--threads",
	"invalid box name", "--name: invalid box name", "invalid storage area", "--storage: invalid storage area")

// absRefs makes local SDK paths and file: secrets absolute (the daemon has a different working directory).
func absRefs(uses []tsbox.SDKUse) {
	for i := range uses {
		if sdkreg.IsPath(uses[i].Name) {
			if a, err := filepath.Abs(uses[i].Name); err == nil {
				uses[i].Name = a
			}
		}
		for k, v := range uses[i].Config {
			if p, ok := strings.CutPrefix(v, "file:"); ok {
				if a, err := filepath.Abs(p); err == nil {
					uses[i].Config[k] = "file:" + a
				}
			}
		}
	}
}

// boxDriver is the driver a box gets: its own, or the daemon's default.
func boxDriver(info client.Info, drv string) string {
	if drv == "" || drv == "auto" {
		return info.Driver
	}
	return drv
}

// checkDriver checks --driver/--rootfs against the daemon. A daemon that lists its drivers runs boxes with any
// available one; an older daemon only with its single driver.
func checkDriver(info client.Info, drv, rootfs string) error {
	if drv != "" && drv != "auto" && len(info.Drivers) > 0 {
		found := false
		for _, d := range info.Drivers {
			if d.Name != drv {
				continue
			}
			found = true
			if !d.Available {
				return fmt.Errorf("driver %q is not usable: %s (tsbox doctor)", drv, d.Error)
			}
		}
		if !found {
			return fmt.Errorf("driver %q does not exist on this platform", drv)
		}
	} else if drv != "" && drv != "auto" && drv != info.Driver {
		if info.Mode == "system" {
			return fmt.Errorf("tsboxd uses the driver %q, not %q (system daemon: change TSBOX_DRIVER in its systemd unit)", info.Driver, drv)
		}
		return fmt.Errorf("tsboxd uses the driver %q, not %q (\"tsbox daemon stop\", then run again with TSBOX_DRIVER=%s)", info.Driver, drv, drv)
	}
	if rootfs != "" {
		return errors.New("--rootfs applies to tsboxd: set TSBOX_ROOTFS or use \"tsbox daemon --rootfs ...\"")
	}
	return nil
}

// printAPIErr prints an error of the daemon (build errors unchanged, as before).
func printAPIErr(err error) int {
	var ae *client.Error
	if errors.As(err, &ae) && ae.Code == "build" {
		fmt.Fprintln(os.Stderr, ae.Message)
	} else {
		fmt.Fprintln(os.Stderr, "tsbox:", err)
	}
	return exitTsboxError
}

func exitCode(st tsbox.BoxStatus, quiet bool) int {
	if len(st.Scripts) == 0 || st.Scripts[0].LastExit == nil {
		if st.Error != "" {
			fmt.Fprintln(os.Stderr, "tsbox:", st.Error)
		}
		return exitTsboxError
	}
	ex := st.Scripts[0].LastExit
	if !quiet && ex.Reason != tsbox.ReasonExit && ex.Reason != tsbox.ReasonCrash {
		fmt.Fprintln(os.Stderr, endMessage("script", ex.Reason, ex.Detail))
	}
	switch ex.Reason {
	case tsbox.ReasonExit, tsbox.ReasonCrash:
		return ex.Code
	case tsbox.ReasonIntegrity, tsbox.ReasonProtocol:
		return exitRefused
	case tsbox.ReasonSpawnFailed, tsbox.ReasonBoxDied:
		return exitTsboxError
	}
	return exitKilled
}

func printLog(l tsbox.LogLine) {
	w := os.Stdout
	prefix := ""
	switch l.Level {
	case "warn", "error", "stderr":
		w = os.Stderr
	}
	if l.Level == "warn" || l.Level == "error" {
		prefix = strings.ToUpper(l.Level) + " "
	}
	fmt.Fprintln(w, prefix+l.Msg)
}

// endMessage formats the end of a script: "tsbox: <who> ended: <reason> – <detail>".
func endMessage(who string, reason tsbox.ExitReason, detail string) string {
	msg := fmt.Sprintf("tsbox: %s ended: %s", who, reason)
	if detail != "" {
		msg += " - " + detail
	}
	return msg
}

// printEvent prints an event of a foreground box. It reports whether it printed a script end.
func printEvent(ev tsbox.Event) bool {
	switch ev.Kind {
	case tsbox.EventRestartPlanned:
		fmt.Fprintf(os.Stderr, "tsbox: restarting %s in %s\n", ev.Script, ev.Detail)
	case tsbox.EventCircuitOpen:
		fmt.Fprintf(os.Stderr, "tsbox: %s is no longer restarted (%s)\n", ev.Script, ev.Detail)
	case tsbox.EventBoxQuarantined:
		if strings.HasPrefix(ev.Detail, "memory limit of the box reached") {
			fmt.Fprintf(os.Stderr, "tsbox: %s (raise --mem)\n", ev.Detail) // the script end already says "oom"
		} else {
			fmt.Fprintf(os.Stderr, "tsbox: box quarantined: %s\n", ev.Detail)
		}
	case tsbox.EventScriptExited:
		if ev.Exit != nil && ev.Exit.Reason.Failure() && ev.Exit.Reason != tsbox.ReasonCrash {
			fmt.Fprintln(os.Stderr, endMessage(ev.Script, ev.Exit.Reason, ev.Exit.Detail))
			return true
		}
	}
	return false
}

var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// baseName is the cleaned file name of a script (box prefix and default storage area).
func baseName(entry string) string {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(entry), filepath.Ext(entry)))
	base = strings.Trim(unsafeName.ReplaceAllString(base, "-"), "-")
	if base == "" {
		base = "box"
	}
	if len(base) > 38 {
		base = strings.Trim(base[:38], "-")
	}
	return base
}

func defaultName(entry string) string {
	var r [3]byte
	_, _ = rand.Read(r[:])
	return baseName(entry) + "-" + hex.EncodeToString(r[:])
}

// parseBytes and parseRestart share the syntax with tsbox.json.
func parseBytes(s string) (int64, error)                 { return project.ParseBytes(s) }
func parseRestart(s string) (tsbox.RestartPolicy, error) { return project.ParseRestart(s) }

func jsonValid(s string) bool { return json.Valid([]byte(s)) }

// notFound reports a missing script and suggests files with the same name in nearby directories
// (e.g. "hello.ts" → "examples/hello.ts").
func notFound(entry string) string {
	msg := fmt.Sprintf("script %s not found", entry)
	base := filepath.Base(entry)
	var found []string
	root, _ := filepath.Abs(filepath.Dir(entry))
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || strings.Count(rel, string(filepath.Separator)) >= 2) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == base && len(found) < 3 {
			found = append(found, filepath.Join(filepath.Dir(entry), rel))
		}
		return nil
	})
	if len(found) > 0 {
		msg += " (did you mean " + strings.Join(found, " or ") + "?)"
	}
	return msg
}
