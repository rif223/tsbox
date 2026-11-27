# tsbox – Notes

Measurements, design decisions, test setup, platform pitfalls and known limits. The architecture itself is in
[ARCHITECTURE.md](ARCHITECTURE.md); the gVisor experiments the design is based on are in
[gvisor-spike.md](gvisor-spike.md); the comparison of Firecracker and gVisor in
[firecracker-test.md](firecracker-test.md).

## 1. Measurements

`bench_test.go` (`BenchmarkBox`, `BenchmarkBuild`): box with default limits (0.5 CPU), idle script
`testdata/scripts/service.ts`, mean of 3 runs.

```bash
go test -run '^$' -bench . -benchtime 3x .                              # dev
TSBOX_TEST_DRIVER=gvisor go test -run '^$' -bench . -benchtime 3x .     # Linux, root
TSBOX_TEST_DRIVER=firecracker go test -run '^$' -bench . -benchtime 3x . # Linux, root, KVM
```

**Snapdragon X Elite laptop (ARM64):**

| Metric | Windows 11, dev | WSL2, dev | WSL2, gVisor (systrap) |
|---|---|---|---|
| box until init ready | 62 ms | 6 ms | 1.44 s |
| 1 script until ready | 0.61 s | 45 ms | 2.2 s |
| 4 scripts until ready | 1.96 s | 53 ms | 3.1–6.1 s (strongly fluctuating) |
| stop, 1 / 4 scripts | 72 / 61 ms | 71 / 55 ms | 129 / 321 ms |
| RAM of the box, 1 / 4 scripts | 51 / 137 MiB | 41 / 71 MiB | 26 / 60 MiB |
| idle CPU, 1 / 4 scripts (thousandths of a core) | 0 / 16 | 3 / 7 | 41 / 167 |
| build cold / from the cache | 3.1 s / 1.4 ms | 0.9 s / 0.3 ms | 15 s / 0.5 ms |

**AMD Ryzen 7 4800H, WSL2 (Firecracker nested under Windows):**

| | firecracker | firecracker + `StartCPU 2` | gvisor (systrap) | gvisor + `StartCPU 2` | gvisor (kvm) |
|---|---|---|---|---|---|
| box until init ready | 1.06 s | 0.93 s | 1.19 s | 1.19 s | 1.24 s |
| 1 script until ready | 1.49 s | 1.07 s | 1.46 s | 1.33 s | 1.56 s |
| 4 scripts until ready | 1.84 s | 1.15 s | 2.38 s | 1.47 s | 2.83 s |
| RAM of the box (cgroup), 1 / 4 scripts | 45 / 78 MiB | 45 / 78 MiB | 26 / 61 MiB | 25 / 61 MiB | 28 / 62 MiB |
| idle CPU, 1 / 4 scripts (thousandths of a core) | 12 / 22 | 11 / 21 | 15 / 50 | 15 / 44 | 9 / 25 |

Interpretation:
- **Build cache:** an unchanged script costs well under 1 ms instead of a build (15 s in the gVisor build sandbox).
- **Script start under gVisor** needs CPU (Deno start plus system calls via systrap). With 0.5 CPU, 4 scripts
  share half a core; the CFS quota loses 2–3× more than the pure CPU time. `StartCPU` (start boost, opt-in)
  helps most; more `--cpu` or the kvm platform help less in a nested setup.
- **Windows dev:** the time goes into process start, not into the CPU limit; the boost changes nothing there.
- **RAM:** under gVisor the sentry's shared memory is not counted in the box's `memory.current`. Firecracker's
  value is the guest's box cgroup; on the host every VM needs its guest kernel and its own page cache of the
  Deno binary on top (about 60 MiB, firecracker-test.md).
- **Idle CPU** under gVisor is about 4 % of a core per script (heartbeat, systrap stubs, sentry timers);
  Firecracker and dev are close to native.
- On a laptop that sleeps or stands still, time limits in tests expire; outliers in such runs are no bugs.

## 2. Design decisions

**Daemon and access**
- Windows uses a **named pipe** instead of AF_UNIX: `connect` on AF_UNIX failed with `WSAEINVAL` and left
  socket files behind that could no longer be deleted. The pipe `\\.\pipe\tsboxd-<hash(SID, state)>` has a DACL
  for the user only; the client checks with `GetNamedPipeServerProcessId` that the server belongs to the same
  user, so `env:` secrets never reach a foreign pipe. Dependency `github.com/Microsoft/go-winio`, Windows only.
- Unix socket paths longer than 100 characters move to `$XDG_RUNTIME_DIR/tsbox/<hash>.sock` or
  `/tmp/tsbox-<uid>/<hash>.sock` (AF_UNIX allows 108 bytes). A lock per state directory prevents two daemons on
  the same state; root without systemd uses the user socket.
- Access to tsboxd corresponds to the `docker` group (I12): system socket `0660 root:tsbox`.
- `env:` secrets: the CLI sends only the named variables, tsboxd keeps them in memory only and never falls back
  to its own environment; after a daemon restart such a box starts again only with `tsbox start`, which passes
  them again (`TestSecretsNotPersisted` with a canary value).
- Installed SDKs live in the daemon's state; for the system daemon `sdk add` runs as root. `sdk add`/`sdk rm`
  first ask every reachable tsboxd to end the SDK's provider processes and refuse while a running box uses it;
  installing and removing are atomic (copy next to the target, rename; `lock.json` changes last).
- **tsboxd as a service** (like Docker's engine): `tsbox setup` on every system. Windows uses a logon entry in
  `HKCU\...\Run`, not a Windows service: the only driver there is dev (no isolation), and a service would run
  scripts as SYSTEM. The entry runs `tsbox daemon start`, which starts tsboxd without a window and exits, so
  the console Windows opens for it closes at once. Autostart on demand stays as the fallback when nothing is
  set up; with a service set up but stopped the CLI refuses to start a second daemon on Linux and macOS (it would
  hold boxes nobody looks for, e.g. a user daemon next to a stopped system daemon); on Windows it starts the
  same permanent daemon the logon entry would.
- **Driver per box** (like Docker's runtimes): one daemon runs boxes with every usable driver, and every
  box names its own: there is no default driver (a box without one is refused; `TSBOX_DRIVER` is ignored with a
  warning in the daemon log). Separate daemons per driver were a workaround with no gain in isolation: the
  system daemon runs as root anyway. Drivers load on first use; a failed load is retried after 30 s, so a
  driver set up later works without a restart. Creating a box stores its driver. Isolating and other builds have separate slot limits, so a dev build never waits for a
  sandbox build. gVisor and Firecracker share the artifact cache (both build with the rootfs's Deno).

**Build**
- Removing a box deletes its directory first and forgets the box afterwards: if the directory cannot be
  deleted, the box stays listed (`ps -a`) and `rm` can be repeated, instead of an unknown box whose directory
  is left behind.
- A script is `running` as soon as any frame of its own arrives; init forwards the first heartbeat of every
  start for that, so scripts that never log or only log through `@tsbox/log` (call frames) do not stay
  `starting`. The protocol version stays 2: the host only accepts one more type from init, and init and host
  are always the same binary. `ready` stays the script's opt-in; the start boost waits for it as before.
- The scripts of one box are built in parallel (at most 4, `GOMAXPROCS`); errors are reported in script
  order. All real builds of an engine share a limit (cache hits do not count): 4 with dev, 2 with gVisor and
  Firecracker, where a single build may use up to 768 MiB. Measured on Windows (dev, Deno 2.9.6), one box with 3 changed scripts: `up -d` 6.9 s → 3.5 s.
- **Where a build's time goes** (Windows, dev, one changed small script, state outside the virtualization):
  `deno info` (cache key) 0.2 s, `deno bundle --check` 1.9–2.5 s, of which the type check is ≈ 1.65 s (also as a
  separate `deno check`, Deno's check cache does not help after a change; bundling alone 0.2 s). Splitting
  check and bundle or running them in parallel would save ≤ 0.2 s; an opt-in `--no-check` would save the
  1.65 s but give up the guarantee that type errors abort, so it is not offered. `WaitDelay` (5 s) is not
  involved: deno exits normally.
- Builds always run with `--check` (without it `deno bundle` does not type-check) and with `--no-config
  --import-map`: otherwise Deno uses the nearest `deno.json` for modules in other directories, which bundled
  `@tsbox/core` twice and allowed imports of SDKs that were not activated.
- **Build sandbox / build VM:** under gVisor and Firecracker the build runs without network as UID 65534; it sees
  only the script's directory, the SDK modules and `@tsbox/core`. esbuild comes from a read-only `DENO_DIR` that
  tsbox fills once on the host (needs network once).
- **Build cache:** key = cache version, hash of `@tsbox/core`, toolchain (mode and `deno --version`, remembered
  per binary path, size and mtime), entry, paths and contents of all files of the script's directory, import map
  and SDK directories. On the host (dev) `deno info --json` adds every file outside the directory (also pure type
  imports) with its hash; if the graph cannot be resolved, there is no cache. Above 5000 files or 64 MiB the
  build runs without cache. Failed builds are never cached. `WithoutBuildCache()` turns it off.
- **Artifact store cleanup:** artifacts, cache entries, work directories of other core versions and leftovers
  of interrupted builds are removed after 7 days without use, checked at most once per hour and engine after a
  build. A use refreshes the modification time (at most once a day). Boxes keep their own artifact copies, so
  a removed entry only costs a rebuild.
- An SDK's module must be in the root directory of the SDK (the build sandbox mounts that directory).
- `@tsbox/core` is written once per version into a sibling directory and renamed atomically (parallel builds
  once read a half-written file).

**Boxes**
- Under gVisor runsc gets `--overlay2=none` and an empty read-only `/tmp`: without its own entry runsc mounts a
  writable tmpfs at `/tmp` that all scripts of a box share (found by the escape simulation).
- **`oom_score_adj` 500** for all host processes of a box, set via `tsbox __oomexec … -- runsc`: in the OCI
  spec runsc applies it only to sentry and gofer, not to the systrap stubs, and then the cgroup OOM hits the
  sentry (the whole box) instead of the largest script. tsboxd runs with `OOMScoreAdjust=-500`.
- **Start boost** (`Limits.StartCPU`, `--start-cpu`, `"startCpu"`) is opt-in (default 0) because it softens the
  hard CPU limit. The box starts with `StartCPU` cores and falls back to `CPU` when no script is starting any
  more, after 30 s at the latest; if the limit cannot be reset, the box is ended.
- **Profiles:** `default` = `TSBOX_ROOTFS`, others at `TSBOX_PROFILES/<name>`; the fingerprint is in
  `BoxStatus` and `doctor`; the dev driver ignores profiles; builds use the Deno of `default`.
- `kv` stores a JSON file per storage area (enough for ≤ 1 MiB per script); a different backend later for
  larger data.

**Firecracker driver**
- tsbox is PID 1 in the microVM (`__fcinit`): read-only rootfs image (ext4 from the profile's rootfs plus the
  tsbox binary, built once, content-addressed, pruned after 7 days unused), `/proc` with `hidepid=2`, minimal
  `/dev`, empty read-only `/tmp`, hardening sysctls (no user namespaces, no unprivileged BPF).
- Host and guest talk only over vsock: setup (header and tar stream with the files), the unchanged init channel,
  stats. Every guest connection first sends a greeting, because a connect that timed out in the guest may still
  arrive at the host; the guest waits for the host before it ends the VM, so that no data is lost.
- The box limits are a cgroup in the guest (the guest kernel ends the largest script on OOM); the host cgroup of
  the VM enforces the CPU limit (start boost via `cpu.max`). VM RAM = box memory + 64 MiB.
- The jailer is opt-in (`TSBOX_FC_JAILER=1`): chroot, own UID/GID per VMM (from 63000), own mount namespace;
  without `--new-pid-ns` (the jailer would fork and tsbox could not wait for the VMM).

**CLI**
- All output is ASCII: Windows PowerShell 5.1 garbles UTF-8 in pipes and redirections.
- `-h` prints the help on stdout (exit 0); errors print one line, the usage line and a hint; unknown flags and
  commands get a suggestion.
- PowerShell 5.1 strips inner quotes of arguments: `--params '{\"n\":1}'` there, `"{\"n\":1}"` in cmd, or
  `--params-file`; it also drops empty arguments (`--name ""`).

## 3. Tests

```bash
go test ./...                                       # unit + E2E with the dev driver (all systems)
sudo bash scripts/wsl-test.sh                       # Linux/WSL as root: E2E with dev and gVisor
sudo TSBOX_DRIVERS=firecracker bash scripts/wsl-test.sh   # E2E with Firecracker (KVM)
TSBOX_FC_JAILER=1 TSBOX_TEST_DRIVER=firecracker go test -run 'TestFirecrackerJail' .   # as root
bash scripts/fuzz.sh                                # all fuzz targets
```

- **Attack suite** (`testdata/attack`): env, files, network, run, ffi, sys, `node:` modules, dynamic imports,
  workers; under gVisor and Firecracker additionally an escape simulation (`__probe`): a native process with its
  own UID fails at kill/ptrace/environ/mem of its siblings, at init, at `setuid(0)`, at every write path and at
  TCP, UDP and DNS.
- **Canary in the box memory** (`TestCanaryInBoxMemory`, Linux): a secret that only the provider on the host has
  is searched as ASCII, UTF-16LE and base64 in the memory of all host processes of the box (under gVisor
  including the sentry's memfd, under Firecracker the VM's memory); a marker passed on purpose must be found.
- **Load test** (`TestLoadManyBoxes`): 8 boxes (isolating drivers 4, `TSBOX_LOAD_BOXES`) × 2 scripts × 40 kv
  rounds next to a box under full load; every line belongs to the right box and script, nothing is dropped.
- **Fuzzing:** 21 targets – frames and payloads, manifest, grants and configuration, `tsbox.json`, log files,
  kv store, HTTP requests and host patterns, external provider frames, `lock.json`, daemon state and API requests,
  cgroup files, `/proc` parsers, NDJSON streams, the Go provider router, CLI flags. A finding:
  `ParseBytes("Inf")`, `"NaN"` and values from 2⁶³ gave negative sizes; they are rejected now.
- **CI** (`.github/workflows/ci.yml`): Linux, Windows, macOS with the dev driver, fuzzing, a gVisor job (systrap,
  plus kvm when the runner has `/dev/kvm`) and a Firecracker job (only on runners with KVM); all green since
  2026-11-15. On failure the kvm step prints `dmesg` and runsc's debug logs (`TSBOX_RUNSC_DEBUG=<dir>`).

## 4. Platform pitfalls

- **Windows: hanging builds.** `deno bundle` starts esbuild as a service; with parallel builds these processes
  occasionally inherited pipe handles of other builds and kept `cmd.Wait` waiting forever. Fix: `WaitDelay` and
  a job object with `KILL_ON_JOB_CLOSE` per build.
- **Windows: "Access is denied" on rename.** Virus scanners keep freshly written files open for a moment; all
  atomic renames go through `fsutil.Rename` with retries (up to ≈ 4 s).
- **Windows: Go 1.26 binaries are charged 32 MiB they never use.** Every Go program that imports
  `crypto/rand` has a 32 MiB `.noptrbss` buffer (`crypto/internal/fips140/drbg.memory`) that is meant to cost
  nothing until first use. Linux counts only touched pages; Windows charges the writable sections of an image
  in full to the private memory (commit) of the process, which is what the job object and the dev driver's
  watchdog measure. tsbox-init (the tsbox binary) thus showed about 57 MiB instead of about 24 MiB, and a
  box with one idle script 87 MiB of a 128 MiB limit. The dev driver on Windows now subtracts the size of the
  writable sections of the init binary (`debug/pe`) from the box's memory and adds it to the job's hard limit;
  init's heap still counts. Measured: 86.8 MiB → 57.1 MiB for one idle script.
- **Windows: packaged apps (MSIX) virtualize `%LOCALAPPDATA%`.** tsbox started from a terminal inside such an
  app writes its default state `%LOCALAPPDATA%\tsbox` to
  `%LOCALAPPDATA%\Packages\<family>\LocalCache\Local\tsbox`; a directory that exists at both places is merged.
  Since Go 1.25 `os.RemoveAll` opens entries relative to the parent handle with `OBJ_DONT_REPARSE`; bindflt
  answers that in a merged directory with `STATUS_REPARSE_POINT_ENCOUNTERED` (`ELOOP`, "too many levels of
  symbolic links", with go1.26.0 "unlinkat …: is a directory"), so `rm`, `down` and recreating boxes in `up`
  failed. All removals go through `fsutil.RemoveAll`, which falls back to deleting with plain paths (links
  are removed, never followed). Such processes may have no package identity
  (`GetCurrentPackageFamilyName` fails) and are still redirected, so `tsbox doctor` checks the effect: it
  creates a probe file in the state and looks for it below `Packages\*\LocalCache`. Builds there are about
  2.5–5 s slower (every file access of Deno goes through the filter); `TSBOX_STATE` outside `%LOCALAPPDATA%`
  and `%APPDATA%` avoids all of this. A state written from inside the app is not visible to tsbox outside it
  (`doctor` reports such copies).
- **Windows: V8 heap message.** On Windows a native stack trace follows V8's out-of-memory message, so init
  checks every stderr line on arrival.
- **WSL2 nested virtualization on AMD** was lost silently with Windows build `….9711` (KB5127648):
  `Hyper-V: Nested features: 0x0`, no `svm`, no `/dev/kvm`. The following cumulative update fixed it.
- **gVisor packaging:** from release 20261102 on `runsc` needs companion binaries in `gvisor-bin/` next to it,
  and the direct download `releases/release/latest/<arch>/runsc` returns 404; install the Debian package.
- **Firecracker CI images** (guest kernels, used by `tsbox setup --firecracker`) lag behind the releases;
  `setup` takes the newest minor version that has them.
- **vsock** connects time out after 2 s by default; the guest raises the timeout to 10 s.
- **gVisor with the kvm platform: box OOM.** Without systrap stubs the sentry is the only process in the box's
  cgroup, so the host's OOM killer ends the whole sandbox (not the largest script as with systrap). runsc removes
  the cgroup at once; the driver therefore reads `oom_kill` of the parent cgroup and reports `oom`.
- **macOS: `/var` is a symlink** to `/private/var`; path checks compare resolved paths on both sides (found in
  `fcproto.AddTree`).
- **dev driver on macOS** measures no RAM, RSS or threads; those limits do not apply there (tests skip them).
- **gVisor systrap on WSL kernel 6.6:** `runsc` fails with "starting sandbox: creating process: out of memory"
  on the AMD PC's WSL2 kernel 6.6.87 (31 GiB free); `--platform=kvm` works there. The ARM64 laptop's WSL2
  kernel 6.18 runs systrap fine. Since then the gVisor driver starts a test sandbox (`runsc do /bin/true`, once
  per runsc binary and platform) before it counts as usable; without an explicit platform it falls back to kvm
  when only kvm works, and `doctor` and `/v1/info` say why. `TSBOX_GVISOR_PROBE=0` skips the test.
- **Driver detection:** gVisor and Firecracker exist for amd64 and arm64 only; other architectures get a clear
  error. Without `/dev/kvm` the message says how to get it: WSL2 needs `nestedVirtualization=true` in
  `.wslconfig`; WSL2 on Windows on ARM (e.g. Snapdragon X) has no nested virtualization at all, so Firecracker
  and gVisor's kvm platform are not possible there (gVisor with systrap is).
- **Firecracker build context:** the build VM gets the script's whole directory as files in a tmpfs (gVisor mounts
  it instead). A script next to large data (found: a state directory of 364 MiB) broke the transfer with
  "broken pipe"; the host now refuses contexts above 192 MiB with a clear message.

## 5. Known limits and open points

1. Firecracker ran nested under Windows (AMD, WSL2) and on the GitHub runners (amd64 with KVM); a run on Linux
   on hardware and on aarch64 is missing.
2. kvm, amd64 Linux and macOS are only covered by the CI.
3. With the dev driver on Linux, a script that grows very fast can end as `oom` (whole box) instead of `rss`: both
   limits are polled, whichever notices first wins.
4. Scripts of one box are one trust domain (under gVisor they see each other's `cmdline` and `status`).
5. Side channels between boxes on one host are not addressed; there is no independent audit yet
   ([SECURITY.md](../SECURITY.md)).
6. `update` stops the old box before the new one starts (same name, storage and artifact directory): the
   scripts are down for stop plus start (about 0.1–2 s, see §1), not for the build. A rolling update per script
   inside a running box is not implemented.
7. Tenant quotas sum the box limits, not the measured use; a box that is started while another start of the same
   tenant runs waits for the quota check (`Engine.quotaMu`), so two starts cannot both fit.
8. `cron` ticks come from the host's clock; runs missed while the host slept or the script was not running are
   not repeated. `@every` counts from the session's open, not from the full minute.
9. The E2E tests of inbox, cron, kv v2, update and quotas (`e2e_features_test.go`) need Deno; the daemon and CLI
   paths of `update`, `emit` and `/v1/metrics` have no E2E test of their own yet.
