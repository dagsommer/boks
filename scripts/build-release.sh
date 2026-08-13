#!/usr/bin/env bash
#
# Build one release artifact: a boks binary for one GOOS/GOARCH, its tarball, and the
# tarball's SHA-256.
#
#   scripts/build-release.sh <goos> <goarch> [version] [outdir]
#
# The workflow calls this once per target and so can you, which is the point: the release
# is not a thing that only exists inside GitHub Actions. Everything Boks ships is pure Go
# with no cgo, so every target cross-compiles from any host and the result does not depend
# on which runner produced it.
#
# The tarball is deterministic. Entries are sorted, ownership is zeroed, timestamps come
# from SOURCE_DATE_EPOCH (defaulting to the commit date) and gzip is told not to record a
# name or a time. Building the same commit twice gives the same bytes, so a published
# checksum is something a third party can reproduce rather than merely compare against
# itself. `make release-verify` checks exactly that.
set -euo pipefail

goos="${1:?usage: build-release.sh <goos> <goarch> [version] [outdir]}"
goarch="${2:?usage: build-release.sh <goos> <goarch> [version] [outdir]}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${3:-$("$root/scripts/image-tag.sh")}"
outdir="${4:-$root/dist}"

# The commit date, not the wall clock: a release rebuilt tomorrow must not differ from the
# one built today. Falls back to a fixed epoch outside a git checkout.
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH="$(git -C "$root" log -1 --format=%ct 2>/dev/null || echo 0)"
fi
export SOURCE_DATE_EPOCH

name="boks_${version}_${goos}_${goarch}"
binary="boks"
[ "$goos" = windows ] && binary="boks.exe"

staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT
mkdir -p "$staging/$name" "$outdir"

# -trimpath so the binary carries no build-host paths; CGO_ENABLED=0 because nothing here
# needs libc and a static binary is what a tarball should contain. Version is stamped into
# the same variable `make build` stamps, so `boks --version` reports the release tag.
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
	go build -trimpath \
	-ldflags "-s -w -X github.com/dagsommer/boks/internal/cli.Version=${version}" \
	-o "$staging/$name/$binary" "$root/cmd/boks"

cp "$root/LICENSE" "$root/README.md" "$staging/$name/"

tar --sort=name --numeric-owner --owner=0 --group=0 \
	--mtime="@${SOURCE_DATE_EPOCH}" \
	-C "$staging" -cf - "$name" | gzip -9n >"$outdir/${name}.tar.gz"

(cd "$outdir" && sha256sum "${name}.tar.gz" >"${name}.tar.gz.sha256")

echo "built ${outdir}/${name}.tar.gz"
cat "$outdir/${name}.tar.gz.sha256"
