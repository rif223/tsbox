# tsbox – Architecture

tsbox runs TypeScript code on Deno in isolation, similar to how Docker runs containers: with one command, hard
limits and controlled capabilities (SDKs). It can be used as a CLI and as a Go library.

**Status marks:** ✅ implemented and tested · 🟡 with limitations · ⬜ planned

Measurements, design decisions and known limits: [NOTES.md](NOTES.md); the gVisor experiments the design rests
on: [gvisor-spike.md](gvisor-spike.md); Firecracker compared with gVisor: [firecracker-test.md](firecracker-test.md).

---

## 1. Terms

| Term | Meaning | Docker analogy |
|---|---|---|
| **Box** | Isolated environment with hard limits (RAM, CPU, PIDs), one trust domain. Exactly one sandbox under gVisor | container |
| **Script** | TS entry point in a box, built into an artifact (SHA-256), runs as its own Deno process with its own UID | process in the container |
| **Artifact** | Bundled JS in the content-addressed store `<state>/artifacts/sha256/<hash>.js` | image layer |
| **SDK** | Capability made of manifest, TS module and provider | volume/network plugin |
| **Provider** | Host side of an SDK: built-in (Go) or external process (any language, frames over stdio) | Docker plugin, LSP server |
| **Grant** | Right of an SDK granted explicitly at start, e.g. `http:host=api.github.com` | `-v`, `-p` |
| **Driver** | Isolation technology: `gvisor` (Linux, isolated), `firecracker` (Linux with KVM, one microVM per box, isolated, opt-in), `dev` (all systems, **not** isolated) | runtime (runc/runsc) |
| **Rootfs/profile** | Debian minbase + Deno (`scripts/build-rootfs.sh`); `default` = `TSBOX_ROOTFS`, further named profiles at `TSBOX_PROFILES/<name>` (e.g. a different Deno version), fingerprint in `doctor` and `BoxStatus` | base image |
| **tsboxd** | Daemon with a local API (Unix socket or named pipe), persistent state and log files | dockerd |
| **Project** | `tsbox.json` with several boxes, `tsbox up`/`down` | Compose project |
| **Storage** | Storage area of stateful SDKs (kv), default = box name | named volume |

```
tsbox (CLI) ──HTTP/JSON over Unix socket or named pipe──▶ tsboxd ──▶ engine
 ├─ driver: gvisor | firecracker | dev
 ├─ SDK registry: log, kv, http, echo (bundled) + installed + local paths
 └─ box "bot" ── cgroup limits, one trust domain, labels
     ├─ tsbox-init (PID 1 in the sandbox)
     ├─ script "main" ── Deno, UID 10001, heap/RSS/threads/run time/watchdog, restart policy
     └─ script "cron" ── Deno, UID 10002
```

## 2. Components and trust zones

```
T0   host: tsboxd (engine), built-in providers, secrets, state              trusted
T0c  CLI / API clients with access to the socket or pipe                     like the docker group (= rights of tsboxd)
T0'  external providers (host processes, installed by the operator)         trusted by the operator
═════ stdin/stdout of "runsc run" (frames) ═════════════════════════════════════════════════
T1   box (gVisor, --network=none, --host-uds=none, --overlay2=none, ro rootfs):
     tsbox-init as UID 0 of the sandbox, capabilities only SETUID/SETGID/KILL   considered compromisable
═════ pipes per script ═════════════════════════════════════════════════════════════════════
T2   script: Deno, own UID, no capabilities, no_new_privs, RLIMIT_NPROC, empty environment,
     --allow-read=<own artifact>, --no-remote --no-npm --no-config --no-lock    untrusted
TB   build sandbox (gVisor, no network, UID 65534, sources ro, output via stdout)  untrusted
```

| Component | Location | Task |
|---|---|---|
| Engine | `tsbox.go`, `engine.go`, `box.go`, `sdkcall.go`, `logs.go` | box lifecycle, build, frame dispatch, SDK checks, restart policy, watchdog, events, logs (files + `Box.Logs`), stats |
| tsboxd | `internal/daemon` (role `tsbox daemon`) | API, state per box (`tsboxd.json`), restore, autostart idle exit, event ring buffer |
| Client | `client/` (public) | Go client of the API, addresses (`Hosts`, `UserHost`) |
| Project | `internal/project` | parser for `tsbox.json` |
| tsbox-init | `internal/initbox` (role `tsbox __init` of the same binary) | script processes, integrity, per-script limits, `sid` from the pipe |
| Drivers | `internal/driver/{gvisor,firecracker,dev,cgroup}` | start the sandbox, channel, stats, `cgroup.kill`, build sandbox |
| Guest of the firecracker driver | `internal/fcguest`, `internal/fcproto` | PID 1 in the microVM (`__fcinit`), rights before exec (`__fcexec`), vsock protocol host ↔ guest |
| Build | `internal/build` | `deno bundle --check --no-remote --no-npm`, boot entry, import map, artifact store, build cache by input hash |
| Protocol | `internal/proto`, `js/core/frames.ts` | frames `u32 BE | JSON`, ≤ 1 MiB, strict |
| `@tsbox/core` | `js/core` | script side: console/stdout → log, heartbeat, `call`, `on`, `keepAlive`, `script` |
| SDK system | `manifest/`, `sdk/`, `internal/sdkreg`, `internal/providers/*`, `sdks/` | manifest, provider API, registry, built-in and external providers |

## 3. CLI

```bash
tsbox run <script.ts> [--mem 128Mi --cpu 0.5 --pids 64 --start-cpu 0]    # box
                      [--heap 64Mi --rss 0 --threads 32 --timeout 0 --watchdog 10s]  # script
                      [--restart no|on-failure[:n]|always] [--driver auto|gvisor|firecracker|dev]
                      [--profile <name>]
                      [--sdk <name|path> …] [--grant <sdk>:<right>=<value> …]
                      [--sdk-config <sdk>:<name>=<value> …] [--storage <name>] [--params '<json>' | --params-file <file>] [-q]
                      [-d [--rm]] [--name <box>]
tsbox ps [-a] [--json]
tsbox logs [-f] [--since 10m|<RFC3339>] [--script <name>] [-n <lines>] [-t] <box>
tsbox stats [--json] [--no-stream] [<box>]     # live table BOX SCRIPT STATE RAM/LIMIT CPU PIDS RESTARTS UPTIME
tsbox stop [-t 5s] | start | restart [-t] | rm [-f] <box> …
tsbox inspect <box> …                          # JSON without secret values
tsbox events [--since] [--box] [--json]
tsbox up [-d] [-f tsbox.json] | down [-f tsbox.json]
tsbox daemon [--system] [--socket] [--idle 0] [--driver] [--rootfs] [--profiles] [--platform systrap|kvm] [--fc-kernel] [--fc-jailer] [--oom-score-adj 500] [--group tsbox]   # tsboxd
tsbox daemon stop [-f]                   # end the user daemon (-f: also with running boxes)
tsbox doctor | setup [--dry-run] [--no-rootfs] [--no-systemd] [--firecracker] [--force]
tsbox check <script.ts> [--sdk …]       # build with type checking and SDK checks, without starting
tsbox types [--sdk …]                   # deno.json + .tsbox/types for the editor
tsbox sdk ls | info <sdk> | add <path|git+url#ref> | rm <name> | init <name> [--provider deno|go] | test <path>
tsbox version
```

**Daemon:** All box commands (including `run` in the foreground) talk to tsboxd; `check`, `types` and `sdk` work
locally. The CLI tries `TSBOX_HOST`, on Linux `/run/tsbox/tsboxd.sock` (system daemon), and then the user
daemon. If none answers, it starts `tsbox daemon --autostarted --idle 5m` detached (output to
`<state>/tsboxd.log`); it exits after 5 min without a running box and without a connection (`TSBOX_IDLE`).
`TSBOX_NO_AUTOSTART=1` turns this off. An autostarted daemon of a different build is replaced as long as no box
is running. The daemon chooses driver and rootfs (`TSBOX_DRIVER`, `TSBOX_ROOTFS`); `run --driver` only checks
that it matches; to switch, `tsbox daemon stop` and start again with another `TSBOX_DRIVER`. If an autostarted
daemon ends right away (e.g. `runsc` missing), the CLI shows its error at once instead of waiting.

| Address | Linux/macOS | Windows |
|---|---|---|
| User daemon | `<state>/run/tsboxd.sock` (directory 0700, socket 0600; for paths > 100 characters `$XDG_RUNTIME_DIR/tsbox/<hash>.sock` or `/tmp/tsbox-<uid>/`, owner checked) | named pipe `\\.\pipe\tsboxd-<hash(SID, state)>`, DACL `D:P(A;;GA;;;<SID>)`, the client checks the owner of the server process |
| System daemon | `/run/tsbox/tsboxd.sock`, directory 0750, socket 0660 `root:tsbox` | – |

**API (`/v1`, JSON, streams as NDJSON):** `GET info` · `GET boxes?all=` · `POST boxes` `{spec, start,
autoRemove, attach, env}` (with `attach` a stream of logs, events and the final status; if it breaks, the daemon
stops the box) · `GET|DELETE boxes/{name}` · `POST boxes/{name}/start|stop|restart` ·
`GET boxes/{name}/logs?follow&since&script&tail` · `GET stats?box&stream` · `GET events?since&box` ·
`POST shutdown`. Errors: `{"error":{"code","message"}}` with `not_found`, `conflict`, `invalid`, `build`.

**State:** `<state>/boxes/<name>/tsboxd.json` (specification with absolute paths and references
`file:`/`env:`, desired state, last status), `artifacts/`, `logs/<script>.log` + `.log.1` (JSON lines, rotation
at 10 MiB). At start tsboxd loads all boxes; it restarts those with desired state "running" and a restart policy
≠ `no` (build from the sources). The CLI sends `env:` secrets along as values; tsboxd keeps them in memory only.
After a restart of the daemon, `tsbox start` passes them again.

**Project file `tsbox.json`** (strict, paths relative to the file, boxes are named `<project>-<box>`):

```json
{
  "name": "demo",
  "boxes": {
    "bot": {
      "limits": {
        "memory": "256Mi",
        "cpu": 0.5,
        "startCpu": 2,
        "pids": 64
      },
      "restart": "on-failure:5",
      "storage": "bot",
      "profile": "default",
      "labels": {
        "team": "a"
      },
      "sdks": {
        "kv": {},
        "http": {
          "grants": {
            "host": ["api.github.com"]
          },
          "config": {
            "token": "env:GITHUB_TOKEN"
          }
        },
        "own": {
          "path": "./sdks/own"
        }
      },
      "scripts": {
        "main": {
          "entry": "src/bot.ts",
          "params": {
            "x": 1
          },
          "limits": {
            "heap": "64Mi",
            "rss": "96Mi",
            "threads": 32,
            "timeout": "0",
            "watchdog": "10s"
          }
        },
        "cron": {
          "entry": "src/cron.ts",
          "restart": "always"
        }
      }
    }
  }
}
```

`up` creates missing boxes, recreates changed ones (label `tsbox.spec-hash` over specification and sources) and
removes boxes that are no longer in the file. Without `-d` it follows the logs, Ctrl+C stops the boxes. `down`
removes all boxes of the project (kv data stays). The project name defaults to the directory name; the label
`tsbox.project-dir` records the directory, so `up` and `down` refuse to touch boxes of another directory with the
same project name (give one of them its own `"name"`).

**Exit codes of `run`:** the script's code; 137 for limits (`rss`, `threads`, `heap`, `oom`, `watchdog`,
`timeout`); 126 for `integrity`/`protocol`; 125 for errors of tsbox or tsboxd (build, start); 2 for wrong input
(unknown command or flag, invalid value, missing script); 130 for Ctrl+C.

**Exit codes of the other commands:** 0 success; 1 failure (box not found, build failed in `check`, tsboxd not
reachable, ...); 2 wrong input; 130 for Ctrl+C in `logs -f`, `stats`, `events` and `up`.

## 4. Go API

```go
func main() {
	tsbox.Init() // the own binary serves as tsbox-init in the box
	e, _ := tsbox.New(ctx, tsbox.WithLogs(onLog), tsbox.WithEvents(onEvent),
		tsbox.WithProvider("mysdk", myProvider), tsbox.WithSDKDir("./sdks"))
	b, _ := e.Run(ctx, tsbox.BoxSpec{
		Name: "bot", Limits: tsbox.Limits{Memory: 256 << 20, CPU: 0.5},
		Restart: tsbox.RestartPolicy{Mode: tsbox.RestartOnFailure},
		SDKs: []tsbox.SDKUse{{Name: "kv"}, {Name: "http", Grants: map[string][]string{"host": {"api.github.com"}}}},
		Scripts: []tsbox.ScriptSpec{{Name: "main", Entry: "bot.ts", Limits: tsbox.ScriptLimits{RSS: 96 << 20}}},
	})
	st, _ := b.Wait(ctx) // or b.Stop(ctx, 5*time.Second), b.Status(), b.Stats(ctx)
	for l, err := range b.Logs(ctx, tsbox.LogOptions{Tail: 100, Follow: true}) { … } // from the log files
	_ = e.Remove(ctx, "bot", true)
}

// Against a running daemon:
c, _ := client.New(client.Hosts()[0])
boxes, _ := c.List(ctx, true)
for m, err := range c.Attach(ctx, client.CreateRequest{Spec: spec}) { … } // like tsbox run
```

- **Public packages:** `tsbox` (engine), `client` (API of tsboxd), `sdk` (provider API), `manifest` (manifest, grants). Everything else is `internal/`.
- **Logs:** per script `<state>/boxes/<box>/logs/<script>.log`, rotated at 10 MiB (`WithLogFileSize`), at most two files; can be turned off with `WithoutLogFiles()`. A separate goroutine writes them via a bounded queue (losses are counted). `LogLine.Seq` is strictly monotonic; `Box.Logs`/`Engine.Logs` return stored lines (`Since`, `Script`, `Tail`) and with `Follow` new ones until the box ends.
- **Script states:** `pending` (not started yet) → `starting` (process started, @tsbox/core not up yet) → `running` (the first frame of the script arrived: its first heartbeat, a log line, an SDK call, …; independent of whether and how it logs) → `ready` (only after the script's own `ready()`, opt-in) → `exited`; also `backoff` and `broken`. `script_started` is emitted once per start, `script_ready` at `ready()`.
- **Events** (asynchronous, never blocking, losses are counted): `box_started`, `box_exited`, `box_quarantined`, `script_started`, `script_ready`, `script_exited` (with reason), `restart_scheduled`, `circuit_open`, `integrity_failed`, `oom`, `violation`.
- **Exit reasons:** `exit`, `crash`, `heap`, `rss`, `threads`, `oom`, `watchdog`, `timeout`, `integrity`, `protocol`, `backpressure`, `spawn_failed`, `killed`, `stopped`, `box_hung`, `box_died`, `box_shutdown`.
- **Restart policy:** `no` | `on-failure` | `always`, backoff 1 s · 2^(n−1) up to 60 s. Circuit breaker: n failures in 10 min ⇒ `broken`. `integrity` and `protocol` never restart; a hung or dead box is quarantined.
- **Profiles and host OOM:** `BoxSpec.Profile` selects the rootfs (gVisor only; `WithRootFS` = `default`, `WithProfilesDir` for others, `Engine.Profiles()` lists them with their fingerprint). `WithOOMScoreAdj` (default 500) applies to the host processes of every box.
- **Build cache and platform:** unchanged scripts are not rebuilt (key over script directory, SDK modules, core, Deno version; `WithoutBuildCache()` turns it off). Real builds of an engine are limited (dev 4, gVisor and Firecracker 2); store entries unused for 7 days are removed. `WithGVisorPlatform("kvm")` or `TSBOX_PLATFORM` selects the gVisor platform (needs `/dev/kvm`).
- **CPU boost at start (opt-in):** `Limits.StartCPU` (`--start-cpu`, `"startCpu"`) lets the box start with more CPU until all scripts are ready or have ended (at most 30 s); after that `Limits.CPU` applies. If the reset fails, the box is ended. Works under gVisor (cgroup `cpu.max`) and dev on Windows (job object).

## 5. Protocol v2

`u32 big endian length | JSON object`, 1 byte up to 1 MiB. Unknown fields and types are rejected, IDs are ≤ 2⁵³−1.

| Type | Direction | Fields |
|---|---|---|
| hello | init → host | p{proto, init, uidMode, baselineRss} |
| start / stop | host → init | sid, p{artifact, sha256, uid, heapMiB, rssBytes, threads, timeoutMs, watchdogMs, params} |
| call / ret | script → host / back | id, sdk, m, p / id, p \| err{code, msg} |
| sub / unsub / event | script → host / host → script | sdk, events[] / sdk, name, p |
| ready, log, stats | script → host | – / level, msg / p{heapUsed, heapTotal, external} |
| alive | script → init (heartbeat); init forwards only the first one of every start (`starting` → `running`) | – |
| pstats, exit | init → host | p[{sid, rss, cpuNs, threads}] / sid, p{code, signal, reason, detail} |
| ping / pong | host ↔ init (box watchdog) | id |
| init, init_ok, open, opened, close, emit | engine ⇄ external provider | see §6.4 |

init always sets the `sid` from the pipe, the engine sets the box identity from the driver's channel.
`proto.FromScript` and `proto.FromInit` define which types a script or init may send.

## 6. SDKs

### 6.1 Layout

```
my-sdk/
  tsbox-sdk.json   manifest
  mod.ts           TS module (types + wrapper over @tsbox/core)
  example.ts       optional; "tsbox sdk test" runs it in a box
  provider/        only for an external provider
```

### 6.2 Manifest

```json
{
  "name": "http", "version": "0.1.0", "tsbox": "^0.1",
  "import": "@tsbox/http", "module": "mod.ts",
  "provider": { "builtin": "http" },
  "config": { "maxBody": { "type": "bytes", "default": "1Mi" }, "token": { "type": "secret" } },
  "permissions": { "host": { "type": "host-pattern", "multiple": true, "required": true } },
  "methods": { "fetch": { "permission": "host", "rate": "10/s", "burst": 20, "concurrency": 4,
                          "maxParams": "256Ki", "maxResult": "1020Ki", "timeout": "30s" } },
  "events": { "tick": {} }
}
```

- The provider is either `builtin` (registered in the engine) or `exec` (external process; `deno` stands for the host's Deno, `./x` is relative to the SDK).
- The prefix `@tsbox/` is reserved for bundled SDKs.
- Configuration types: `string`, `int`, `bool`, `bytes`, `duration`, `secret` (only `file:`/`env:`).
- Right types: `string`, `host-pattern`, `int`, `bytes`, `enum`.
- Core upper limits: parameters and result just below 1 MiB, timeout ≤ 5 min, ≤ 64 concurrent calls per method.

### 6.3 Checks before every call (core)

1. SDK active and method in the manifest; otherwise `not_found`, the same answer for both
2. right granted (`permission_denied`)
3. parameter size (`too_large`)
4. rate per script and method (`rate_limited`)
5. concurrency per method and at most 16 per script
6. time limit of the method
7. result size

The provider checks the content. The script sees foreign errors only as a neutral `internal`.

### 6.4 Provider protocol for external providers

One process per SDK and configuration, sessions via `sid`:

`init{proto,sdk,version,config}` → `init_ok` · `open{sid,box,script,grants}` → `opened` ·
`call{sid,id,m,p}` → `ret{sid,id,p|err}` · `emit{sid,name,p}` · `close{sid}` · `log` · `ping/pong`

If the process dies, open calls fail with `unavailable`; the next open restarts it.

### 6.5 Bundled

| SDK | Provider | Content |
|---|---|---|
| `log` | builtin | structured lines with fields |
| `kv` | builtin | get/set/delete/list per storage and script, persistent (JSON file), quota 1 MiB, value ≤ 64 KiB |
| `http` | builtin | fetch: host allowlist, https only, IP check while connecting (against SSRF and DNS rebinding), redirects checked, no proxy, no cookies, size and time limit |
| `echo` | external (Deno) | template for external providers with events |

## 7. Security model

| # | Invariant | Enforcement | Evidence |
|---|---|---|---|
| I1 | Identity only from the channel | box = the driver's pipe, script = init's pipe; the `sid` in the frame is overwritten | E2E `TestIdentityAndStop`, fuzz |
| I2 | No secret in the box | config stays on the host, `start` frame without secrets, init environment empty; tsboxd stores only references (`file:`/`env:`), `env:` values in memory only | `TestSecretsNotPersisted` (state, inspect, logs, events), `TestCanaryInBoxMemory` (memory of all box processes incl. the sentry's memfd, with positive control) ✅ |
| I3 | Box without network | `runsc --network=none`; Deno without `--allow-net` | `TestSiblingsAndNetwork` (TCP, UDP, DNS with a native process) ✅ |
| I4 | Script only with its own artifact; no env/write/run/ffi/sys/net | Deno flags, empty environment, own UID | `TestEscape` (incl. `node:`, dynamic imports, workers) ✅ |
| I5 | Bytes from the box do not crash the host | strict codec, `recover` for providers; sizes without NaN/Inf | 21 fuzz targets for all parsers (`scripts/fuzz.sh`, CI) ✅ |
| I6 | DoS stays in the box or script | cgroup (no swap, cpu, pids), the kernel OOM hits the largest script, box watchdog with `cgroup.kill`; per script heap, RSS, threads (RLIMIT_NPROC), run time, watchdog; every queue bounded; host OOM hits boxes first (`oom_score_adj` 500, tsboxd −500) | `TestLimits`, `TestThreads`, `TestBoxOOM`, `TestOOMScoreAdj`, `TestLoadManyBoxes` ✅ |
| I7 | SDK calls only within the grants | core + provider, default: deny | `TestSDK*`, provider unit tests ✅ |
| I8 | No writable path in the box | ro rootfs without overlay, `/tmp` empty and ro, no rw mounts | `TestProfile`, `TestSiblingsAndNetwork` ✅ |
| I9 | Only checked artifacts run | content-addressed, init checks SHA-256 before every start | `TestIntegrity` ✅ |
| I10 | Build without network and without host write access | build sandbox (gVisor) or `--no-remote --no-npm` (dev) | `TestBuildSandbox`, build unit tests ✅ |
| I11 | Scripts of one box cannot reach each other | own UID, no ptrace/kill; `cmdline`/`status` stay readable (gVisor ignores `hidepid`) | `TestSiblingsAndNetwork` ✅ |
| I12 | Only authorized users control tsboxd | socket 0600 (user) or 0660 `root:tsbox` (system), directories 0700/0750 with owner check; Windows: named pipe with a DACL for the user only, the client checks the owner of the pipe server | `TestAutostartIdleAndPerms` ✅ |

**Residual risks:**
- The dev driver does not isolate.
- With `StartCPU` (opt-in) a box may use up to 30 s more CPU than `CPU` at every start; restarts of single scripts trigger no boost, and only the operator starts a box (CLI, API or the restore by tsboxd).
- External providers are trusted code.
- Bugs in gVisor or the kernel; side channels between boxes.
- A compromised init can impersonate scripts of the same box (the box is the trust domain).
- The per-script RSS limit is only approximate under gVisor (`VmRSS − baseline`, spike finding 1).
- Access to tsboxd corresponds to the `docker` group: whoever reaches the socket starts boxes with the daemon's rights. The system daemon runs as root and reads script, SDK and `file:` paths with root rights.
- Installed SDKs live in the daemon's state; a user's `tsbox sdk add` does not affect the system daemon.

## 8. Platforms

| | Linux + gVisor | Linux + Firecracker (opt-in) | Linux without gVisor | Windows | macOS |
|---|---|---|---|---|---|
| driver `auto` | gvisor | never chosen by `auto` | dev + warning | dev + warning | dev + warning |
| box RAM | cgroup | cgroup in the guest (VM RAM = limit + 64 MiB) | watchdog | job object + watchdog (private memory without the unused writable sections of init's image) | – |
| box CPU / processes | cgroup | host cgroup of the VM / cgroup in the guest | – | job object | – |
| script RSS / threads | `VmRSS−baseline` / RLIMIT_NPROC | RssAnon (exact) / RLIMIT_NPROC (real guest kernel) | RssAnon / measured | private memory / – | – |
| UID per script | yes | yes, `/proc` with `hidepid=2` | no | no | no |
| host OOM hits boxes first | all box processes `oom_score_adj` 500 | the firecracker process 500 | init and scripts 500 | – | – |
| build | build sandbox | build microVM | host | host | host |

With `TSBOX_REQUIRE_ISOLATION=1` or `WithRequireIsolation()` tsbox refuses the dev driver. Deno is found via
`TSBOX_DENO`, `PATH` or the npm installation on Windows; the npm wrapper is skipped.

## 9. Project structure

```
tsbox.go engine.go box.go sdkcall.go logs.go   engine (public)
client/  sdk/  manifest/               API client, provider API, manifest (public)
cmd/tsbox/                             CLI (+ roles daemon, __init, __exec, __oomexec, __probe, __fcinit, __fcexec), CLI E2E tests
internal/daemon                        tsboxd: API, state, socket/pipe
internal/logfile  internal/project    log files with rotation, tsbox.json
internal/profile  internal/fsutil     named profiles (rootfs), file helpers: rename and RemoveAll with retries and fallbacks (Windows), SHA-256, path checks
internal/proto                         frames v2
internal/initbox                       tsbox-init
internal/driver/{gvisor,firecracker,dev,cgroup}   drivers, build sandbox / build microVM
internal/fcguest  internal/fcproto     PID 1 in the Firecracker microVM, vsock protocol
internal/build                         bundler, artifact store, build cache
internal/sdkreg                        registry, lock.json
internal/providers/{log,kv,httpp,extproc}
internal/denobin  internal/sdktest
js/core/                               @tsbox/core
sdks/{log,kv,http,echo}/               bundled SDKs (embedded)
examples/  testdata/{scripts,attack,sdk,sdk-secret}/
scripts/                               build-rootfs.sh (embedded for setup, PROFILE=), wsl-test.sh (also CI), fuzz.sh
docs/
```

## 10. Planned

- a public driver API (own isolation drivers outside this repository)
- an SDK index (finding and installing SDKs by name)
