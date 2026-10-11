#!/usr/bin/env bash
# Build the boot initrd of a generic container boot: the initrd that
# com.urunc.unikernel.bootInitrd points at, paired with a Linux kernel through
# com.urunc.unikernel.bootKernel. It contains only what has to run before the
# container's own rootfs takes over:
#   /init          the early userspace script next to this file (container-init)
#   /busybox       a static BusyBox, for the shell and the mount/switch_root tools
#   /urunit        the guest init that becomes PID 1: URUNIT, or built from
#                  URUNIT_REPO at URUNIT_COMMIT when URUNIT is unset
#   /urunit-agent  the in-guest exec agent, cross-built from this checkout
#                  unless URUNIT_AGENT names a prebuilt static Linux binary
#
# The archive is left uncompressed (cpio newc), because urunc appends the
# per-container urunit configuration to a copy of it and the kernel walks the
# concatenated archives.
#
# The same inputs give a byte-identical archive: every entry is root-owned
# with mtime SOURCE_DATE_EPOCH, and device and inode numbers are normalised.
#
# Environment:
#   URUNIT            prebuilt static urunit for the target arch; when unset,
#                     urunit is fetched and built (needs git, make and a C
#                     compiler that links statically for the target; CC is
#                     honoured for cross builds)
#   URUNIT_REPO       git repository urunit is fetched from
#   URUNIT_COMMIT     urunit commit to build (any ref git fetch accepts)
#   URUNIT_AGENT      prebuilt urunit-agent; built with go when unset
#   BUSYBOX           local static busybox; downloaded from BUSYBOX_URL when unset
#   BUSYBOX_URL       BusyBox download (default only for x86_64)
#   BUSYBOX_SHA256    expected SHA-256 of BusyBox; required for a download,
#                     checked for a local BUSYBOX only when set
#   SOURCE_DATE_EPOCH mtime of every archive entry (default: 0)
#   TARGET_ARCH       x86_64 (default: the host arch) or aarch64
#
# Usage: ./build-container-initrd.sh [INITRD_OUT]
set -euo pipefail
umask 022

INITRD="${1:-/opt/urunc/container-initrd/container-initrd}"
here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"

case "${TARGET_ARCH:-$(uname -m)}" in
  arm64|aarch64) busybox_arch=aarch64; agent_goarch=arm64 ;;
  x86_64|amd64)  busybox_arch=x86_64;  agent_goarch=amd64 ;;
  *) echo "ERROR: unsupported TARGET_ARCH=${TARGET_ARCH:-$(uname -m)}" >&2; exit 1 ;;
esac
# busybox.net publishes 1.35.0 static binaries for x86_64 only. The pinned
# checksum applies to that download, not to a caller-supplied BUSYBOX.
if [ "$busybox_arch" = x86_64 ] && [ -z "${BUSYBOX:-}" ]; then
  BUSYBOX_URL="${BUSYBOX_URL:-https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox}"
  BUSYBOX_SHA256="${BUSYBOX_SHA256-6e123e7f3202a8c1e9b1f94d8941580a25135382b99e8d3e34fb858bba311348}"
fi
URUNIT_REPO="${URUNIT_REPO:-https://github.com/safts/urunit.git}"
URUNIT_COMMIT="${URUNIT_COMMIT:-6123182babd3294f080f75fb0d8f31f82a17a3a6}"

command -v cpio >/dev/null || { echo "ERROR: cpio is required (apt-get install -y cpio)" >&2; exit 1; }
if [ -n "${URUNIT:-}" ]; then
  [ -s "$URUNIT" ] || { echo "ERROR: URUNIT is missing or empty: $URUNIT" >&2; exit 1; }
fi
if [ -z "${BUSYBOX:-}" ] && { [ -z "${BUSYBOX_URL:-}" ] || [ -z "${BUSYBOX_SHA256:-}" ]; }; then
  echo "ERROR: no checksummed BusyBox download for $busybox_arch: set BUSYBOX, or BUSYBOX_URL and BUSYBOX_SHA256" >&2; exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
stage="$work/initrd"
mkdir "$stage"

if [ -z "${URUNIT:-}" ]; then
  git init -q "$work/urunit"
  (cd "$work/urunit" && git fetch -q --depth 1 "$URUNIT_REPO" "$URUNIT_COMMIT" && git checkout -q FETCH_HEAD \
    && echo "urunit commit: $(git rev-parse HEAD) from $URUNIT_REPO" && make -s static >&2)
  URUNIT="$work/urunit/dist/urunit_static"
fi

install -m0755 "$here/container-init" "$stage/init"
install -m0755 "$URUNIT" "$stage/urunit"

# Must be named exactly "busybox": busybox dispatches `busybox <applet>` only
# when argv[0]'s basename is "busybox".
if [ -n "${BUSYBOX:-}" ]; then
  install -m0755 "$BUSYBOX" "$stage/busybox"
else
  curl -fsSL "$BUSYBOX_URL" -o "$stage/busybox" || { echo "ERROR: could not download BusyBox from $BUSYBOX_URL" >&2; exit 1; }
  chmod 0755 "$stage/busybox"
fi
[ -s "$stage/busybox" ] || { echo "ERROR: BusyBox binary is empty" >&2; exit 1; }
if [ -n "${BUSYBOX_SHA256:-}" ]; then
  echo "$BUSYBOX_SHA256  $stage/busybox" | sha256sum -c --quiet - >/dev/null 2>&1 \
    || { echo "ERROR: BusyBox checksum mismatch: expected $BUSYBOX_SHA256, got $(sha256sum "$stage/busybox" | cut -d' ' -f1)" >&2; exit 1; }
fi

# urunit-agent runs inside the container's root before any loader or library
# of the image can be assumed, so it has to be a static Linux binary.
if [ -n "${URUNIT_AGENT:-}" ]; then
  [ -s "$URUNIT_AGENT" ] || { echo "ERROR: URUNIT_AGENT is missing or empty: $URUNIT_AGENT" >&2; exit 1; }
  install -m0755 "$URUNIT_AGENT" "$stage/urunit-agent"
else
  command -v go >/dev/null || { echo "ERROR: go is required to build urunit-agent" >&2; exit 1; }
  (cd "$repo_root" && CGO_ENABLED=0 GOOS=linux GOARCH="$agent_goarch" \
    go build -trimpath -ldflags "-s -w" -o "$stage/urunit-agent" ./cmd/urunit-agent)
fi

# The mountpoints /init uses.
mkdir -p "$stage/proc" "$stage/dev" "$stage/newroot" "$stage/run"

mkdir -p "$(dirname "$INITRD")"
find "$stage" -mindepth 1 -exec touch -h -d "@${SOURCE_DATE_EPOCH:-0}" {} +
# --reproducible zeroes device numbers and renumbers inodes (GNU cpio 2.12+).
# shortcut: directory link counts still come from the build filesystem (btrfs
# differs from ext4/overlayfs); generate the archive from a list if that matters.
( cd "$stage" && find . -mindepth 1 -print | LC_ALL=C sort | cpio -o -H newc --quiet --reproducible -R 0:0 ) > "$INITRD"

echo "Container boot initrd: $INITRD ($(stat -c%s "$INITRD") bytes, cpio newc, uncompressed)"
echo "sha256: $(sha256sum "$INITRD" | cut -d' ' -f1)"
echo "Pass it as com.urunc.unikernel.bootInitrd together with a kernel as com.urunc.unikernel.bootKernel."
