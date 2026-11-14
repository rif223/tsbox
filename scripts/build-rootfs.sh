#!/usr/bin/env bash
# Builds the rootfs for the gVisor driver (as root, Linux):
#   Debian (minbase) + Deno at /usr/local/bin/deno, without further packages.
#
#   sudo scripts/build-rootfs.sh                 # to /var/lib/tsbox/rootfs, Deno from PATH
#   OUT=/srv/tsbox/rootfs DENO=/opt/deno sudo -E scripts/build-rootfs.sh
#   PROFILE=deno-next DENO=/opt/deno-next sudo -E scripts/build-rootfs.sh   # named profile at
#                                                 /var/lib/tsbox/profiles/deno-next (TSBOX_PROFILES)
#   PACKAGES=ca-certificates,tzdata …             # additional Debian packages (debootstrap --include)
#
# tsbox itself copies its (static) binary to <state>/gvisor/init when a box starts and mounts it
# read-only; it does not have to be in the rootfs.
set -euo pipefail

PROFILE="${PROFILE:-default}"
if [ "$PROFILE" = default ]; then
  OUT="${OUT:-/var/lib/tsbox/rootfs}"
else
  echo "$PROFILE" | grep -Eq '^[a-z0-9][a-z0-9_.-]{0,31}$' || { echo "invalid profile name $PROFILE" >&2; exit 1; }
  OUT="${OUT:-${TSBOX_PROFILES:-/var/lib/tsbox/profiles}/$PROFILE}"
fi
PACKAGES="${PACKAGES:-}"
SUITE="${SUITE:-bookworm}"
MIRROR="${MIRROR:-http://deb.debian.org/debian}"
DENO="${DENO:-$(command -v deno || true)}"

[ "$(id -u)" -eq 0 ] || { echo "please run as root" >&2; exit 1; }
command -v debootstrap >/dev/null || { echo "debootstrap missing: apt-get install debootstrap" >&2; exit 1; }
[ -x "$DENO" ] || { echo "Deno not found (set DENO=<path>)" >&2; exit 1; }
head -c 4 "$DENO" | grep -q ELF || { echo "$DENO is not a binary (npm wrapper?)" >&2; exit 1; }

if [ ! -x "$OUT/bin/sh" ]; then
  echo "== debootstrap $SUITE (minbase) to $OUT"
  mkdir -p "$(dirname "$OUT")"
  debootstrap --variant=minbase ${PACKAGES:+--include="$PACKAGES"} "$SUITE" "$OUT" "$MIRROR"
fi
echo "== Deno $("$DENO" --version | head -1)"
install -m 0755 "$DENO" "$OUT/usr/local/bin/deno"
# Empty mount targets (gVisor creates missing ones; existing directories avoid surprises).
mkdir -p "$OUT/artifacts" "$OUT/.tsbox"
chmod 0755 "$OUT" "$OUT/artifacts" "$OUT/.tsbox"
if [ "$PROFILE" = default ]; then
  echo "== done: $OUT ($(du -sh "$OUT" | cut -f1)). Usage: TSBOX_ROOTFS=$OUT or the default path /var/lib/tsbox/rootfs"
else
  echo "== done: profile $PROFILE in $OUT ($(du -sh "$OUT" | cut -f1)). Usage: tsbox run --profile $PROFILE …"
fi
