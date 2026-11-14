# tsbox – Phase 4 (hardening), as of 2026-11-03

## Scope

| Part | Location | Status |
|---|---|---|
| Canary test I2 in the box memory: secret at the provider, memory of all box processes searched (readable mappings via `/proc/<pid>/mem`, under gVisor additionally the sentry's memfd `runsc-memory`, which holds the box's memory), positive control with a marker | `e2e_linux_test.go` | ✅ |
| Named profiles: `default` = rootfs, others at `TSBOX_PROFILES/<name>`; `BoxSpec.Profile`, `run --profile`, `"profile"` in `tsbox.json`, fingerprint in `BoxStatus` and `doctor`, `build-rootfs.sh` with `PROFILE=`/`PACKAGES=` | `internal/profile`, `engine.go`, `internal/driver/gvisor` | ✅ |
| Path SDKs in directories with their own `deno.json` (build with `--no-config --import-map`) | `internal/build` | ✅ (fixed in phase 3, see phase3.md point 1) |
| Load test: 8 boxes (gVisor 4) × 2 scripts × 40 kv rounds in parallel next to a box under full load | `e2e_load_test.go` | ✅ |
| Fuzzing of all parsers: 21 fuzz targets, `scripts/fuzz.sh` | see below | ✅ |
| gVisor E2E in the CI: runsc 20261026.0 with checksum, rootfs with `build-rootfs.sh`, `TSBOX_DRIVERS=gvisor scripts/wsl-test.sh` | `.github/workflows/ci.yml` | 🟡 written, not yet run in the CI (no push) |
| `oom_score_adj` 500 for all host processes of a box (gVisor: runsc, sentry, gofer, systrap stubs; dev on Linux: init and scripts); tsboxd via systemd `OOMScoreAdjust=-500` | `internal/driver/{gvisor,dev}`, `internal/initbox` (`__oomexec`), `cmd/tsbox/doctor.go` | ✅ |
| Windows: hang in the full test run found and fixed, renames with retries | `internal/build`, `internal/fsutil` | ✅ |

## Results and findings

### 1. Canary in the box memory (I2)

`TestCanaryInBoxMemory` starts a box with the test SDK `secretlen` (external provider, secret via `env:`) and a
script that keeps a second marker, passed on purpose via `params`, in its heap. As soon as the script has
reported the length of the secret, the test reads the memory of all processes under the box's host process and
searches for the secret as ASCII, UTF-16LE and base64.

- **dev (WSL):** 2 processes (init, Deno), 438 MiB in 0.4 s. Marker found in init and in Deno's heap. Also green
  as a normal user (UID 65534, `ptrace_scope=1`), as later in the CI: the test may read the memory because init
  and Deno are its descendants. `TestOOMScoreAdj` likewise.
- **gVisor (WSL):** 14 processes (runsc, gofer, sentry, systrap stubs), 397 MiB in 2.6 s. Marker found in the
  sentry and in the memfd `runsc-memory`. So the scan demonstrably sees the box's memory.
- **Secret:** not found in any form.
- The systrap stubs cannot be opened via `/proc/<pid>/maps` (ENOENT). But their memory lives completely in the
  sentry's memfd, which the test reads without holes via SEEK_DATA/SEEK_HOLE.
- Linux only; on Windows and macOS the test is not built.

### 2. `oom_score_adj` (I6, host OOM)

First approach: `process.oomScoreAdj` in the OCI spec. runsc applies the value to sentry and gofer, but **not**
to the systrap stubs that map the scripts' memory pages. Consequence in the gVisor E2E: `TestBoxOOM` aborted
with `box_died` instead of `oom`. The cgroup OOM now hit the sentry (500) instead of the largest script (stub
with 0), i.e. the whole box.

Solution: the gVisor driver starts `runsc run` via `tsbox __oomexec <adj> -- runsc …`. The new role sets
`/proc/self/oom_score_adj` and replaces itself with runsc. So all processes of the box inherit the value from
the start. With the same value for all of them, the cgroup OOM still hits the largest script, and when the host
runs out of memory the kernel hits boxes first. The value in the OCI spec stays (identical, does no harm).
`TestOOMScoreAdj` now checks all 14 processes of the box (all 500). On Linux the dev driver sets the value for
init before the first script (only raising, possible without privileges). Configurable with
`WithOOMScoreAdj` or `tsbox daemon --oom-score-adj` (0 = unchanged).

### 3. Windows: hang in the full test run (phase3.md point 5, phase2.md point 2)

With `go test -count=3 -timeout 45m ./...` the hang was reproducible. The stack dump showed
`build.Bundle → exec.Cmd.Wait → awaitGoroutines`: `deno bundle` had ended, but the goroutine reading its
stdout/stderr pipe waited forever.

Cause: `deno bundle` starts esbuild as a service (`esbuild --service --ping`). With parallel builds on Windows
these processes occasionally inherit pipe handles of other builds, then wait for each other's stdin and keep
running. On the test machine there were such esbuild processes from November 1, 2 and 3, i.e. from all hangs so
far.

Solution:
- `cmd.WaitDelay = 5s`: `Wait` returns after deno has ended, even if a grandchild holds the pipes.
  `exec.ErrWaitDelay` counts as success, because deno exited with code 0.
- On Windows every build runs in a job object with `KILL_ON_JOB_CLOSE`, which ends all descendants after the
  build.

After that `go test -count=3 ./...` ran completely green (root package 3× in 420 s). The two long full runs
before had further causes: the machine went to sleep (run time 4388 s with a 6 min timeout).

`Access is denied` while renaming (TestEcho): `sdkreg.builtinDir` renames a freshly written directory that the
virus scanner keeps open for a moment. All atomic renames (build artifacts, sdkreg, state of tsboxd, log
rotation, kv) now go through `fsutil.Rename` with retries for up to ≈ 4 s (Windows only). The same phenomenon
also hits the `go` tool itself (`go: remove …\cgroup.test.exe: Access is denied` while fuzzing); that is
outside of tsbox.

### 4. Heap limit on Windows

In the first Windows run, `TestLimits/attack/heap.ts` once ended with `crash` (code 0x80000003) instead of
`heap`. The detection searched for V8's message only in the last 20 stderr lines. On Windows, however, a native
stack trace follows it: the message is in line 10 of 61. Now every stderr line is checked on arrival
(`script.heapOOM`). After that the case was green 4/4 (Windows) and green under gVisor.

### 5. Fuzzing

| Package | Targets | What |
|---|---|---|
| `internal/proto` | `FuzzRead`, `FuzzPayload` | frames, payloads of all types from init and scripts |
| `manifest` | `FuzzParse`, `FuzzScalars`, `FuzzGrantsAndConfig` | manifest; sizes, rates, grants, host patterns; values of all right and configuration types |
| `internal/project` | `FuzzParse`, `FuzzScalars` | `tsbox.json`, sizes, restart policy |
| `internal/logfile` | `FuzzRead` | broken log files (filter, tail) |
| `internal/providers/kv` | `FuzzKV` | damaged store file, parameters of all methods; quota and value limit hold |
| `internal/providers/httpp` | `FuzzFetch`, `FuzzAllowed` | requests from the box never connect to foreign hosts; host patterns |
| `internal/providers/extproc` | `FuzzReadLoop` | frames of a faulty provider process; open calls end |
| `internal/sdkreg` | `FuzzLock` | damaged `lock.json` |
| `internal/daemon` | `FuzzLoad`, `FuzzDecode` | state `tsboxd.json`, API requests |
| `internal/driver/cgroup` | `FuzzRead` | cgroup files |
| `internal/initbox` | `FuzzProcInfo` | `/proc/<pid>/status` and `stat` (parsers extracted for this) |
| `client` | `FuzzNDJSON` | streams of the daemon (parser `ndjson` extracted) |
| `sdk` | `FuzzRouter` | parameters for Go providers; errors always with a code |
| `cmd/tsbox` | `FuzzSince`, `FuzzSDKFlags` | `--since`, `--sdk/--grant/--sdk-config` |

**Finding:** `ParseBytes("Inf")`, `"NaN"` and values from 2⁶³ gave negative sizes (in `manifest` and
`project`), `ParseRate("Inf/s")` an infinite rate. They are rejected now. The CI runs all targets with
`FUZZTIME=15s bash scripts/fuzz.sh`.

### 6. Named profiles

- A profile is a rootfs directory with `/usr/local/bin/deno`.
  - `default` is `WithRootFS`/`TSBOX_ROOTFS`.
  - Every other profile `<name>` lives under `WithProfilesDir`/`TSBOX_PROFILES` (default
    `/var/lib/tsbox/profiles`).
  - Names: `[a-z0-9][a-z0-9_.-]{0,31}`.
- Under gVisor, `Create`/`Check` check that the profile exists. `Start` determines the fingerprint
  (`BoxStatus.ProfileHash`) and passes the rootfs to the driver.
- The dev driver uses the host's Deno and ignores profiles; only the name is checked.
- The build sandbox keeps building with the Deno of `default`.
- `tsbox doctor` lists all profiles with Debian version, Deno size and fingerprint.
- `PROFILE=deno-next DENO=/opt/deno-next scripts/build-rootfs.sh` builds a profile; `PACKAGES=a,b` adds packages
  (`debootstrap --include`).
- Tests: `internal/profile` (resolution, names, hash), `TestProfileNames` (all drivers), `TestProfiles` (gVisor:
  profile as a symlink to the rootfs, the box's `config.json` points to the profile, a missing profile fails
  before the build).

### 7. Load test

`TestLoadManyBoxes` starts N boxes with two scripts each at the same time. Every script does 40 rounds of
`kv.set`/`kv.get` with a log line. Next to them runs a box with an endless loop (CPU 1). Checked: all boxes end
regularly, every line belongs to the right box and the right script, the order is right, nothing is discarded
(`DroppedEvents() == 0`), the noisy box keeps running. The number of boxes can be set with `TSBOX_LOAD_BOXES`.

| Environment | Boxes | Duration |
|---|---|---|
| Windows 11 ARM64, dev | 8 | 38 s |
| WSL2, dev | 8 | 9 s |
| WSL2, gVisor | 4 | 21 s |

### 8. gVisor in the CI

New job `gvisor` (ubuntu-24.04, 90 min):
1. Install `runsc.deb` from the gVisor pool in a fixed version (20261026.0, as locally) with SHA-256. gVisor's
   apt index only lists the newest version.
2. Install debootstrap and build the rootfs with `scripts/build-rootfs.sh` and the Deno from `setup-deno`.
3. Run `sudo … TSBOX_DRIVERS=gvisor bash scripts/wsl-test.sh -v`. The script now understands `TSBOX_DRIVERS`
   for this.

The job is checked with shellcheck, but has never run on GitHub. Whether runsc with systrap and cgroup v2 works
on the runners as in WSL will only show on the first push.

## Tests (2026-11-03)

| Environment | Unit + fuzz seeds | E2E engine | E2E CLI/daemon |
|---|---|---|---|
| Windows 11 ARM64, dev | ✅ | ✅ `-count=3`, without hang (gVisor and Linux tests skipped) | ✅ |
| WSL2 Ubuntu 24.04, dev | ✅ | ✅ | ✅ 7/7 |
| WSL2, gVisor | – | ✅ | ✅ 7/7 |

## Open points

1. The CI job `gvisor` is untested (see 8).
2. The test machine still has esbuild process objects from earlier hangs (November 1–3). They report
   `HasExited` and no longer block anything; a reboot clears them.
3. Unchanged from phase 3: `setup` only tested with `--dry-run`; `stats` shows RSS/threads only after the first
   `pstats` report. (Both done in phase 5, see phase5.md, addenda 2 and 4.)

## Demo

```bash
# Windows: full run as used to narrow down the hang
go test -count=3 -timeout 45m ./...
bash scripts/fuzz.sh                                   # all fuzz targets, 10 s each
TSBOX_LOAD_BOXES=16 go test -run TestLoadManyBoxes -v .
```

WSL with gVisor (as root):

```bash
bash <repo>/scripts/wsl-test.sh -run 'TestCanary|TestOOM|TestProfiles|TestLoad' -v
PROFILE=test DENO=$(command -v deno) bash scripts/build-rootfs.sh   # second profile (debootstrap, network)
tsbox doctor                                            # lists default and test with fingerprint
tsbox run --profile test examples/hello.ts
```
