#!/usr/bin/env bash
#
# Build the nerdbox guest kernel and root filesystem — the two files a Homebrew formula
# cannot produce, and the two a microVM boots. `boks doctor` fails its `guest image` check
# without them.
#
#   scripts/build-nerdbox-guest.sh [arch] [outdir]
#
#   arch    arm64 (Apple silicon, and the verified platform) or x86_64. Defaults to this
#           machine's. The name is nerdbox's own spelling and is also the suffix in the
#           kernel's filename, which is why it is not "amd64".
#   outdir  where the two files land. Defaults to ./dist/nerdbox-guest-<arch>.
#
# Requires Docker with buildx, and a Linux kernel build's worth of time and disk. It does
# not have to run on the machine that will use the output: these files are guest artefacts,
# so building them once on any Linux box, or in CI, and copying them to the Mac is fine.
#
# ## Why this is a script and not a formula
#
# nerdbox builds its guest with `docker buildx bake`. Homebrew has no Docker and no Linux
# cross-toolchain, so no formula can do this, and there is nothing published to download
# instead: nerdbox's own release workflow has failed on every tag since v0.2.0 and all ten
# of its GitHub releases carry zero assets. Boks pins nerdbox by tag and checksum here, and
# `packaging/homebrew/tap/Formula/nerdbox.rb.in` pins the same tag for the shim.
#
# ## What is pinned, and what is only pinned by nerdbox
#
# This script pins the nerdbox source tarball by SHA-256 and refuses to continue if it does
# not match. Everything the build then fetches — the kernel.org tarball, a crun release
# binary, Debian packages — is pinned by *nerdbox's* Dockerfile, not by Boks. That is worth
# reading before trusting the output: the recipe is upstream's, and this script's guarantee
# stops at "the recipe is the one from v0.2.3".
set -euo pipefail

# v0.2.3 contains cd2c23f, which is the nerdbox commit docs/verification.md records the VM
# boundary being verified against. Bumping this means the guest under Boks is no longer the
# guest that evidence was collected with.
NERDBOX_VERSION="${NERDBOX_VERSION:-0.2.3}"
NERDBOX_SHA256="${NERDBOX_SHA256:-8eb4c638d161701f93b01ec2c84fbc4891a0be98a10d1887473095c6c309cbc1}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

case "${1:-}" in
"") case "$(uname -m)" in
	aarch64 | arm64) arch=arm64 ;;
	x86_64 | amd64) arch=x86_64 ;;
	*)
		echo "cannot guess a nerdbox KERNEL_ARCH for $(uname -m); pass one" >&2
		exit 1
		;;
	esac ;;
arm64 | x86_64) arch="$1" ;;
aarch64)
	echo "use 'arm64', not 'aarch64': the shim looks for nerdbox-kernel-arm64" >&2
	exit 1
	;;
*)
	echo "unsupported arch: $1 (expected arm64 or x86_64)" >&2
	exit 1
	;;
esac

outdir="${2:-$root/dist/nerdbox-guest-$arch}"

command -v docker >/dev/null || {
	echo "docker is required: nerdbox builds its guest with docker buildx bake" >&2
	exit 1
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

url="https://github.com/containerd/nerdbox/archive/refs/tags/v${NERDBOX_VERSION}.tar.gz"
echo "==> fetching $url"
curl -fsSL "$url" -o "$work/nerdbox.tar.gz"

echo "==> verifying the source tarball"
echo "${NERDBOX_SHA256}  $work/nerdbox.tar.gz" | sha256sum -c -

tar -xzf "$work/nerdbox.tar.gz" -C "$work"
src="$work/nerdbox-${NERDBOX_VERSION}"

# KERNEL_ARCH drives the kernel's cross-compiler and its filename, and NOTHING ELSE. The
# rootfs needs the platform set explicitly, and this was wrong here until 2026-08-17.
#
# nerdbox's bake file does define `_DOCKER_ARCH` mapping KERNEL_ARCH to a Docker platform,
# which is where the old comment's claim came from — but only `_host_common` (the shim) and
# `libkrun` reference it. `target "rootfs"` inherits `_guest_common`, which sets no
# `platforms` at all, so buildx builds it for the BUILDER's architecture. The Dockerfile's
# rootfs stages then follow that: `vminit-build` is `GOARCH=${TARGETARCH} go build` and
# `crun-build` wgets `crun-…-linux-${TARGETARCH}`. Only the kernel reads KERNEL_ARCH.
#
# So `KERNEL_ARCH=arm64` on an x86_64 builder produced an arm64 kernel beside an x86_64
# vminitd, and the VM died at boot with a kernel panic naming neither architecture:
#
#   Kernel panic - not syncing: Requested init /sbin/vminitd failed (error -8).
#
# -8 is ENOEXEC. Every published guest archive up to and including v0.1.1 has this, because
# CI builds on x86_64 runners; building on an aarch64 host hid it, since native then happened
# to be right. assert-guest-image.py now reads vminitd's own e_machine, so a repeat is a red
# build rather than a panic on someone's Mac.
#
# No emulation is needed for the cross build: the Go and mkfs.erofs stages are all
# `FROM --platform=$BUILDPLATFORM`, so setting the platform changes TARGETARCH and nothing
# about where the work runs.
case "$arch" in
arm64) docker_arch=arm64 ;;
x86_64) docker_arch=amd64 ;;
esac

echo "==> building the guest kernel and rootfs for $arch (this takes a while)"
(cd "$src" && KERNEL_ARCH="$arch" docker buildx bake kernel rootfs \
	--set "rootfs.platform=linux/$docker_arch")

mkdir -p "$outdir"
cp "$src/_output/nerdbox-kernel-$arch" "$src/_output/nerdbox-rootfs.erofs" "$outdir/"

# The same assertion CI runs, on the same two files, because this script's output is copied
# straight into $(brew --prefix)/lib by hand and a mismatch here surfaces as a kernel panic
# rather than as an error naming the file.
#
# Exit 2 means the check could not be MADE — no python3, or no fsck.erofs to open the rootfs
# with — and that is a warning rather than a failure: this script is meant to run on any
# Docker host, including one that is not a Boks host and has neither. Exit 1 means it was
# made and the artifact is wrong, which is fatal.
echo
echo "==> checking the built guest is really $arch"
if command -v python3 >/dev/null; then
	set +e
	python3 "$root/packaging/linux/assert-guest-image.py" "$outdir" --arch "$arch"
	rc=$?
	set -e
	case "$rc" in
	0) ;;
	1) exit 1 ;;
	*) echo "    (could not check: see above; the files are still in $outdir)" >&2 ;;
	esac
else
	echo "    (skipped: no python3 on this machine)" >&2
fi

echo
echo "==> built into $outdir"
(cd "$outdir" && sha256sum nerdbox-kernel-"$arch" nerdbox-rootfs.erofs | tee SHA256SUMS)
cat <<EOF

Install them where the shim looks. On Apple silicon with Homebrew that is:

  cp $outdir/nerdbox-kernel-$arch $outdir/nerdbox-rootfs.erofs \\
     "\$(brew --prefix)/lib/"

Any directory on containerd's PATH, or on LIBKRUN_PATH, works equally well — the shim
scans both. Note that containerd's PATH is the daemon's, not your shell's.

'boks doctor' reports these two as 'guest image', scanning the same PATH and LIBKRUN_PATH
the shim does. Without them it fails that line rather than reporting the host ready.
EOF
