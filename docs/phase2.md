# tsbox – Phase 2 (SDK system), as of 2026-11-01

## Scope

| Part | Location | Status |
|---|---|---|
| Manifest `tsbox-sdk.json` (strict), grants, operator configuration, secrets only `file:`/`env:` | `manifest/` | ✅ incl. fuzz test |
| Provider API for Go (`Provider`, `Session`, `Handler`, `Router`/`Method`, error codes) | `sdk/` | ✅ |
| Registry: bundled, installed (`lock.json` with tree hash, tampering is detected), paths, Git | `internal/sdkreg` | ✅ |
| Engine: check before every call (SDK active, method, right, parameter size, rate, concurrency per method and 16 per script), time limit, result size, events only when subscribed, never blocking | `sdkcall.go` | ✅ |
| Built-in: `log`, `kv` (persistent, quota), `http` (allowlist, SSRF protection) | `internal/providers/{log,kv,httpp}`, `sdks/` | ✅ |
| External providers (any language, frames over stdio), template SDK `echo` (Deno, with events) | `internal/providers/extproc`, `sdks/echo` | ✅ |
| CLI: `run --sdk/--grant/--sdk-config/--storage`, `check`, `types`, `sdk ls/info/add/rm/init/test` | `cmd/tsbox` | ✅ |
| Templates for new SDKs (Deno or Go provider, `example.ts` as a test) | `cmd/tsbox/sdkinit.go` | ✅ |
| Build sandbox under gVisor: no network, UID 65534 without capabilities, sources ro, esbuild from a ro cache, output via stdout | `internal/driver/gvisor/build.go`, `internal/build/sandbox.go` | ✅ (slow, see below) |

## Security (new in phase 2)

- **I7 – rights:** no call without a grant (`permission_denied` with an example for `--grant`). The core checks rights and limits; the provider checks the content.
- **No oracle:** an unknown SDK and an unknown method give the same answer (`not_found`).
- **Secrets** live only on the host. They are only allowed as `file:`/`env:` and are shown as `***` in `BoxStatus.SDKs`. They never appear in the script or in the `start` frame.
- **`http`:** https only (http via a setting), host allowlist with wildcard and port. The IP is checked *while connecting* (no private, loopback, link-local, CGNAT or metadata addresses), so DNS rebinding does not help. Redirects are checked again. No proxy, no cookies, size and time limit.
- **`kv`:** namespace per storage area and script, paths validated (no traversal), quota per script.
- **External providers** are trusted operator code on the host (like Docker plugins). They get a minimal environment and never reach boxes directly: their frame types are missing from `FromScript`/`FromInit`.
- **Build sandbox (P1 S08):** `deno bundle --check --no-remote --no-npm` in gVisor without network. It sees only the script's directory; imports outside fail (`TestBuildSandbox`).

## Tests (2026-11-01)

| Environment | Unit | E2E |
|---|---|---|
| Windows 11 ARM64, dev | ✅ | ✅ (core + SDK; gVisor tests skipped) |
| WSL2, dev | ✅ | ✅ |
| WSL2, gVisor | ✅ | ✅ 16/16 incl. build sandbox |

New E2E tests:
- `TestSDKKVAndLog`: persistence over two runs, value and quota limit
- `TestSDKHTTP`: grant ok, foreign host and `file:` rejected
- `TestSDKGrants`: five configuration errors with clear messages
- `TestSDKLimits`: rate/concurrency, oracle, parameter size
- `TestSDKExternalEcho`: events, grant `ticks` enforced
- `TestBuildSandbox`

Unit tests for manifest, registry, kv, http (SSRF cases) and the external provider (incl. crash and restart).

## Open points

1. **The build sandbox is slow:** about 17 s per script under gVisor (systrap, WSL2) compared to 1–3 s on the host. The read-only cache explains only a small part of it (1.8 s instead of 0.9 s on the host); the rest is gVisor's overhead during type checking. Proposal for phase 5: build cache by input hash (source directory, SDK hashes, core version, Deno version) so that unchanged scripts are not rebuilt at all; additionally measure `--platform=kvm` on target hardware. → Build cache implemented in phase 5 (phase5.md); `--platform=kvm` selectable, measured only in the CI.
2. **One sporadic failure** in the full `go test ./...` run on Windows; single runs were green three times. Probably load from packages building in parallel (watchdog test). Narrow down in phase 4 with `-count=5`. → Cause found and fixed in phase 4 (phase4.md, finding 3).
3. The SDK module must be in the root directory of its SDK so that the build sandbox can mount it together with neighboring files (`module` in subdirectories with `../` imports does not work there).
4. `kv` stores a JSON file instead of bbolt (no dependency, sufficient for ≤ 1 MiB per script). Another backend later for larger data.

## Demo

Windows (in the repo directory):

```bash
go build -o tsbox.exe ./cmd/tsbox
./tsbox.exe run examples/counter.ts --sdk kv --sdk log        # twice: the counter goes up
./tsbox.exe run examples/fetch.ts --sdk http                  # ⇒ message with a --grant example
./tsbox.exe run examples/fetch.ts --sdk http --grant http:host=api.github.com
./tsbox.exe run examples/echo.ts --sdk echo --grant echo:ticks=3
./tsbox.exe sdk ls
./tsbox.exe sdk info http
./tsbox.exe sdk init greeter --provider deno
./tsbox.exe sdk test greeter --grant greeter:greeting=Hi
./tsbox.exe types --sdk kv --sdk http                         # deno.json for VS Code
```

WSL with gVisor:

```bash
bash <repo>/scripts/wsl-test.sh
cd /root/tsbox && tsbox run examples/counter.ts --sdk kv --sdk log
```
