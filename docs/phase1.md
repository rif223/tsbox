# tsbox – Phase 1 (core), as of 2026-11-01

## Scope

| Part | Location | Status |
|---|---|---|
| Frame protocol v2 (`call/ret`, `sub`, `event`, `alive`, `pstats` …) + fuzz test | `internal/proto` | ✅ |
| `@tsbox/core` (console/stdout → log, heartbeat, `call`, `on`, `keepAlive`, `script`) | `js/core` | ✅ |
| Build: boot entry, `--check`, no remote/npm imports, content-addressed, reproducible | `internal/build` | ✅ (on the host; build sandbox in phase 2) |
| tsbox-init: process per script, integrity, heap, RSS, threads, run time, watchdog, protocol, backpressure | `internal/initbox` | ✅ |
| UID per script (`__exec`: RLIMIT_NPROC, setuid, no_new_privs) | `internal/initbox/exec_linux.go` | ✅ under gVisor |
| Driver gvisor: stdio channel, `--host-uds=none`, no `/tmp`, SETUID/SETGID/KILL, `cgroup.kill` | `internal/driver/gvisor` | ✅ |
| Driver dev: Windows job object (RAM/CPU/processes), RAM watchdog, process group | `internal/driver/dev` | ✅ |
| Engine: box lifecycle, restart policy with backoff and breaker, OOM detection, box watchdog, quarantine, events, logs with rate limit, stats | `tsbox.go`, `engine.go`, `box.go` | ✅ |
| CLI `tsbox run` (foreground, exit codes, Ctrl+C) | `cmd/tsbox` | ✅ |
| Attack and E2E tests | `e2e_test.go`, `testdata/` | see below |
| CI (Linux, Windows, macOS; fuzz) | `.github/workflows/ci.yml` (job `tsbox`) | set up, not yet run on GitHub |

## Deviations from the architecture draft

- **Logs per box** exist in phase 1 only as a callback (`WithLogs`), not yet as a `Box.Logs` iterator. The iterator comes with the log files in phase 3.
- **`call`** always answers `not_found` in phase 1, because there are no SDKs yet; the path script → init → host → script is already tested with it.
- **Script watchdog** via an `alive` heartbeat instead of a ping (spike g). A script without open work ends by itself; for this `@tsbox/core` uses `core.read` + `unrefOpPromise` (internal Deno API, with a fallback).
- **Box OOM in the dev driver** ends the whole box (reason `oom`, box quarantined). Under gVisor the kernel hits only the largest script (spike f).
- **`/tmp` under gVisor (new finding):** without its own entry, runsc mounts a *writable* tmpfs at `/tmp`, shared
  by all scripts of the box. Only the escape simulation (`__probe`) revealed this; the spike had always mounted
  /tmp itself. tsbox now mounts an empty, read-only tmpfs at `/tmp` and starts runsc with `--overlay2=none` (no
  writable layer over the rootfs). `TestProfile` and `TestSiblingsAndNetwork` cover this.

## Tests (2026-11-01)

| Environment | Unit | E2E |
|---|---|---|
| Windows 11 ARM64, dev (job object) | ✅ | ✅ 8 green, 2 skipped (TestThreads: threads not enforced on Windows; TestSiblingsAndNetwork: gVisor only) |
| WSL2 Ubuntu 24.04 arm64, dev | ✅ | ✅ 9 green, 1 skipped (TestSiblingsAndNetwork: gVisor only) |
| WSL2, gVisor (runsc 20261026, systrap) | ✅ | ✅ 10/10 |
| macOS | – | not checked locally (runs in the CI) |

E2E checks with real Deno processes: exit codes and errors, script end despite open stdin, stdout redirection,
the `call` path, attack suite I4 (env, files, network, run, ffi, sys, `node:` modules, dynamic imports,
workers), limits (RSS, watchdog, run time, heap, protocol, threads, box OOM), restart with breaker, integrity,
identity of two scripts and stop, and under gVisor the escape simulation (I3, I8, I11): a native process with
its own UID fails at kill/ptrace/environ/mem of the siblings, at init, at `setuid(0)`, at every write path and
at TCP, UDP and DNS.

## Demo

Windows (PowerShell or Git Bash, in the repo directory):

```bash
go build -o tsbox.exe ./cmd/tsbox
./tsbox.exe run examples/hello.ts
./tsbox.exe run testdata/attack/escape.ts                 # all PROBE … NotCapable
./tsbox.exe run testdata/attack/rss.ts --rss 64Mi --mem 512Mi   # ⇒ rss, exit 137
./tsbox.exe run testdata/attack/busy.ts --watchdog 2s     # ⇒ watchdog
./tsbox.exe run testdata/scripts/crash.ts --restart on-failure:3   # 3 runs, then the breaker
```

WSL with gVisor (as root; `wsl-test.sh` builds to `/usr/local/bin/tsbox` and tests dev + gVisor):

```bash
bash <repo>/scripts/wsl-test.sh
cd /root/tsbox && tsbox run examples/hello.ts
cd /root/tsbox && tsbox run testdata/attack/rss.ts --mem 192Mi   # ⇒ oom, only this script
```

## Open for phase 2

SDK system (manifest, providers, grants, kv/log/http, external providers), build sandbox without network
(profile with a read-only `DENO_DIR`), `tsbox types`/`sdk` commands. gVisor E2E in the CI (phase 4).
