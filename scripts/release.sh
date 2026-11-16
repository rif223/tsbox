#!/bin/bash
# Builds the release archives of tsbox into dist/: static binaries (CGO_ENABLED=0, needed for gVisor and
# Firecracker) for Linux, Windows and macOS, each with LICENSE, NOTICE and README.md, plus SHA256SUMS.
#   bash scripts/release.sh 0.1.0        # version without the leading "v"
# The release workflow (.github/workflows/release.yml) runs it for a tag v<version>.
set -euo pipefail
VERSION="${1:?usage: scripts/release.sh <version, e.g. 0.1.0>}"
VERSION="${VERSION#v}"
cd "$(dirname "$0")/.."
TARGETS="${TARGETS:-linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64}"

rm -rf dist && mkdir -p dist
for t in $TARGETS; do
  os="${t%/*}" arch="${t#*/}"
  name="tsbox_${VERSION}_${os}_${arch}"
  dir="dist/$name"
  mkdir -p "$dir"
  exe=tsbox
  [ "$os" = windows ] && exe=tsbox.exe
  echo "== $name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X github.com/rif223/tsbox.Version=$VERSION" -o "$dir/$exe" ./cmd/tsbox
  cp LICENSE NOTICE README.md "$dir/"
  if [ "$os" = windows ]; then
    if command -v zip >/dev/null; then
      (cd dist && zip -qr "$name.zip" "$name")
    else
      (cd dist && python3 -m zipfile -c "$name.zip" "$name")
    fi
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -rf "$dir"
done
(cd dist && sha256sum ./*.tar.gz ./*.zip | sed 's| \./| |' > SHA256SUMS)
cat dist/SHA256SUMS
