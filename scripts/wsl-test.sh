#!/bin/bash
# Tests tsbox on Linux/WSL2 as root: copies the repo to /root/tsbox, builds statically and runs unit and
# E2E tests with the dev and the gVisor driver. The gVisor job of the CI uses this script as well.
#   bash scripts/wsl-test.sh [-run pattern]
#   TSBOX_DRIVERS=gvisor bash scripts/wsl-test.sh      # only E2E with gVisor (CI)
#   TSBOX_DRIVERS=firecracker bash scripts/wsl-test.sh # only E2E with Firecracker (needs /dev/kvm, CI)
# Rootfs: TSBOX_ROOTFS or /var/lib/tsbox/rootfs (scripts/build-rootfs.sh).
set -euo pipefail
SRC="$(cd "$(dirname "$0")/.." && pwd)"
DRIVERS="${TSBOX_DRIVERS:-dev gvisor}"
export TSBOX_ROOTFS="${TSBOX_ROOTFS:-/var/lib/tsbox/rootfs}"
case " $DRIVERS " in *" gvisor "*|*" firecracker "*)
  [ -x "$TSBOX_ROOTFS/usr/local/bin/deno" ] || { echo "no rootfs at $TSBOX_ROOTFS – first: sudo $SRC/scripts/build-rootfs.sh" >&2; exit 1; } ;;
esac
rm -rf /root/tsbox && mkdir -p /root/tsbox
tar -C "$SRC" --exclude=./.git -cf - . | tar -C /root/tsbox -xf -
cd /root/tsbox
export CGO_ENABLED=0
go vet ./...
go build -o /usr/local/bin/tsbox ./cmd/tsbox
if [ "$DRIVERS" = "dev gvisor" ]; then
  echo "== unit tests"
  go test -count=1 ./internal/... ./manifest/... ./client/... ./sdk/...
fi
for d in $DRIVERS; do
  echo "== E2E $d"
  TSBOX_TEST_DRIVER=$d go test -count=1 -timeout 40m . ./cmd/tsbox "$@"
done
