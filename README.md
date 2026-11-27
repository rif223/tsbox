# tsbox

[![ci](https://github.com/rif223/tsbox/actions/workflows/ci.yml/badge.svg)](https://github.com/rif223/tsbox/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Run TypeScript (Deno) in isolation – like Docker, but for scripts.

- One gVisor sandbox (or, opt-in, one Firecracker microVM) with hard limits per **box**.
- Inside it one Deno process per **script**, with its own UID and its own limits.
- The only channel is stdin/stdout (no network, no writable host path).

> Docs: [architecture](docs/ARCHITECTURE.md), [notes](docs/NOTES.md) (measurements, design decisions, limits),
> [gVisor spike](docs/gvisor-spike.md), [Firecracker compared with gVisor](docs/firecracker-test.md).

## Install

Needs [Deno](https://deno.com) 2.9 (`deno` in `PATH` or `TSBOX_DENO`).

**Prebuilt binaries** for Linux, Windows and macOS (amd64, arm64) are on the
[releases page](https://github.com/rif223/tsbox/releases), with `SHA256SUMS`:

```bash
# Linux amd64 (other systems: pick the matching archive; Windows: .zip)
V=0.1.0
curl -fLO "https://github.com/rif223/tsbox/releases/download/v$V/tsbox_${V}_linux_amd64.tar.gz"
curl -fLO "https://github.com/rif223/tsbox/releases/download/v$V/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf "tsbox_${V}_linux_amd64.tar.gz"
sudo install -m 755 "tsbox_${V}_linux_amd64/tsbox" /usr/local/bin/
```

**From source** (Go, version from `go.mod`):

```bash
CGO_ENABLED=0 go install github.com/rif223/tsbox/cmd/tsbox@latest
```

**Then:**

```bash
sudo tsbox setup                       # Linux: rootfs, group tsbox, systemd unit (system daemon)
sudo tsbox setup --firecracker         # Linux with KVM: additionally Firecracker, jailer, guest kernel
tsbox setup                            # Windows, macOS: tsboxd starts at logon and keeps running
tsbox doctor                           # what is available, which driver isolates
```

Like Docker's engine, tsboxd then runs permanently. `tsbox setup --remove` undoes that (boxes stay).

**Windows:** started from a terminal inside a packaged (MSIX) app, tsbox's state in `%LOCALAPPDATA%\tsbox` is
redirected into the app's package folder: builds get slower, and tsbox outside the app sees a different state.
`tsbox doctor` warns about it; set `TSBOX_STATE` to a directory outside `%LOCALAPPDATA%` and `%APPDATA%`.

## Quick start

```bash
git clone https://github.com/rif223/tsbox && cd tsbox
go build -o tsbox ./cmd/tsbox          # on Linux for gVisor/Firecracker: CGO_ENABLED=0
./tsbox run examples/hello.ts --driver dev        # Linux: --driver gvisor (tsbox doctor lists the usable ones)
./tsbox run examples/fetch.ts --driver dev --sdk http --grant http:host=api.github.com
```

- **Linux with gVisor** (root, `runsc` in PATH, rootfs with `/usr/local/bin/deno` at `TSBOX_ROOTFS` or
  `/var/lib/tsbox/rootfs`): isolated. Further rootfs variants (e.g. a different Deno version) are named profiles
  at `/var/lib/tsbox/profiles/<name>` (`TSBOX_PROFILES`; build with `PROFILE=<name> scripts/build-rootfs.sh`),
  selected with `run --profile <name>` or `"profile"` in `tsbox.json`; `tsbox doctor` lists them with their
  fingerprint.
- **Linux with KVM, opt-in:** driver `firecracker` (per box with `run --driver firecracker` or `"driver"` in
  `tsbox.json`): every box runs in its own microVM. Needs root, `/dev/kvm`, the `firecracker` binary, a guest
  kernel (`TSBOX_FC_KERNEL`, default `/var/lib/tsbox/firecracker/vmlinux`), `e2fsprogs` and the same rootfs as
  gVisor. `sudo tsbox setup --firecracker` installs binary, jailer and kernel. With `TSBOX_FC_JAILER=1` every
  VMM additionally runs under Firecracker's jailer (chroot, own UID per VM). `auto` never chooses it.
- **Otherwise** (Windows, macOS, Linux without gVisor): driver `dev` with the warning "NO isolation".
- With `TSBOX_REQUIRE_ISOLATION=1` tsbox refuses the dev driver.

## Managing boxes (tsboxd)

All box commands talk to the daemon `tsboxd`, which `tsbox setup` sets up to run permanently: on Linux a systemd
unit (system daemon, access via the group `tsbox` like the `docker` group), on Windows an entry that starts it at
logon (no admin rights; it runs as you, since Windows has only the dev driver), on macOS a LaunchAgent. If it is
set up but not running, commands on Linux and macOS say how to start it instead of starting a second daemon; on
Windows they start it the way the logon entry does. Without `tsbox setup`
the CLI starts tsboxd itself on demand and it exits after 5 min of idle time (`TSBOX_NO_AUTOSTART=1` turns this
off).

```bash
tsbox run -d --name bot --restart always --driver gvisor bot.ts   # in the background
tsbox ps -a                                          # boxes
tsbox logs -f --since 10m bot                        # log files per script (2 × 10 MiB)
tsbox stats                                          # live table: RAM/LIMIT, CPU, PIDS, RESTARTS, UPTIME
tsbox stop bot · tsbox start bot · tsbox restart bot · tsbox rm -f bot
tsbox inspect bot                                    # JSON without secret values
tsbox update bot                                     # rebuild from the changed files, then swap (old one runs until the build passed)
tsbox update --mem 256Mi --entry bot2.ts bot         # … or with new limits, entry, params, or --spec box.json
tsbox emit bot reload '{"force":true}'               # message to the scripts (@tsbox/inbox)
tsbox events --since 1h
tsbox up -d · tsbox down                             # project from tsbox.json (several boxes)
tsbox doctor                                         # runsc, cgroup v2, rootfs, Deno, daemon, isolation?
tsbox daemon start · tsbox daemon stop               # start tsboxd in the background / end it
```

One daemon runs boxes with every driver that is usable on the machine. Each box names its driver (`run --driver`,
`"driver"` in `tsbox.json`); there is no default driver, so a box without one is refused. A box keeps the driver
it was created with; a box stored by tsbox 0.1 without a driver does not start until it is created again. `tsbox doctor` shows which drivers the daemon can use and
why one is missing (no root, no KVM, WSL2 on Windows on ARM, unsupported CPU architecture). gVisor counts as
usable only after a test sandbox started; it switches to its kvm platform by itself where systrap does not work.

`tsbox.json` describes boxes with scripts, SDKs, grants and limits (format:
[ARCHITECTURE.md §3](docs/ARCHITECTURE.md#3-cli)).

## `tsbox run`

```
tsbox run <script.ts> [--mem 128Mi] [--cpu 0.5] [--pids 64] [--start-cpu 0] box
                      [--heap 64Mi] [--rss 0] [--threads 32]              script
                      [--timeout 0] [--watchdog 10s] [--restart no|on-failure[:n]|always]
                      --driver gvisor|firecracker|dev [--profile default] [--params '{"k":1}' | --params-file p.json] [-q] [-d [--rm]] [--name box]
```

| Exit code | Meaning |
|---|---|
| the script's code | regular end (`Deno.exit(n)`; unhandled error ⇒ 1) |
| 137 | limit exceeded: `rss`, `threads`, `heap`, `oom`, `watchdog`, `timeout` |
| 126 | `integrity` (artifact changed) or `protocol` (invalid bytes on stdout) |
| 125 | error of tsbox or tsboxd (build, start) |
| 2 | wrong input: unknown flag, invalid value, missing script (also for all other commands) |
| 130 | Ctrl+C |

Errors in the script show the file and line of the source (the bundle carries a source map). Files for
`--params-file` and `tsbox.json` may be UTF-8 with or without BOM or UTF-16 (PowerShell `Out-File`).

A script ends by itself as soon as its event loop is empty. Whoever wants to run permanently uses `keepAlive()`
or subscribes to events.

## In the script: `@tsbox/core`

```ts
import { call, keepAlive, on, ready, script, SdkError } from "@tsbox/core";
console.log(script.name, script.params);  // console.* and Deno.stdout end up in the log
```

Bundled SDKs (`--sdk <name>`): `log`, `kv`, `http`, `inbox`, `cron` and the template `echo`.

```ts
import { kv } from "@tsbox/kv";        // per-script store: versions, counters, TTL, paging
await kv.incr("visits");
await kv.set("session:42", { user: 1 }, { ttl: 60_000 });
const e = await kv.getEntry<number>("n");
await kv.set("n", (e?.value ?? 0) + 1, { ifVersion: e?.version ?? 0 });   // false if someone else wrote

import { inbox } from "@tsbox/inbox";  // tsbox emit <box> <topic> [json] → here; send → event script_message
inbox.on((m) => inbox.send("ack", m.topic));

import { cron } from "@tsbox/cron";    // --grant 'cron:schedule=nightly=0 3 * * *' --grant 'cron:schedule=@every 5m'
cron.on("nightly", () => cleanup());
```

## Operating tsboxd for several users

- **Metrics:** `GET /v1/metrics` (Prometheus) on the socket; `tsbox daemon --metrics-addr 127.0.0.1:9464`
  additionally serves only `/metrics` over TCP (box names, states, RAM/CPU, restarts, SDK calls by result).
- **Tenant quotas:** `tsbox daemon --tenant-label tenant --tenant-quota boxes=10,memory=2Gi,cpu=4` groups boxes by
  the label `tenant` and limits each group (boxes counted always, resources of running boxes).
  `GET /v1/tenants` shows usage. The socket stays the only access to the API: put your own authenticated API in
  front of it for untrusted users.
- **Own kv storage:** embedding applications pass `tsbox.WithKVStore(myStore)` (interface `kvstore.Store`), e.g.
  to keep kv data in PostgreSQL.

## As a Go library

```go
func main() {
	tsbox.Init() // must come first: the own binary serves as tsbox-init in the box
	e, _ := tsbox.New(ctx, tsbox.WithLogs(func(l tsbox.LogLine) { fmt.Println(l.Script, l.Msg) }),
		tsbox.WithEvents(func(ev tsbox.Event) { /* ready, exited, oom, circuit_open … */ }))
	b, _ := e.Run(ctx, tsbox.BoxSpec{Name: "bot", Scripts: []tsbox.ScriptSpec{
		{Name: "main", Entry: "bot.ts", Limits: tsbox.ScriptLimits{RSS: 96 << 20}},
	}, Restart: tsbox.RestartPolicy{Mode: tsbox.RestartOnFailure}})
	st, _ := b.Wait(ctx)
	for l, err := range b.Logs(ctx, tsbox.LogOptions{Tail: 50}) { /* from the log files */ }
}
```

For a running daemon there is the package `client` (`client.New(client.Hosts()[0])`, then `List`, `Create`,
`Attach`, `Update`, `Emit`, `Logs`, `Stats`, `Events`, …). The engine itself has `Engine.Update`, `Box.Emit`,
`Engine.SDKCalls`, `Engine.Tenants` and the options `WithTenantQuotas` and `WithKVStore`.

## What is enforced

| Limit | gvisor | firecracker | dev (Windows) | dev (Linux/macOS) |
|---|---|---|---|---|
| Isolation | gVisor, no network, ro rootfs, UID per script | microVM (KVM), no network device, ro rootfs, UID per script, `hidepid=2` | **none** (only Deno permissions) | **none** |
| Box RAM | cgroup, hard; the kernel ends the largest script | cgroup in the guest, hard; the guest kernel ends the largest script | job object + watchdog (whole box) | watchdog (Linux), macOS: no |
| Box CPU / PIDs | cgroup | host cgroup of the VM / cgroup in the guest | job object (processes instead of threads) | no |
| Script RSS | `VmRSS − baseline` (measured) | RssAnon (exact, real guest kernel) | private memory | RssAnon (Linux), macOS: no |
| Script threads | `RLIMIT_NPROC` per UID (hard) | `RLIMIT_NPROC` per UID (hard) | no | measured (Linux) |
| Heap, run time, watchdog, integrity, protocol | yes | yes | yes | yes |

## Tests

```bash
go test ./...                                  # unit + E2E with the dev driver (needs Deno)
bash scripts/wsl-test.sh                       # in WSL/Linux as root: additionally with gVisor (incl. daemon tests)
TSBOX_DRIVERS=firecracker bash scripts/wsl-test.sh   # as root, with KVM: E2E with Firecracker
bash scripts/fuzz.sh                           # all fuzz targets
```

## Security, contributing, license

- How isolation works, its limits and how to report a vulnerability privately: [SECURITY.md](SECURITY.md)
- Development and pull requests: [CONTRIBUTING.md](CONTRIBUTING.md)
- License: [Apache License 2.0](LICENSE) – Copyright 2026 rif223
