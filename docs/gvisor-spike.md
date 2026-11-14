# tsbox – gVisor spike (2026-11-01)

Goal: check the eight assumptions the design of [ARCHITECTURE.md](ARCHITECTURE.md) rests on, before building on
them.

**Environment:**
- WSL2 Ubuntu 24.04 on Snapdragon X Elite (arm64), kernel 6.18
- runsc release-20261026.0 (systrap, `--directfs`), Deno 2.9.6, Go 1.24
- Rootfs: Debian bookworm (minbase) with Deno at `/usr/local/bin/deno` (today: `scripts/build-rootfs.sh`)
- Windows 11 (ARM64) with Deno 2.9.6 win32-x64

**Code:** the throwaway code (`spike/`: spike init, host starter for `runsc run`, Deno test scripts, test runs)
was removed later; the results are recorded here.

## Results at a glance

| # | Assumption | Result | Consequence for the design |
|---|---|---|---|
| a | Channel over stdin/stdout of `runsc run` with `--host-uds=none` | ✅ **confirmed** | as planned |
| b | UID switch in gVisor with only SETUID/SETGID/KILL | ✅ **confirmed** | as planned; the bounding set stays set in the child (harmless) |
| c | `RLIMIT_NPROC` counts threads per UID under gVisor | ✅ **confirmed** | hard thread limit per script |
| d | `/proc` with `hidepid=2`; ptrace/kill between UIDs refused | 🟡 **partly** | ptrace, kill, environ, maps and mem are blocked; `hidepid` has no effect, cmdline and status are readable |
| e | `deno bundle` in gVisor without network | 🟡 **only with a pre-filled cache** | esbuild comes from npm; the profile needs a read-only `DENO_DIR` |
| f | `memory.oom.group` with the runsc cgroup | ❌ **rejected** | a pre-created cgroup defeats the limit; without `oom.group` only the script dies |
| g | Script ends despite open stdin | ✅ with `reader.cancel()` | the transport cancels reading as soon as nothing more is expected |
| h | `Deno.stdout` can be redirected | ✅ **confirmed** | the protocol stays on stdout; fd 3/4 fails under gVisor |

In addition there were four findings I was not looking for (§9).

---

## a) stdio channel through runsc ✅

`host -mode echo`: 2,000 frames (1 B to 1 MiB, random content) there and back through the spike init.

```
{"echo_frames":2000,"bytes":35172732,"corrupt":0,"first_roundtrip_since_start_ms":75,"avg_rtt_us":909,"mib_per_s":17.2}
# runsc exited after 2.032s: <nil>
```

- 0 errors. Binary data passes unchanged (`terminal: false`).
- First round trip **75 ms** after the start of `runsc run`.
- Mean RTT 0.9 ms; the value is skewed by the 1 MiB frames.
- If the host closes stdin, init sees EOF and exits, and `runsc run` ends with 0.
- Flags `--network=none --host-uds=none`, **no** socket mount, no rw bind mount.

## b) UID per script ✅

`host -mode uid -caps SETUID,SETGID,KILL`. init (UID 0 of the sandbox, `CapEff=0xe0`) starts a helper. It sets rlimits, switches via `setgroups/setgid/setuid` to 10001 or 10002 and then runs `exec`.

| Check (script 10001 against sibling 10002) | Result |
|---|---|
| own rights | `CapEff=0 CapPrm=0 NoNewPrivs=1`, `setuid(0)` → EPERM |
| `kill(pid, 0)` | EPERM |
| `ptrace(ATTACH)` | EPERM |
| `/proc/<pid>/environ`, `maps`, `mem` | EACCES |
| `/proc/<pid>/cmdline`, `status` | **readable** |
| writing `/` | EACCES (ro rootfs) |
| writing `/tmp` | **allowed** (tmpfs 1777, all UIDs share it) |

- **Without capabilities** even `setgroups` fails (EPERM). init needs SETUID and SETGID; it needs KILL to end scripts with a foreign UID.
- `PR_CAPBSET_DROP` fails without `CAP_SETPCAP`, so the script's bounding set stays `0xe0`. Because of `no_new_privs` and UID ≠ 0 the script cannot get these rights back. Decision: do **not** grant SETPCAP; the risk is smaller than one more capability in init.

## c) Thread limit per script ✅

`host -mode nproc -mem 1024 -- <limit> 24`. A Deno script running as its own UID starts workers until it fails. A sibling script runs with the same limit under another UID.

| RLIMIT_NPROC | gVisor | native (Linux) |
|---|---|---|
| no limit | 24 workers | 24 workers |
| 12 | 5 workers, then `WouldBlock (os error 11)` | – |
| 20 | 13 workers, then `WouldBlock` | 12 workers, then `WouldBlock` |

- An empty Deno process has **7 threads**, each worker about one more.
- The error can be caught in JS, the script does not crash, and the sibling script keeps running.
- The limit applies per UID, i.e. exactly per script.
- Memory: each worker costs about 12 MiB; 24 workers needed a box peak of 290 MiB.

## d) /proc and siblings 🟡

gVisor **silently ignores** `hidepid=2` as a mount option for `/proc`: the PID list is the same with and without the option. Visible to sibling scripts remain `cmdline` (artifact path, script name) and `status` (RAM, threads).

**Consequence:**
- Rule I2 tightened: argv **never** contains anything secret; that is the case anyway.
- Invariant I11 is reworded: "no access to memory, environment, signals and ptrace of siblings". Readable metadata is an accepted residual risk within one trust domain.

## e) Build in gVisor without network 🟡

```
error: Failed loading https://registry.npmjs.org/@esbuild%2flinux-arm64 for package "@esbuild/linux-arm64"
```

`deno bundle` loads **esbuild from npm on the first call** into `DENO_DIR`. Without network every build therefore fails, even with purely local imports. With a `DENO_DIR` that was filled once on the host and then mounted **read-only** (9.4 MiB), it works:

| Case (in gVisor, ro cache, no network) | Result |
|---|---|
| local imports | ✅ bundle on stdout (113 B), 0.58 s |
| `jsr:` import, with and without `--no-remote` | ✅ rejected: `Do not know how to load path: deno:jsr:@std/assert@1` |
| `deno check` with a type error | ✅ `TS2322 … Type checking failed.` |
| `deno bundle --check` with a type error | ✅ rejected (ran without cache, check needs no esbuild) |

**Consequence:**
- Besides `deno` and `tsbox-init`, the profile contains a **read-only `DENO_DIR` with esbuild for the target architecture**. `tsbox setup` fills it once with network and records the hash in the profile manifest.
- The output comes via stdout, sources are mounted ro, and the build does not need `/tmp`.

## f) OOM behavior ❌ (`oom.group` rejected, better behavior found)

`host -mode oom -mem 192`: an idle script (UID 10001) and a memory hog (UID 10002, an 8 MiB ArrayBuffer every 100 ms).

| Variant | Result |
|---|---|
| runsc creates the cgroup (default) | **Only the hog dies** (`signal: killed` at 152 MiB). The other script and init keep running, the sandbox survives. `memory.events: oom 1, oom_kill 1` |
| cgroup pre-created, `memory.oom.group=1` | ⚠ **No limit any more:** peak 2.5 GB at "192 MiB". If the cgroup already exists, runsc sets no `memory.max` |

- **Explanation:** under systrap every process in the sandbox has a stub process on the host. The host OOM killer picks the stub with the largest RSS, and the sentry reports the process as ended by SIGKILL. In effect this is an OOM per script. In 3 of 3 runs the hog was hit (oom.sh, rss.sh, reproduction of the hang).
- **Observed once and not reproduced:** on the first run with 40 workers in 256 MiB **the whole sandbox hung** at `memory.current = 255 MiB`. Even `runsc ps` stopped answering. Only a hard kill helped.
- **`cgroup.kill`** (cgroup v2) ends a box reliably in under 100 ms, even if runsc no longer reacts (`exit status 137`).

**Consequence:**
1. `memory.oom.group` is dropped.
2. tsbox reads `memory.events`. If `oom_kill` rises while a script ends with a SIGKILL that init did not send, the reason is **`oom`** (script).
3. Restart and breaker work per script.
4. **Box watchdog:** the host pings init over the channel. If the pong does not come, `cgroup.kill` follows and the box becomes `quarantined`. The same applies if the sandbox itself dies.
5. tsbox also sets `oom_score_adj` for the box processes, so that the host OOM killer hits boxes first.
6. Because the limits would otherwise have no effect, tsbox never pre-creates the cgroup itself.

## g) Script end despite open stdin ✅

| Variant (Windows and Linux) | Result |
|---|---|
| `for await` over `Deno.stdin.readable` | runs forever (timeout) |
| `reader.cancel()` after the main module ends | reading ends with `done`, **the process ends with 0** |
| `reader.releaseLock()` | unhandled error |

**Consequence:** `@tsbox/core` reads stdin only while calls are open, events are subscribed or `keepAlive()` applies, and otherwise cancels reading with `cancel()`.
- The **watchdog is reversed:** with an unref timer the script sends an `alive` every second. init ends it if that stops, because then the event loop is blocked. A ping from outside would force stdin reading and prevent the end.
- If the script exits, init sees the process end.

## h) Redirecting stdout ✅

`h_stdout2.ts` on Windows and Linux: `writeSync`, `write` and the getter `writable` can be replaced via `Object.defineProperty`. `node:process.stdout.write` goes through `Deno.stdout.writeSync` and is thus covered as well. All four paths end up in the log, none of them on fd 1.

Rated as **convenience, not a security boundary**: whoever writes raw bytes past `Deno.stdout` only damages their own channel and is ended with `protocol`. The message for this must be clear.

**Alternative fd 3/4 checked and rejected:**
- Natively, `--allow-read=/proc/self/fd/3 --allow-write=/proc/self/fd/4` works (tested; `environ` stays blocked).
- Under gVisor opening fails with EACCES because the pipe belongs to root. `fchmod` on the pipe is forbidden in gVisor without further capabilities.
- Windows has no `/proc`.

---

## 9. Further findings

1. **Memory per process is hard to measure in gVisor.** `VmRSS` of an empty Deno process is **≈ 100 MB**, because the mapped Deno binary counts too. `RssAnon`/`RssFile` are missing in `/proc/<pid>/status`, `shared = 0` in `statm`, and `smaps_rollup` does not exist.
   - A cgroup **inside** the sandbox is possible: cgroupfs v1 with memory, pids, cpu and job; init can create sub-cgroups. But it only **measures** the same value and **enforces no limit** (`limit_in_bytes = 96 MiB` set, growth up to 563 MB).
   - **Consequence:** the script RSS limit applies to `VmRSS − baseline`. init measures the baseline once per profile on an empty Deno process. The limit is approximate (±10 MiB). The hard limit remains the box cgroup, whose OOM hits the largest script according to f).
2. **Base cost of a box with the Go init: 13.0 MiB** (`memory.current` after 8 s idle); with an empty Deno script 30.7 MiB.
   - My expectation in the proposal ("4–6 MiB instead of 14") was wrong: the ≈ 14 MiB of the old model are already sentry and gofer.
   - What the Go init really saves is the Deno runner process, ≈ 12–18 MiB per box (measurements: [NOTES.md](NOTES.md)).
3. **`deno bundle` does not check types without `--check`:** a script with a type error is built (verified on Windows, exit 0). In tsbox the build therefore always runs with `--check`.
4. **Deno does not need `/tmp`:** with a non-writable `DENO_DIR` and without `/tmp`, a script starts in 100 ms (`console.log` works). **Consequence:** the box no longer gets a shared `/tmp`. This closes the channel between scripts from b).

Also noticed in passing: `deno bundle` knows `--no-remote` (for the dev build without a sandbox).

## 10. Not checked in the spike (covered later by the tests)

- Windows job objects (RAM, CPU, process count) in the dev driver
- macOS
- gVisor E2E on GitHub runners
- `oom_score_adj` for sentry and gofer
- Long-term behavior

## 11. Reproducing

The spike code is removed; the measurements above can therefore no longer be repeated one to one. The
product's behavior (isolation, limits, watchdog) is checked by the E2E tests: under WSL as root with
`bash scripts/wsl-test.sh` (dev and gVisor), on Windows with `go test ./...`.
