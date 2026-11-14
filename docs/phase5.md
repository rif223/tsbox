# tsbox – Phase 5 (benchmarks, build cache, KVM), as of 2026-11-03

## Scope

| Part | Location | Status |
|---|---|---|
| Build cache by input hash: unchanged scripts are not rebuilt (host and build sandbox) | `internal/build/cache.go` | ✅ |
| Selectable gVisor platform: `systrap` (default) or `kvm`; `WithGVisorPlatform`, `TSBOX_PLATFORM`, `tsbox daemon --platform`, `doctor` shows KVM | `internal/driver/gvisor`, `engine.go` | ✅ (kvm not measurable locally, see below) |
| Benchmarks: start time per box and script, stop, RAM per box and script, idle CPU, build cold and from the cache | `bench_test.go` | ✅ |
| CI: benchmarks in the job `gvisor` with systrap and (if available) kvm, plus isolation tests under kvm | `.github/workflows/ci.yml` | 🟡 written, not yet run |
| Race when storing `@tsbox/core` for parallel builds fixed | `internal/build/build.go` | ✅ |
| Diagnostics for tsboxd: stack dump when an API call hangs (20 s) or exiting hangs (30 s), log lines "tsboxd exiting/exited" | `internal/daemon` | ✅ |

## Build cache

The key is a SHA-256 over:
- the cache version and the hash of `@tsbox/core`,
- the toolchain: mode (host or build sandbox) and `deno --version` of the building Deno, remembered per binary
  by path, size and modification time (under gVisor the rootfs's Deno),
- the entry relative to the context,
- paths and contents of all files in the context (the script's directory),
- the import map: names of the SDK modules and contents of their directories.

An entry `<state>/artifacts/cache/<xx>/<key>` contains the artifact hash. A hit counts only if the artifact is
in the store and its content matches the hash; otherwise the script is rebuilt. init checks the hash before
every start anyway (I9). Failed builds are never cached; so a type error is noticed even after an earlier hit.

- **Build sandbox (gVisor):** complete. The sandbox sees only the hashed directories.
- **Host (dev):** a script can also import files outside its directory there. Before the build, `deno info
  --json` determines the whole module graph, including pure type imports. Every file outside the hashed
  directories is stored with its SHA-256 in the entry and checked on lookup. Hashing happens before the build:
  if a file changes meanwhile, the hash does not match next time and the script is rebuilt. If the graph cannot
  be resolved completely, the build runs without cache. (At first the cache checked the module comments in the
  bundle; type imports were missing there. Addendum 2026-11-05.) `deno info` gets the entry as a `file://` URL:
  otherwise Deno reads a Windows path `C:\…` as the scheme `c:` and reports the module as "external".
- **Limits:** above 5000 files or 64 MiB in the context the build runs without cache.
- **Turning it off:** `WithoutBuildCache()` (engine) or `Options.NoCache` (build).

Tests:
- `TestBuildCache`: hit; rebuild on changes in the script directory, the SDK module or the toolchain; `NoCache`;
  type error; returning to the old state hits again; tampered artifact; imports outside the directory (code and
  type imports) are cached with their hash, and every change there rebuilds.
- `TestCacheLimits`.
- `TestBuildCacheEngine` (E2E): the second `Create` of the same script in 4 ms; `WithoutBuildCache` rebuilds.

## Platform kvm

`gvisor.New` checks the platform: `systrap` runs everywhere, `kvm` needs read and write access to `/dev/kvm`.
If the device is missing, a clear message follows ("platform kvm needs /dev/kvm … without KVM use platform
systrap"). If the platform is set explicitly, `auto` then does not switch silently to the dev driver.
`tsbox doctor` reports whether KVM is usable.

**Not measurable locally:** WSL2 on the test machine has no `/dev/kvm` (no nested virtualization). So only the
error path is tested (`TestPlatform`, E2E with `TSBOX_PLATFORM=kvm`). The CI job `gvisor` measures on GitHub
runners that offer KVM. There `TestHello`, `TestEscape`, `TestSiblingsAndNetwork` and `TestBoxOOM` as well as
the benchmarks run with `TSBOX_PLATFORM=kvm`.

## Benchmarks

```bash
go test -run '^$' -bench . -benchtime 3x .                              # dev
TSBOX_TEST_DRIVER=gvisor go test -run '^$' -bench . -benchtime 3x .     # Linux, root
```

Measurements (mean of 3 runs; box with default limits 0.5 CPU, script `service.ts` idle):

| Metric | Windows 11, dev | WSL2, dev | WSL2, gVisor (systrap) |
|---|---|---|---|
| box until init ready (box-ms) | 62 | 6 | 1443 |
| 1 script until ready (ready-ms) | 612 | 45 | 2207 |
| 4 scripts until ready (ready-ms) | 1957 | 53 | 10590 |
| stop (stop-ms), 1 / 4 scripts | 72 / 61 | 71 / 55 | 129 / 321 |
| idle RAM of the box, 1 script (box-MiB) | 51 | 41 | 26 |
| RAM of the box, 4 scripts | 137 | 71 | 60 |
| ⇒ RAM per additional script | ≈ 29 MiB | ≈ 10 MiB | ≈ 12 MiB |
| idle CPU, 1 / 4 scripts (thousandths of a core) | 0 / 16 | 3 / 7 | 41 / 167 |
| build cold (build-ms) | 3108 | 921 | 15049 |
| build from the cache (build-ms) | 1.4 | 0.3 | 0.5 |

Interpretation:
- **Build cache:** under gVisor an unchanged build now costs 0.5 ms instead of 15 s (phase2.md, open point 1).
  The gVisor E2E run of the root package dropped from 542 s to 411 s as a result.
- **Script start under gVisor:** about 2.1 s per script. With 4 scripts it is 10.6 s, because the scripts share
  0.5 CPU; Deno needs CPU at start (under gVisor plus system calls via systrap). Whoever wants faster starts
  gives the box more CPU or uses kvm. (The 10.6 s turned out to be an outlier, see addendum 1.)
- **Windows dev:** here too the job object limits the box to 0.5 CPU. 4 scripts take 2 s.
- **RAM:** RAM per script is the difference between box RAM with 4 and with 1 script, divided by 3. Under gVisor
  the box with one script is smaller than under dev, because the sentry's shared memory (memfd) does not count
  in the box's `memory.current`. `rss-MiB` shows the RSS per script only in the dev driver; under gVisor an idle
  script is below the calibrated baseline (`VmRSS − baseline`, spike finding 1) and is reported as 0.
- **Idle CPU** under gVisor: about 4 % of a core per script (heartbeat, systrap stubs, timers of the sentry);
  barely measurable under dev.
- The Windows values come from Go for amd64 (emulated on ARM64), hence slower.

## Findings

1. **Race when storing `@tsbox/core`:** every build rewrote the core files (truncate, write) while parallel
   builds read them. In the load test a build saw an empty `transport.ts` ("has no exported member
   'transport'", WSL dev). Now the directory is written once per core version into a sibling directory and
   renamed atomically (marker `.complete`).
2. **Outlier `TestRestore` in the Windows full run with `-count=2`:** reproduced twice. The old daemon did not
   answer at all after the restart command (not even `shutdown` within 30 s), the new one waited 40 s for the
   lock and gave up. Alone the test is green 4/4, in the full run with `-count=1` too. On the third attempt the
   whole machine stood still for about 13 min (builds aborted after 2 min, the watchdog fired in the dev driver,
   the old daemon exited after 13 min in 219 ms). Cause still open. So there is now, for diagnostics:
   - If an API call without a stream (`info`, list, `inspect`, `shutdown`) hangs for 20 s, tsboxd writes all
     goroutines to the log once.
   - The same if exiting takes longer than 30 s.
   - Log lines "tsboxd exiting" and "tsboxd exited" with the duration.
   - The test helper `shutdown` waits up to 30 s and no longer treats a timeout under load as "daemon gone".
   - If a CLI test fails, `tsboxd.log` appears in the test output.
3. **One-time failure of `TestLoadManyBoxes`** in a Windows full run (79 s instead of about 40 s). The message is
   missing because the output was filtered. After that the test alone and the full runs with `-count=1`,
   `-count=2` and `-count=3` were green. Cause open; it is probably not related to the fixed race (finding 1),
   because the run already had the fix.
4. **Continuous run with the machine awake (2026-11-04):** 5 full runs in a row, 2× `-count=3` and 3×
   `-count=2` with `TSBOX_LOAD_BOXES=16`, i.e. 12 complete passes in 20 min. All green, no outlier, no stack
   dump. The outliers from findings 2 and 3 only occurred in runs in which the machine demonstrably stood still
   or went to sleep, or shortly afterwards. So they are most likely a consequence of that (time limits that had
   expired after waking up) and not a bug in tsbox. The diagnostics stay in.

## Addenda (2026-11-05)

1. **Script start under gVisor:** measured again, with a thread metric and a configurable CPU
   (`TSBOX_BENCH_CPU`, 5 runs each):

   | 4 scripts until ready | Time |
   |---|---|
   | 0.5 CPU (default) | 3.1–6.1 s (strongly fluctuating; the earlier value of 10.6 s was an outlier) |
   | 2 CPU | 1.7 s |

   - A script has 6–7 threads. The V8 flag `--single-threaded` (5 threads) brought no measurable gain.
   - Starting 4 scripts costs about 0.85 core-seconds. At 0.5 CPU that would be 1.7 s in theory; measured is
     2–3× more, apparently throttling losses of the CFS quota under gVisor.
   - This cannot be fixed within the limit: starting one after the other does not reduce the total CPU.
   - Only a temporary CPU boost at box start would help. It softens the hard CPU limit and is therefore
     built in only as opt-in (addendum 5).
2. **`stats` right away:** init sends the first measurement of a script right after its first measurement
   (≈ 0.5 s), not only with the next `pstats` round (up to 5 s). `TestIdentityAndStop` checks this: RSS or
   threads of both scripts are known at the latest 2 s after ready (phase3.md, open point 6).
3. **The build cache in dev mode** now also covers imports outside the script directory (see above).
   `TestBuildCache` checks: hit with code and type imports outside; a change to the code rebuilds; a change to a
   pure type import that makes the build fail is detected.
4. **`tsbox setup` actually run** (WSL, root, systemd running; the rootfs existed):
   - group `tsbox` created, unit written, `systemctl enable --now tsboxd` run.
   - `doctor` is completely green afterwards; the system daemon uses gVisor.
   - The socket has `0660 root:tsbox`, tsboxd runs with `oom_score_adj` −500 (`OOMScoreAdjust`).
   - `tsbox run` runs via the system daemon. A box with `--restart always` runs again after
     `systemctl restart tsboxd`.
   - As `nobody` with the group `tsbox`, `tsbox ps` works; without the group the message is "permission denied
     – member of the group tsbox?".
   - The E2E tests (dev and gVisor) stay green with the system daemon running.
   - The service stays installed in WSL. Remove it with `systemctl disable --now tsboxd`,
     `rm /etc/systemd/system/tsboxd.service` and `groupdel tsbox`.
5. **CPU boost at start (opt-in):** `Limits.StartCPU`, on the command line `--start-cpu`, in `tsbox.json`
   `"startCpu"`.
   - The box starts with `StartCPU` cores. `endStartBoost` resets the limit to `CPU` as soon as no script is
     starting any more (all ready, ended, in backoff or broken), after 30 s at the latest.
   - Restarts of single scripts get no boost.
   - If the limit cannot be reset (driver without `SetCPU` or an error while writing), the box is ended. It
     never runs above its limit for long.
   - Implementation: under gVisor `cpu.max` of the cgroup (`cgroup.SetCPU`), under dev/Windows the CPU rate of
     the job object. dev on Linux/macOS sets no CPU limit, so there is no boost there.
   - Validation: `StartCPU` must be 0 or between `CPU` and 64.

   Effect (`TSBOX_BENCH_START_CPU=2`, 5 runs each, measured twice):

   | until ready | without boost | `StartCPU 2` |
   |---|---|---|
   | gVisor, 1 script | 1.7–2.1 s | 1.36 s |
   | gVisor, 4 scripts | 5.4 s | 1.65 s |
   | Windows dev, 4 scripts | 1.96 s | 2.04 s |

   On Windows the CPU is not the limit: even a fixed limit of 2 CPU changes nothing there (1.95 s). The time goes
   into process start. Under gVisor the idle value right after a start with boost is somewhat higher (102–126
   instead of 65–68 thousandths of a core): the scripts still do some work that without the boost already fell
   into the start time.

   Test `TestStartCPU` (dev and gVisor):
   - With `service.ts` the boost ends after ready.
   - With `noready.ts` (without `ready()`) it ends only after the deadline (3 s in the test).
   - Under gVisor `cpu.max` contains `50000 100000` afterwards.
   - `StartCPU < CPU` is rejected.

   Plus `cgroup.TestSetCPU` and the field `startCpu` in `project.TestParse`.

## CLI review (2026-11-06)

Every command was run with valid, invalid and awkward arguments (quotes, spaces and umlauts in paths, flags
before and after arguments, missing and surplus arguments, Ctrl+C) in Windows PowerShell 5.1, cmd.exe, Git Bash
and WSL (dev and gVisor). Bugs found and fixed:

1. **`sdk rm`/`sdk add` on Windows:** tsboxd keeps the provider processes of external SDKs running; their open
   files blocked the removal, and `rm` had already deleted the entry in `lock.json` (half-removed SDK). Now the
   CLI asks every reachable tsboxd to end the providers first (`POST /v1/sdks/{name}/release`,
   `Engine.ReleaseSDK`); a running box that uses the SDK stops the command with a clear message. Installing and
   removing are atomic (copy next to the target, then rename; `lock.json` only changes when the files are
   gone; renames retry on Windows because of virus scanners).
2. **Providers got `grants: null`** when a script ran without `--grant`; the provider from the `sdk init`
   template crashed on it. The engine now always sends objects (`grants`, `config`), the templates tolerate
   both.
3. **`sdk init --provider go` on Windows:** the printed build command produced `provider` without `.exe`, and
   `&&` does not work in Windows PowerShell. Now: `go build -C <dir>/provider -o provider.exe .`.
4. **`types` ignored surplus arguments** and overwrote existing import entries of the project in `deno.json`;
   now it rejects arguments, needs an existing `--dir` and leaves foreign entries alone.
5. **German leftovers** in two error messages of the manifest check.

Improvements:
- All commands: `-h`/`--help` prints the help on stdout and exits with 0; flags appear as `--name`/`-x`.
  Errors print one line, the usage line and a hint instead of the whole flag list; unknown flags and commands
  get a suggestion ("did you mean --mem?"); surplus arguments are errors (`ps`, `events`, `doctor`, …).
- Readable messages for invalid values (durations need a unit, numbers a dot), sizes (`--mem 1Ki`, `MB` is
  accepted), negative durations, a directory instead of a script, `--driver`, host patterns given as URLs,
  `logs --script` with an unknown name, `tsbox.json` errors with line and column.
- `--params-file`; for `--params` without quotes the message explains the quoting of PowerShell and cmd.
- The CLI writes only ASCII (Windows PowerShell 5.1 garbles UTF-8 in pipes and redirections).
- A limit end is reported in one line (`main ended: rss - …`), durations without trailing zeros (`10m`).

Tests: `TestParsePos`, `TestHelp`, `TestSuggest` (argument handling), `TestAddRemoveAtomic` (registry),
`TestSDKLifecycle` (E2E: SDK without grant, in use, reinstall, removal), extensions of `TestRunExitCodes`.

Known limits: stack traces of scripts point to the bundled artifact (no source maps); Windows PowerShell 5.1
drops empty arguments (`--name ""`) before tsbox sees them.

## Tests (2026-11-05)

| Environment | Unit | E2E engine | E2E CLI/daemon | Benchmarks |
|---|---|---|---|---|
| Windows 11 ARM64, dev | ✅ | ✅ 12 passes (finding 4) | ✅ 12 passes (finding 4) | ✅ |
| WSL2 Ubuntu 24.04, dev | ✅ | ✅ | ✅ | ✅ |
| WSL2, gVisor (systrap) | – | ✅ | ✅ | ✅ |
| gVisor kvm | – | error path only (no `/dev/kvm`) | – | CI |

## Open points

1. Outliers `TestRestore`/`TestLoadManyBoxes` (findings 2–4): not reproducible with the machine awake. If one
   occurs again, the stack dump from the new diagnostics shows the cause.
2. kvm can only be measured in the CI; the CI job has not run yet.
3. Script starts under gVisor with 0.5 CPU stay slow without the boost. With `StartCPU` (opt-in, addendum 5)
   they are 3× faster. Decided (2026-11-05): the boost stays opt-in, the default is 0.
