# tsbox – Phase 3 (daemon and management), as of 2026-11-02

## Scope

| Part | Location | Status |
|---|---|---|
| `tsboxd`: engine with HTTP/JSON API (`/v1`, streams as NDJSON), state per box, restore after restart, event ring buffer (1000), idle exit | `internal/daemon`, role `tsbox daemon` | ✅ |
| Transport: Unix socket (user 0600, system 0660 `root:tsbox`), Windows: named pipe with a DACL for the user only and an owner check in the client | `internal/daemon/listen_*.go`, `client/addr_*.go` | ✅ |
| Go client (public): `List`, `Create`, `Attach`, `Inspect`, `Start/Stop/Restart/Remove`, `Logs`, `Stats`, `Events`, `Shutdown` | `client/` | ✅ |
| Autostart of the user daemon (detached, lock per state, replacement on a new build, `TSBOX_NO_AUTOSTART`, `TSBOX_IDLE`) | `cmd/tsbox/daemoncli.go`, `spawn_*.go` | ✅ |
| `run` via the daemon (foreground with exit codes and Ctrl+C, `-d`, `--rm`) | `cmd/tsbox/main.go` | ✅ |
| `ps`, `logs`, `stats` (live table), `stop`, `start`, `restart`, `rm`, `inspect`, `events` | `cmd/tsbox/boxcmd.go` | ✅ |
| Log files per script with rotation (2 × 10 MiB), `LogLine.Seq`, `Box.Logs`/`Engine.Logs` (Since, Script, Tail, Follow), `Engine.Remove` | `logs.go`, `internal/logfile` | ✅ |
| `env:` secrets: values from the caller, only in the daemon's memory (`SDKUse.Env`, `manifest.ResolveConfigEnv`) | `sdkcall.go`, `manifest/` | ✅ |
| `tsbox.json` (strict, fuzz test) with `up [-d]`/`down`, recreation on change (hash over specification and sources) | `internal/project`, `cmd/tsbox/projectcmd.go` | ✅ |
| `doctor` (Deno, state, runsc, cgroup v2, rootfs with profile hash, group, systemd, daemon, socket permissions, isolation) | `cmd/tsbox/doctor.go` | ✅ |
| `setup` (Linux: rootfs via the embedded `build-rootfs.sh`, group `tsbox`, systemd unit; `--dry-run`) | `cmd/tsbox/doctor.go`, `scripts/embed.go` | ✅ (only `--dry-run` tested; see open point 3) |

## Deviations from the plan

- **Named pipe instead of AF_UNIX on Windows, new dependency `github.com/Microsoft/go-winio` v0.6.2** (plus
  `golang.org/x/sys` indirectly). AF_UNIX without a dependency had been approved. On the test machine
  (Windows 11 ARM64, build 26100), `listen` works there, but `connect` ends with `WSAEINVAL`, and the socket
  file stays behind as a reparse point that can no longer be opened or deleted. This applies to amd64 and
  arm64 binaries and also outside the sandbox. Named pipes work everywhere; go-winio is the standard
  implementation (Docker, containerd) and is compiled only on Windows.
  - The pipe `\\.\pipe\tsboxd-<hash(SID, state)>` has the DACL `D:P(A;;GA;;;<SID>)`. Other users can neither
    open it nor create further instances.
  - If the name is already taken, tsboxd does not start (first instance).
  - The client checks with `GetNamedPipeServerProcessId` that the server process belongs to the current
    user. So `env:` secrets never end up at a foreign pipe.
- **Long socket paths (Unix):** AF_UNIX allows at most 108 bytes. From 100 characters the socket lives at
  `$XDG_RUNTIME_DIR/tsbox/<hash>.sock` or `/tmp/tsbox-<uid>/<hash>.sock`; the daemon checks who owns the
  directory.
- **Root without systemd:** an autostarted daemon of root uses the user socket `/var/lib/tsbox/run/tsboxd.sock`
  instead of `/run/tsbox/tsboxd.sock`. A lock per state directory prevents two daemons on the same state.
- **`run --driver`/`--rootfs`:** the daemon now chooses driver and rootfs (`TSBOX_DRIVER`, `TSBOX_ROOTFS` or
  `tsbox daemon --driver/--rootfs`). `run --driver gvisor` reports an error if the daemon uses a different
  driver.
- **Profiles:** phase 3 only brings the rootfs's profile hash to `doctor`. Several named profiles come in
  phase 4.
- **`check`, `types`, `sdk`** keep running locally without a daemon.

## Security

- **I12 (new):** only authorized users control tsboxd (permissions see above, ARCHITECTURE §3 and §7). Access
  corresponds to the `docker` group.
- **I2 in the daemon:**
  - Only references are stored (`file:`, `env:`).
  - The CLI sends only the values of the named `env:` variables, not its whole environment.
  - tsboxd never falls back to its own environment and keeps the values in memory only.
  - After a restart of the daemon such a box only starts again with `tsbox start`, which passes the values
    again.
  - `TestSecretsNotPersisted` checks with a canary value: state, log files, `inspect`, events and all files
    under the state stay free of it while the provider has the value.
- **Foreground `run`:** if the attach connection breaks (CLI ended, Ctrl+C), the daemon stops the box and
  removes it.

## Tests (2026-11-02)

| Environment | Unit | E2E engine | E2E CLI/daemon |
|---|---|---|---|
| Windows 11 ARM64, dev | ✅ | ✅ (gVisor tests and TestThreads skipped) | ✅ 6 green, TestInterrupt skipped |
| WSL2 Ubuntu 24.04, dev | ✅ | ✅ | ✅ 7/7 |
| WSL2, gVisor | – | ✅ (332 s) | ✅ 7/7 (213 s) |

New:
- **Unit:** `internal/logfile` (rotation, merging by Seq, Tail/Since, broken lines), `internal/project`
  (format, error messages, hash, `FuzzParse`), `client` (addresses, `env:` references).
- **E2E engine:** `TestLogsFollowAndRemove` (Follow before the start, Tail from the file, Remove with and
  without force).
- **E2E CLI/daemon** (`cmd/tsbox/e2e_test.go`, real binary, own daemon per test):
  - `TestRunExitCodes`: 0, 3, 137, 125 for a type error; foreground boxes are removed.
  - `TestLifecycle`: `run -d`, `ps`, `stats`, `logs -n/--since/-t/-f`, `stop`/`start`/`restart`, `rm` with
    and without `-f`, `inspect`, `events`.
  - `TestSecretsNotPersisted`: canary test, see above.
  - `TestRestore`: restore after a restart of the daemon, only with a restart policy.
  - `TestAutostartIdleAndPerms`: `TSBOX_NO_AUTOSTART`, autostart, permissions of socket or pipe, idle exit,
    `doctor`.
  - `TestUpDown`: create, unchanged, changed, `down`.
  - `TestInterrupt`: Ctrl+C ⇒ 130, box removed; Linux/macOS only.

## Open points

1. **Path SDKs in a directory with its own `deno.json` (bug from phase 2, fixed):** if an SDK activated with
   `--sdk <path>` was located under a `deno.json` importing `@tsbox/core` (e.g. in the repo or after `tsbox
   types`), `deno bundle` bundled `@tsbox/core` a second time, and the first SDK call hung. Cause: with
   `--config`, Deno uses the nearest `deno.json` for modules in other directories. The build (host and build
   sandbox) and `tsbox sdk test` now run with `--no-config --import-map`; only tsbox's import map applies.
   This closes a gap along the way: scripts under such a `deno.json` could import SDKs that were not activated
   (e.g. `@tsbox/http`). Tests: `TestSDKPathInRepo` (E2E, `--sdk ./sdks/echo` in the repo) and
   `TestBundleIgnoresNestedConfig`.
2. AF_UNIX leftovers on Windows: the first attempts left socket files under `%LOCALAPPDATA%\tsbox\` and
   `...\run\` that Windows can no longer open or delete (`*.sock`). They do not disturb tsbox, but probably only
   disappear with `chkdsk` or after a reboot.
3. `tsbox setup` was only tested with `--dry-run`; I did not build the rootfs and run `systemctl enable` in WSL
   because that changes the system. → **Caught up on 2026-11-05:** actually run in WSL, see phase5.md,
   addendum 4.
4. Installed SDKs live in the daemon's state. For the system daemon, `sdk add` must run as root.
5. Full `go test ./...` run on Windows: one run once hung in the root package (no CPU, no child processes; green
   alone and in the next full run), another once failed with `rename … Access is denied` in `TestEcho` (green
   three times alone). Both fit open point 2 from phase 2 (load or virus scanner); narrow down in phase 4 with
   `-count` and a stack dump. → **Solved in phase 4:** orphaned esbuild processes kept inherited pipes of
   `deno bundle` open; the `rename` failures came from the virus scanner (see phase4.md, finding 3).
6. `stats` shows RSS and threads of a script only after the first `pstats` report from init (≈ 1–2 s). →
   **Fixed on 2026-11-05** (phase5.md, addendum 2).

## Demo

Windows (in the repo directory):

```bash
go build -o tsbox.exe ./cmd/tsbox
./tsbox.exe run examples/hello.ts                                # starts tsboxd automatically; exit 0
./tsbox.exe run testdata/scripts/exit3.ts; echo $?               # 3
./tsbox.exe run -d --name tick testdata/scripts/ticker.ts --params '{"n":600}'
# Windows PowerShell 5.1 strips inner quotes: there use --params '{\"n\":600}' or --params-file
./tsbox.exe ps
./tsbox.exe stats                                                # live table, Ctrl+C ends it
./tsbox.exe logs -f -n 5 tick
./tsbox.exe stop tick; ./tsbox.exe start tick; ./tsbox.exe restart tick
./tsbox.exe inspect tick
./tsbox.exe events --since 10m
./tsbox.exe rm -f tick
./tsbox.exe doctor
```

Project (`tsbox.json` in its own directory, format see ARCHITECTURE §3):

```bash
./tsbox.exe up -d -f path/to/tsbox.json && ./tsbox.exe ps
./tsbox.exe up -f path/to/tsbox.json                             # follows the logs, Ctrl+C stops
./tsbox.exe down -f path/to/tsbox.json
```

WSL with gVisor (as root):

```bash
bash <repo>/scripts/wsl-test.sh
tsbox doctor                                                     # isolation available: yes
tsbox run -d --name bot --restart always /root/tsbox/testdata/scripts/service.ts
tsbox stats --no-stream
tsbox setup --dry-run
```
