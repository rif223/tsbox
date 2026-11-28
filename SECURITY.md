# Security

tsbox runs TypeScript that you may not trust. This page says what protects you, where the limits are and how to
report a vulnerability.

## Reporting a vulnerability

Please do **not** open a public issue. Use GitHub's private vulnerability reporting instead:
**Security → Report a vulnerability** on the repository page. Describe what you found, how to reproduce it and
which driver (gvisor, firecracker, dev), tsbox version and operating system you used. You will get an answer as
soon as possible; fixes are released together with a security advisory.

## What isolates scripts

| Driver | Boundary | Use for |
|---|---|---|
| `gvisor` (Linux) | gVisor's user-space kernel (sentry) with its seccomp filter; no network, read-only rootfs, no host sockets, own UID per script | untrusted code |
| `firecracker` (Linux with KVM) | one microVM per box (hardware virtualization); no network device, read-only rootfs, own UID per script, `hidepid=2`; optional jailer (chroot, own UID per VMM) | untrusted code, strong separation between boxes |
| `dev` (Windows, macOS, Linux) | **none** – only Deno's permissions | your own, trusted code only |

There is no default driver: every box names its own (`tsbox run --driver`, `BoxSpec.Driver`), so a box never
falls back to `dev` silently. On top of every driver: Deno runs without `--allow-*`, scripts reach the outside only through SDKs with explicit
grants (`--grant`), secrets exist only on the host (`file:`/`env:`), artifacts are checked by hash before every
start, and every box and script has hard limits (memory, CPU, processes, threads, heap, run time, watchdog).
`TSBOX_REQUIRE_ISOLATION=1` makes tsbox refuse the dev driver. The design and the invariants are described in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (§7, security model).

## Known limits

1. **A bug in gVisor or in KVM/Firecracker** can break the boundary. Keep `runsc`, `firecracker` and the host
   kernel up to date. For really hostile code run tsbox in a VM of its own without secrets or access to your
   network; in WSL2, a distribution without `/mnt/c` and without Windows interop.
2. **Membership in the group `tsbox` is equivalent to root** (like the `docker` group): whoever controls tsboxd
   can, for example, start SDKs with external providers, which run on the host as the daemon's user.
3. **External SDK providers are host code** (like Docker plugins). Install only SDKs whose code you trust.
4. **Scripts in the same box are one trust domain.** Under gVisor they can read each other's `cmdline` and
   `status`. Put code that must not see each other into separate boxes.
5. **The build reads the files a script imports.** A JSON or code import is inlined into the artifact, and the
   script can send it out through an SDK it was granted. With `gvisor` and `firecracker` the build sees only the
   script's directory (plus `@tsbox/core` and the activated SDKs), so put a foreign script into a directory of
   its own, without secrets next to it. With `dev` (trusted code only) the build may import any local file, e.g.
   shared code from `../shared`.
6. **Side channels** (timing, Spectre) between boxes on the same host are not addressed.
7. **No independent audit** has been done yet.

## Supported versions

tsbox is pre-1.0; only the latest version on the default branch gets security fixes.
