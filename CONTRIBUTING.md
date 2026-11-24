# Contributing

Thanks for your interest in tsbox. Bug reports, ideas and pull requests are welcome. For security problems
please read [SECURITY.md](SECURITY.md) first and report them privately.

## Development setup

- Go (version from `go.mod`) and Deno 2.9.6 (`deno` in `PATH` or `TSBOX_DENO`)
- Linux for the isolating drivers: gVisor (`runsc`, a rootfs from `scripts/build-rootfs.sh`), optionally
  Firecracker (`/dev/kvm`, `sudo tsbox setup --firecracker`)

```bash
go build -o tsbox ./cmd/tsbox                    # CGO_ENABLED=0 on Linux for gVisor/Firecracker
go test ./...                                    # unit + E2E with the dev driver (all systems)
sudo bash scripts/wsl-test.sh                    # Linux/WSL as root: E2E with dev and gVisor
sudo TSBOX_DRIVERS=firecracker bash scripts/wsl-test.sh   # E2E with Firecracker (needs KVM; with runsc
                                                          # installed also gVisor and Firecracker side by side)
bash scripts/fuzz.sh                             # all fuzz targets
```

## Before a pull request

- `gofmt -l .` prints nothing, `go vet ./...` passes, also with `GOOS=linux`, `GOOS=windows` and `GOOS=darwin`
- `deno fmt --check` and `deno lint` pass; `shellcheck scripts/*.sh` passes
- `go test ./...` passes; changes to a driver are tested with that driver (see above)
- new behaviour has a test; security-relevant changes get an attack or E2E test
- code, comments, messages and docs are in English; CLI output stays ASCII (Windows PowerShell 5.1 garbles
  UTF-8 in pipes)
- user-visible changes are described in the docs (`README.md`, `docs/`)

## Releases

A tag `v<version>` on `main` starts `.github/workflows/release.yml`: checks, `scripts/release.sh <version>`
(static binaries for Linux, Windows, macOS on amd64 and arm64, archives and `SHA256SUMS`) and a GitHub release
with generated notes. Tags with a `-` (e.g. `v0.2.0-rc.1`) become pre-releases. Locally:
`bash scripts/release.sh 0.1.0` writes the same files into `dist/`.

## Layout

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (§9, project structure). [docs/NOTES.md](docs/NOTES.md)
records measurements, design decisions, platform pitfalls and known limits.

By contributing you agree that your contribution is licensed under the Apache License 2.0 (see
[LICENSE](LICENSE)).
