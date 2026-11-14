# tsbox – Phase 6 (Firecracker driver), as of 2026-11-07

The firecracker driver runs every box in its own Firecracker microVM (KVM). It is **opt-in**: `auto` never
chooses it; it is selected with `TSBOX_DRIVER=firecracker`, `tsbox daemon --driver firecracker` or
`WithDriver("firecracker")`. gVisor stays the default isolation. Comparison and measurements before the driver
existed: [firecracker-test.md](firecracker-test.md).

## Scope

| Part | Location | Status |
|---|---|---|
| Driver: one microVM per box, host cgroup (cpu.max, memory.max, pids.max), no network device, read-only rootfs, start boost via `cpu.max` | `internal/driver/firecracker` | ✅ |
| Guest: tsbox as PID 1 (`__fcinit`), rights before exec (`__fcexec`) | `internal/fcguest` | ✅ |
| Protocol host ↔ guest over vsock (setup + files, init channel, stats) | `internal/fcproto` | ✅ incl. unit tests |
| Rootfs image: ext4 from the profile's rootfs directory plus the tsbox binary, content-addressed, built once | `image_linux.go` | ✅ |
| Build microVM (type check and bundle without network, UID 65534) | `build_linux.go` | ✅ |
| Engine, daemon, CLI: `--driver firecracker`, `TSBOX_FC_KERNEL`/`--fc-kernel`, `TSBOX_FIRECRACKER`, profiles | `engine.go`, `cmd/tsbox` | ✅ |
| `tsbox doctor` (binary, kernel, e2fsprogs, KVM, usable?) and `tsbox setup --firecracker` (binary with SHA-256 check, CI guest kernel with configuration check) | `cmd/tsbox/doctor.go`, `setup_fc.go` | ✅ |
| E2E suites with `TSBOX_TEST_DRIVER=firecracker` | `e2e_*_test.go`, `cmd/tsbox/e2e_test.go` | ✅ on AMD/WSL2 (nested) |
| Jailer (opt-in): the VMM in a chroot, as an unprivileged UID/GID of its own per VM, in its own mount namespace | `jail_linux.go`, `TSBOX_FC_JAILER` / `--fc-jailer` | ✅ |
| Unused rootfs images are removed after 7 days (running VMs keep their open file) | `image_linux.go` | ✅ |
| CI job `firecracker` (E2E, jailer, benchmarks; only on runners with KVM) | `.github/workflows/ci.yml` | 🟡 written, not yet run |

## Design

**Host.** `Start` builds (once) the rootfs image, creates the VM directory with the vsock sockets
`v.sock_1024/1025/1026`, a cgroup `/tsbox/<box>` (VM RAM + 48 MiB for the VMM, `cpu.max` = box CPU, 64 PIDs) and
starts `firecracker --no-api --config-file` in it (via `tsbox __oomexec` for `oom_score_adj`). VM: RAM = box
memory + 64 MiB for the guest kernel and PID 1, vCPUs = `max(2, ⌈CPU⌉)` (so that a start boost has an effect),
the host's `cpu.max` enforces the actual limit. The serial console (guest kernel, PID 1, init's stderr) goes
line by line into the box's init log; kernel lines are dropped.

**Guest.** The kernel starts `/.tsbox/tsbox -- __fcinit` (`init=`). PID 1:
- mounts `/proc` with `hidepid=2` (scripts do not see each other – stronger than under gVisor, where `hidepid`
  has no effect, spike d), `/sys`, cgroup v2, a read-only `/dev` with only null, zero, full, random and urandom,
  an empty read-only `/tmp` (box) or a small writable one (build);
- sets hardening sysctls: `user.max_user_namespaces=0`, `kernel.unprivileged_bpf_disabled=1`,
  `dmesg_restrict`, `kptr_restrict=2`, `perf_event_paranoid=3`, `sysrq=0`, protected symlinks/hardlinks;
- connects to the host over vsock (every connection first sends `fcproto.Hello`; the host ignores connections
  without it), receives the header and a tar stream with the files (artifacts → `/artifacts`; a build: `/work`,
  `/core`, `/src`, `/sdk/<n>`, `/denodir`), each into its own tmpfs that becomes read-only (`noexec` everywhere
  except the DENO_DIR of a build, which contains esbuild);
- creates the box cgroup with `memory.max` (the box limit) and `pids.max`;
- box mode: starts the unchanged `tsbox __init --uid-mode` in that cgroup via `__fcexec` – UID 0 with only
  SETUID/SETGID/KILL in the bounding set, `no_new_privs`, `RLIMIT_NOFILE` 1024, no core dumps; its
  stdin/stdout is the vsock channel. A second connection answers stats requests (box cgroup: memory, peak,
  PIDs, `oom_kill`);
- build mode: runs deno as UID/GID 65534 without capabilities and sends the result back;
- at the end it closes its writing direction, waits until the host has read everything and restarts the VM;
  with `reboot=k` Firecracker then ends.

**Limits and OOM** behave as under gVisor: the box limit is a cgroup (in the guest), the guest kernel ends the
largest script on OOM, `oom_kill` in the guest's `memory.events` gives the reason `oom`. Script RSS comes from
`RssAnon` (exact, real kernel), threads from `RLIMIT_NPROC` per UID. CPU usage is measured on the host (the
whole VM).

**Rootfs image.** ext4 without journal, made with `mkfs.ext4 -d <rootfs>`; `debugfs` adds `/.tsbox/tsbox`, the
mount points and `/dev/console`. Key: profile hash, hash of the tsbox binary, layout version
(`<state>/firecracker/images/<key>.ext4`); building takes a few seconds once, running VMs keep theirs.

## Jailer (opt-in)

With `TSBOX_FC_JAILER=1` (jailer from PATH), a path, or `tsbox daemon --fc-jailer 1`, Firecracker's jailer
starts every VMM:
- in a chroot `<state>/firecracker/jail/firecracker/<box>/root` that only contains hard links of the kernel
  (a copy in `<state>/firecracker/kernels/`, so that linking works) and of the rootfs image, `vm.json`, the vsock
  sockets and the device nodes the jailer creates (`/dev/kvm`);
- as an unprivileged UID/GID of its own per VM (63000 + slot, 1000 slots; `firecracker.Config.JailUID`);
- in a mount namespace of its own (pivot_root).

The host cgroup and `oom_score_adj` stay as without jailer. After the VM the chroot is removed.
`tsbox setup --firecracker` installs the jailer from the same release archive (SHA-256 checked);
`tsbox doctor` shows whether it is available and on. `TestFirecrackerJail` checks UID, chroot content, mount
namespace and the removal; the whole E2E suite also passes with the jailer.

## Differences to gVisor

| | gVisor | Firecracker |
|---|---|---|
| boundary | user-space kernel (sentry) + seccomp | hardware virtualization (KVM) + the VMM's own seccomp filters |
| siblings in a box | see `cmdline`/`status` of each other | see nothing (`hidepid=2`) |
| needs | runsc, root | `/dev/kvm`, firecracker, guest kernel, e2fsprogs, root |
| `auto` | yes | never |
| host processes per box | runsc, sentry, gofer, systrap stubs | one firecracker process |

## Measurements (AMD Ryzen 7 4800H, WSL2, Firecracker nested; `BenchmarkBox`, 3 runs, 0.5 CPU)

| | firecracker | firecracker + `StartCPU 2` | gvisor (systrap) |
|---|---|---|---|
| box until init ready | 1.06 s | 0.93 s | 1.19 s |
| 1 script until ready | 1.49 s | 1.07 s | 1.46 s |
| 4 scripts until ready | 1.84 s | 1.15 s | 2.38 s |
| RAM of the box (cgroup), 1 / 4 scripts | 45 / 78 MiB | 45 / 78 MiB | 26 / 61 MiB |
| idle CPU, 1 / 4 scripts (thousandths of a core) | 12 / 22 | 11–12 / 21 | 15 / 50 |
| stop | 124–131 ms | 128–135 ms | 79–110 ms |

The box RAM of Firecracker is the guest's box cgroup (scripts, including their page cache in the guest); on the
host the VM additionally needs its guest kernel and the VMM (firecracker-test.md: ≈ 60 MiB more per box). The
boot is inflated by the nesting (Windows hypervisor → WSL2 → microVM); on bare metal it should be clearly
shorter.

## Tests (2026-11-07)

| Environment | Unit | E2E engine | E2E CLI/daemon |
|---|---|---|---|
| AMD Ryzen 7 4800H, WSL2 (nested), firecracker | ✅ | ✅ 25 tests incl. attack suite, OOM, threads, canary, profiles, load, start boost | ✅ |
| same, firecracker with jailer | – | ✅ 25 tests + `TestFirecrackerJail` | ✅ |
| Windows 11 ARM64, dev | ✅ | ✅ | ✅ |
| WSL2 Ubuntu 24.04 (Snapdragon), dev and gVisor | ✅ | ✅ | ✅ |

New: `fcproto.TestCleanTarget`, `TestAddTree`, `TestMessages`; `firecracker.TestConsoleWriter`, `TestVCPUs`,
`TestPruneImages`; E2E `TestFirecrackerJail`;
the isolation tests (`TestSiblingsAndNetwork`, `TestBuildSandbox`, `TestProfiles`) run with both isolating
drivers.

Findings along the way:
1. The build VM lost its result: the guest restarted right after writing, and Firecracker ended before the
   data reached the host. Now the guest closes its writing direction and waits until the host closes the
   connection (also for the init channel, so that the last frames arrive).
2. esbuild could not start in the build VM (`noexec` on the DENO_DIR); only that tmpfs is executable now.
3. `TestIdentityAndStop` was racy: `service.ts` logs after `ready()`, and logs arrive asynchronously. The test
   now waits briefly for both lines.
4. Once each, the first VM of a fresh state failed while receiving its files ("broken pipe"). Likely cause:
   the guest's vsock connect timed out (default 2 s) while the host was busy, and the host used the abandoned
   connection. Now the guest waits up to 10 s and greets on every connection; the host ignores connections
   without the greeting.

## Open points

1. **Bare metal:** all measurements and tests ran nested under Windows; a run on Linux on hardware is missing
   (boot time, stability).
2. **CI:** the job `firecracker` is written but has not run yet (like the rest of the CI); without KVM on the
   runner it only reports that.
3. **aarch64:** the driver and `setup --firecracker` support it, but it is untested (no ARM machine with KVM).
4. **Jailer:** without `--new-pid-ns` (the jailer would fork and tsbox could no longer wait for the VMM) and
   without a network namespace (the VM has no network device anyway).
