# tsbox – Firecracker test (2026-11-07)

Goal: find out whether a Firecracker driver (one microVM per box) would be worth it compared to gVisor, before
building anything. Measured on one machine with the same script, without changes to tsbox.

**Environment:**
- AMD Ryzen 7 4800H (8 cores, 16 threads), 64 GB RAM, Windows 11 Home build 26300.9818
- WSL2 2.6.3 (kernel 6.6.87), Ubuntu 24.04, `nestedVirtualization=true` in `.wslconfig`
- Firecracker v1.17.0, guest kernel 6.1.155 and Ubuntu 24.04 rootfs from the Firecracker CI (v1.15; the
  bucket has no images for v1.16/v1.17 yet), rootfs read-only, 256 MiB RAM per microVM
- gVisor `runsc` release-20261102.0 (systrap and kvm), Deno 2.9.6 everywhere

Firecracker runs **nested** here (Windows hypervisor → WSL2 VM → microVM), gVisor runs directly in the WSL2 VM.
This makes Firecracker's boot slower than on bare metal.

## Method

- **Firecracker:** a small init as PID 1 starts n Deno processes with an idle script (`ready.js`: prints a line
  and keeps a 1 s timer, like `testdata/scripts/service.ts`). Measured from starting `firecracker --no-api` to
  the init line on the serial console (boot) and to the n-th ready line. Then 10 s rest, host RSS of the
  Firecracker process (VMM and guest memory) and its CPU time over 10 s. The 0.5 CPU limit is a cgroup
  `cpu.max` of `50000 100000` on the Firecracker process (1 vCPU).
- **gVisor and dev:** `BenchmarkBox` from `bench_test.go` (`box-ms`, `ready-ms` from `Start`, `box-MiB` =
  `memory.current` of the box, `idle-mcpu`), box with default limits (0.5 CPU, 128 MiB).
- 3 runs each, mean values.

## Results

| | Firecracker | gVisor systrap | gVisor + `StartCPU 2` | gVisor kvm | dev (no isolation) |
|---|---|---|---|---|---|
| start until ready, 1 script | **1.04 s** | 1.51 s | 1.33 s | 1.56 s | 0.03 s |
| start until ready, 4 scripts | **1.30 s** | 2.55 s | 1.47 s | 2.83 s | 0.04 s |
| of which boot / sandbox | ≈ 0.8 s | ≈ 1.2 s | ≈ 1.2 s | ≈ 1.25 s | 4 ms |
| RAM of the box, 1 script | 84 MiB | **24 MiB** | 25 MiB | 28 MiB | 41 MiB |
| RAM of the box, 4 scripts | 118 MiB | **61 MiB** | 61 MiB | 62 MiB | 76 MiB |
| RAM per additional script | ≈ 11 MiB | ≈ 12 MiB | ≈ 12 MiB | ≈ 11 MiB | ≈ 12 MiB |
| idle CPU, 1 / 4 scripts (thousandths of a core) | **1–2 / 3–4** | 15 / 41 | 15 / 44 | 9 / 25 | 0 / 3 |

Without a CPU limit Firecracker needs 0.87 s (1 script) and 0.96 s (4 scripts, 1 vCPU). Deno directly in WSL
(no VM): 0.17–0.26 s until ready, about 31 MiB RSS per process.

## Interpretation

- **Start:** Firecracker is about twice as fast as gVisor with 4 scripts; with the start boost gVisor comes
  close (1.47 s vs. 1.30 s). Most of Firecracker's time is the boot, which is inflated by the nesting; on bare
  metal Firecracker states about 125 ms.
- **Idle CPU:** about 10× less with Firecracker. Inside the microVM Deno runs with native system calls, while
  gVisor's sentry (systrap stubs, timers) costs CPU even at rest.
- **RAM:** Firecracker needs about 60 MiB more per box: its own guest kernel, and every VM keeps the used parts
  of the Deno binary in its own page cache. The numbers are not measured the same way: Firecracker's value is
  the RSS of the VM process, gVisor's is the box cgroup, which does not count the sentry's shared memory
  (phase5.md); gVisor's real value is somewhat higher.
- **gVisor kvm** only lowered the idle CPU here and was not faster to start, probably also because of the
  nesting.

## Consequences

- A Firecracker driver would pay off for few, active boxes that must start fast or compute a lot, and for
  hardware isolation per box. For many small, mostly idle boxes gVisor is more economical in RAM.
- Firecracker needs `/dev/kvm`: Linux on hardware or a VM with nested virtualization. The Snapdragon laptop
  (WSL without `/dev/kvm`) cannot run it.
- Before a decision, a measurement on Linux directly on hardware is missing (boot time without nesting).

## Findings along the way

1. **WSL2 nested virtualization on AMD after the October update:** with Windows build `….9711` (KB5127648)
   WSL2 silently lost nested virtualization on AMD Zen 2 (`Hyper-V: Nested features: 0x0`, no `svm`, no
   `/dev/kvm`; known issue in microsoft/WSL). The following cumulative update `26100.9818` fixed it
   (`Nested features: 0x4a0000`, `kvm_amd: Nested Virtualization enabled`).
2. **gVisor is no longer a single file from release 20261102 on:** `runsc` needs companion binaries in
   `gvisor-bin/` next to it (e.g. `gvisor_sentry`), and the direct download `releases/release/latest/<arch>/runsc`
   returns 404. Install the Debian package (as the CI does) or copy the whole package content. A lone `runsc`
   fails with `sidecar "gvisor_sentry" not usable`.
3. **Firecracker CI images lag behind releases:** the bucket `spec.ccfc.min` had kernel and Ubuntu images only
   up to v1.15 while Firecracker v1.17 was current.

## Reproducing

The test scripts are not part of the repository (throwaway code, as with the spike). Steps: Firecracker
release tarball, guest kernel and Ubuntu squashfs from `s3://spec.ccfc.min/firecracker-ci/<version>/<arch>/`,
Deno into the rootfs, an init that starts the scripts, `firecracker --no-api --config-file` with a read-only
root drive; gVisor via `bench_test.go` as in phase5.md.
