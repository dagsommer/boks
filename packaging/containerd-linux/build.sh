#!/usr/bin/env bash
#
# Build the containerd the Linux packages vendor, for one architecture.
#
#   packaging/containerd-linux/build.sh <goarch> <outdir> [srcdir]
#
# Writes <outdir>/containerd, an ELF64 executable of the architecture named by <goarch>,
# built from the release in CONTAINERD_VERSION with the tags in BUILDTAGS. With no <srcdir>
# it makes its own shallow checkout of that tag and removes it afterwards; with one, it
# builds the tree you point it at and leaves it alone.
#
# WHY THIS IS BUILT AND NOT DOWNLOADED. containerd does publish static release tarballs, and
# taking one would be less code. Two reasons not to:
#
#   * this repository refuses unpinned downloads everywhere else, and the one binary a user
#     is asked to trust most — the daemon that unpacks and runs their images — is a poor
#     place to start making exceptions;
#   * upstream's tarball is built with upstream's tags, which include CRI and CNI. Dropping
#     those is a measured 5.8 MB per architecture out of a package whose whole cost is size
#     (BUILDTAGS records the numbers), and it cannot be done to a binary after the fact.
#
# Nothing produced here is committed. `git status` must stay clean after a run: a vendored
# binary in the repository is the one thing this script must never leave behind.
set -euo pipefail

goarch="${1:?usage: build.sh <goarch> <outdir> [srcdir]}"
outdir="${2:?usage: build.sh <goarch> <outdir> [srcdir]}"
srcdir="${3:-}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "$goarch" in
amd64) machine=x86-64 ;;
arm64) machine=aarch64 ;;
*)
	echo "unsupported architecture: $goarch (want amd64 or arm64)" >&2
	exit 1
	;;
esac

# The pins, with their comments stripped. Both files are mostly reasoning; the last
# non-comment line of CONTAINERD_VERSION is the tag, and every non-comment line of BUILDTAGS
# is a tag to build with.
version="$(grep -v '^[[:space:]]*#' "$here/CONTAINERD_VERSION" | grep -v '^[[:space:]]*$' | tail -1)"
buildtags="$(grep -v '^[[:space:]]*#' "$here/BUILDTAGS" | grep -v '^[[:space:]]*$' | tr '\n' ' ')"
buildtags="${buildtags% }"

# --- what each build tag must actually REMOVE from the binary -----------------------------
#
# `go build -tags` is silent about a tag that matches no build constraint anywhere: it is not
# a warning, it is not an error, the build succeeds and the plugin stays in. Measured against
# containerd v2.3.3 on linux/arm64 on 2026-08-16: changing `no_cri` to `no_cri_` — one
# character — produced a 35,586,210-byte daemon with CRI and CNI compiled in, 5.8 MB heavier
# than the 29,753,506 bytes BUILDTAGS' own table records, and exit 0 either way. Nothing here
# noticed, because the only post-build assertion was the ELF machine, which is identical.
#
# So each tag names the Go package path it is supposed to make disappear, and the built
# binary is checked for it. Package paths survive -trimpath and -ldflags "-s -w" (they are in
# the function name table, not the symbol table), which is what makes this readable from the
# artifact rather than inferred from the flags that produced it.
#
# A tag with no entry here is a hard error. That is the half that catches the typo: an
# unrecognised tag is either misspelled or new, and both need a human.
markers_for_tag() {
	case "$1" in
	urfave_cli_no_docs) printf '%s\n' 'github.com/cpuguy83/go-md2man' ;;
	no_cri) printf '%s\n' \
		'github.com/containerd/containerd/v2/plugins/cri' \
		'github.com/containerd/go-cni' ;;
	no_devmapper) printf '%s\n' 'github.com/containerd/containerd/v2/plugins/snapshots/devmapper' ;;
	no_zfs) printf '%s\n' 'github.com/containerd/zfs/v2' ;;
	no_tracing) printf '%s\n' 'github.com/containerd/containerd/v2/pkg/tracing/plugin' ;;
	*) return 1 ;;
	esac
}

# The POSITIVE CONTROL for every "must be absent" assertion above. If a future toolchain or
# ldflags change stripped package paths from the binary, every removal check would pass while
# testing nothing — the classic hollow check. erofs is the snapshotter Boks pins
# (internal/runtimecfg) and the one thing this daemon must not be built without, so its
# presence is both the control and an assertion worth having on its own.
REQUIRED_MARKER='github.com/containerd/containerd/v2/plugins/snapshots/erofs'

for tag in $buildtags; do
	markers_for_tag "$tag" >/dev/null || {
		echo "$here/BUILDTAGS names '$tag', which build.sh has no marker for." >&2
		echo "go build IGNORES a tag matching no build constraint, silently and with exit 0," >&2
		echo "so an unrecognised tag here is either a typo or a new tag that needs an entry in" >&2
		echo "markers_for_tag() saying which package it removes. Refusing to build." >&2
		exit 1
	}
done

[ -n "$version" ] || {
	echo "no version in $here/CONTAINERD_VERSION" >&2
	exit 1
}

command -v go >/dev/null || {
	echo "go is not on PATH; containerd is a Go program and this script cannot build it" >&2
	exit 1
}

cleanup=""
trap '[ -n "$cleanup" ] && rm -rf "$cleanup"' EXIT

if [ -z "$srcdir" ]; then
	cleanup="$(mktemp -d)"
	srcdir="$cleanup/containerd"
	echo "cloning containerd $version" >&2
	git clone --quiet --depth 1 --branch "$version" \
		https://github.com/containerd/containerd "$srcdir"
fi

# The checkout must be pristine, and this is checked rather than assumed. The Windows
# containerd carries six patches (packaging/containerd-windows/patches/) and the temptation
# to "just apply one here too" is exactly how a package stops being the upstream release it
# claims to be. If Linux ever does need a patch, add a patches/ directory, apply it here
# deliberately, and change this check and the README together — see README.md for the one
# candidate and why it is not applied.
dirty="$(git -C "$srcdir" status --porcelain)"
[ -z "$dirty" ] || {
	echo "the containerd checkout at $srcdir is not pristine:" >&2
	echo "$dirty" >&2
	echo "packaging/containerd-linux ships UNPATCHED upstream; refusing to build" >&2
	exit 1
}

mkdir -p "$outdir"
out="$outdir/containerd"

pkg="github.com/containerd/containerd/v2"
revision="$(git -C "$srcdir" rev-parse HEAD)"

# CGO_ENABLED=0 so the daemon is static and does not acquire a glibc floor from whichever
# runner built it. containerd's own release builds are static for the same reason, and unlike
# the nerdbox shim — which reaches libkrun through purego and therefore needs the dynamic
# loader — nothing in the daemon dlopens anything.
#
# -trimpath so the binary carries no build-host paths, and the version variables set the way
# containerd's own Makefile sets them, so `containerd --version` and the version this daemon
# reports over its API say v2.3.3 rather than an empty string. That matters beyond tidiness:
# `boks doctor`'s runtime-skew check compares the version the daemon *reports* against the
# containerd the shim was built against, and an unparseable version makes the check report
# nothing at all.
echo "building containerd $version for linux/$goarch with tags: $buildtags" >&2
(
	cd "$srcdir"
	CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
		go build -trimpath -tags "$buildtags" \
		-ldflags "-s -w -X $pkg/version.Version=$version -X $pkg/version.Revision=$revision -X $pkg/version.Package=$pkg" \
		-o "$out" ./cmd/containerd
)

# An ELF check rather than a magic-byte one: `go build -o containerd` will happily write a
# binary for the wrong GOARCH and the filename will not tell you, and the mistake surfaces on
# a user's machine as "cannot execute binary file". assert-elf.py reads e_machine.
python3 "$here/../linux/assert-elf.py" "$out" --machine "$machine"

# The control first, so that "absent" below means absent rather than unreadable.
if ! grep -qaF -- "$REQUIRED_MARKER" "$out"; then
	echo "the erofs snapshotter's package path is not in $out." >&2
	echo "Either the daemon was built without the one snapshotter Boks uses, or package" >&2
	echo "paths are no longer readable from the binary — in which case every tag assertion" >&2
	echo "below would pass while testing nothing, and this script must not be trusted until" >&2
	echo "markers_for_tag() is rewritten against whatever the binary does carry." >&2
	exit 1
fi
echo "control: $REQUIRED_MARKER is present, so package paths are readable" >&2

untaken=0
for tag in $buildtags; do
	while IFS= read -r marker; do
		if grep -qaF -- "$marker" "$out"; then
			echo "TAG DID NOT TAKE: -tags $tag left $marker in the binary" >&2
			untaken=$((untaken + 1))
		else
			echo "  $tag removed $marker" >&2
		fi
	done < <(markers_for_tag "$tag")
done
if [ "$untaken" -ne 0 ]; then
	echo "$untaken build tag(s) had no effect on the binary; see above" >&2
	echo "go build accepts a tag that matches no constraint without complaint, so a tag" >&2
	echo "spelled right for a plugin that has moved fails exactly this way." >&2
	exit 1
fi

echo "built $out ($(stat -c%s "$out") bytes, containerd $version, linux/$goarch)"
