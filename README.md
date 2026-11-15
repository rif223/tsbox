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

Needs Go (version from `go.mod`) and [Deno](https://deno.com) 2.9 (`deno` in `PATH` or `TSBOX_DENO`).

```bash
CGO_ENABLED=0 go install github.com/rif223/tsbox/cmd/tsbox@latest
sudo tsbox setup                       # Linux: rootfs, group tsbox, systemd unit (optional)
sudo tsbox setup --firecracker         # Linux with KVM: additionally Firecracker, jailer, guest kernel
tsbox doctor                           # what is available, which driver isolates
```

## Quick start

```bash
git clone https://github.com/rif223/tsbox && cd tsbox
go build -o tsbox ./cmd/tsbox          # on Linux for gVisor/Firecracker: CGO_ENABLED=0
./tsbox run examples/hello.ts
./tsbox run examples/fetch.ts --sdk http --grant http:host=api.github.com
```

- **Linux with gVisor** (root, `runsc` in PATH, rootfs with `/usr/local/bin/deno` at `TSBOX_ROOTFS` or
  `/var/lib/tsbox/rootfs`): isolated. Further rootfs variants (e.g. a different Deno version) are named profiles
  at `/var/lib/tsbox/profiles/<name>` (`TSBOX_PROFILES`; build with `PROFILE=<name> scripts/build-rootfs.sh`),
  selected with `run --profile <name>` or `"profile"` in `tsbox.json`; `tsbox doctor` lists them with their
  fingerprint.
- **Linux with KVM, opt-in:** driver `firecracker` (`TSBOX_DRIVER=firecracker` or `tsbox daemon --driver
  firecracker`): every box runs in its own microVM. Needs root, `/dev/kvm`, the `firecracker` binary, a guest
  kernel (`TSBOX_FC_KERNEL`, default `/var/lib/tsbox/firecracker/vmlinux`), `e2fsprogs` and the same rootfs as
  gVisor. `sudo tsbox setup --firecracker` installs binary, jailer and kernel. With `TSBOX_FC_JAILER=1` every
  VMM additionally runs under Firecracker's jailer (chroot, own UID per VM). `auto` never chooses it.
- **Otherwise** (Windows, macOS, Linux without gVisor): driver `dev` with the warning "NO isolation".
- With `TSBOX_REQUIRE_ISOLATION=1` tsbox refuses the dev driver.

## Managing boxes (tsboxd)

All box commands talk to the daemon `tsboxd`. If none is running, the CLI starts it itself; it exits after 5 min
of idle time (`TSBOX_NO_AUTOSTART=1` turns this off). On Linux, `sudo tsbox setup` sets up the rootfs, the group
`tsbox` and a systemd unit (system daemon, access like the `docker` group).

```bash
tsbox run -d --name bot --restart always bot.ts      # in the background
tsbox ps -a                                          # boxes
tsbox logs -f --since 10m bot                        # log files per script (2 × 10 MiB)
tsbox stats                                          # live table: RAM/LIMIT, CPU, PIDS, RESTARTS, UPTIME
tsbox stop bot · tsbox start bot · tsbox restart bot · tsbox rm -f bot
tsbox inspect bot                                    # JSON without secret values
tsbox events --since 1h
tsbox up -d · tsbox down                             # project from tsbox.json (several boxes)
tsbox doctor                                         # runsc, cgroup v2, rootfs, Deno, daemon, isolation?
tsbox daemon stop                                    # end the autostarted daemon (e.g. to switch TSBOX_DRIVER)
```

`tsbox.json` describes boxes with scripts, SDKs, grants and limits (format:
[ARCHITECTURE.md §3](docs/ARCHITECTURE.md#3-cli)).

## `tsbox run`

```
tsbox run <script.ts> [--mem 128Mi] [--cpu 0.5] [--pids 64] [--start-cpu 0] box
                      [--heap 64Mi] [--rss 0] [--threads 32]              script
                      [--timeout 0] [--watchdog 10s] [--restart no|on-failure[:n]|always]
                      [--driver auto|gvisor|firecracker|dev] [--profile default] [--params '{"k":1}' | --params-file p.json] [-q] [-d [--rm]] [--name box]
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
`Attach`, `Logs`, `Stats`, `Events`, …).

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
