#!/usr/bin/env bash
#
# Render the winget manifests for one release, in the layout microsoft/winget-pkgs expects.
#
#   packaging/winget/render.sh <version> <sha256-or-zip-path> [outdir]
#
#   packaging/winget/render.sh 0.1.0 dist/boks_0.1.0_windows_amd64.zip
#   packaging/winget/render.sh 0.1.0 8f14e45fceea167a5a36dedd4bea2543...
#
# Output goes to <outdir>/manifests/d/dagsommer/boks/<version>/, which is the path a
# winget-pkgs pull request has to place them at, so the rendered tree can be copied over a
# winget-pkgs checkout wholesale. Default outdir is dist/winget.
#
# Nothing here is hand-editable on purpose. The three values a release changes — the
# version, the archive's SHA-256 and the release date — appear in the templates as
# {{VERSION}}, {{INSTALLER_SHA256}} and {{RELEASE_DATE}} and nowhere else, so stamping a
# release is this script and no diff review.
#
# Template lines beginning `#|` are our own notes and are stripped, so what reaches
# winget-pkgs is a manifest rather than a manifest plus an essay about it.
#
# This has never been run against winget itself. See packaging/winget/README.md: no one on
# this project has a Windows machine, `winget validate` has not been run on these files,
# and the only checking they have had is the schema validation this script performs at the
# end, on Linux, against the JSON schemas winget-pkgs' own tooling targets.
set -euo pipefail

version="${1:?usage: render.sh <version> <sha256-or-zip-path> [outdir]}"
digest_arg="${2:?usage: render.sh <version> <sha256-or-zip-path> [outdir]}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
outdir="${3:-$root/dist/winget}"

# The identifier is also the filename stem and the directory path, so it is written once.
identifier="dagsommer.boks"

# A version with a leading v would produce a PackageVersion that disagrees with every other
# artifact in the release, and winget treats PackageVersion as an opaque string, so nothing
# downstream would catch it.
case "$version" in
v*) echo "render.sh: version must not carry a leading v (got '$version')" >&2; exit 1 ;;
esac

# Either a 64-hex-digit digest or a file to compute one from. Accepting only the digest
# would mean a human transcribing it, which is the one step in a release worth removing.
if [ -f "$digest_arg" ]; then
	sha256="$(sha256sum -- "$digest_arg" | cut -d' ' -f1)"
	echo "render.sh: computed sha256 of $digest_arg" >&2
else
	sha256="$digest_arg"
fi

sha256="$(printf '%s' "$sha256" | tr '[:lower:]' '[:upper:]')"
case "$sha256" in
*[!0-9A-F]* | "") echo "render.sh: '$digest_arg' is neither a file nor a hex digest" >&2; exit 1 ;;
esac
if [ "${#sha256}" -ne 64 ]; then
	echo "render.sh: sha256 must be 64 hex characters, got ${#sha256}" >&2
	exit 1
fi

# The release date, not today's date: rendering the same release twice must produce the
# same bytes, for the same reason scripts/build-release.sh takes its timestamps from the
# commit. SOURCE_DATE_EPOCH overrides, and the tag's own date is the default when there is
# one to read.
if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
	release_date="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%d 2>/dev/null ||
		date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%d)"
elif tagged="$(git -C "$root" log -1 --format=%cI "v${version}" 2>/dev/null)" && [ -n "$tagged" ]; then
	release_date="${tagged%%T*}"
else
	release_date="$(date -u +%Y-%m-%d)"
	echo "render.sh: no tag v${version} and no SOURCE_DATE_EPOCH; using today ($release_date)" >&2
fi

# d for dagsommer: winget-pkgs shards by the lowercased first character of the identifier.
first="$(printf '%s' "$identifier" | cut -c1 | tr '[:upper:]' '[:lower:]')"
dest="$outdir/manifests/$first/dagsommer/boks/$version"
mkdir -p "$dest"

for template in "$here"/manifests/*.yaml.in; do
	name="$(basename "$template" .in)"
	# `#|` lines are notes to us. They are deleted first, so a note may mention a
	# placeholder without the substitutions below turning it into nonsense — and so the
	# file that reaches winget-pkgs carries a manifest and not our reasoning about it.
	sed \
		-e '/^#|/d' \
		-e "s|{{VERSION}}|${version}|g" \
		-e "s|{{INSTALLER_SHA256}}|${sha256}|g" \
		-e "s|{{RELEASE_DATE}}|${release_date}|g" \
		"$template" >"$dest/$name"
done

# A template that still contains a placeholder is a manifest that will be rejected far away
# from here, by a bot, in someone else's repository. Catch it now.
if grep -l '{{[A-Z_]*}}' "$dest"/*.yaml; then
	echo "render.sh: the files above still contain unsubstituted placeholders" >&2
	exit 1
fi

echo "rendered $dest"
ls -1 "$dest"

# Validate if the tooling is here. It is not a substitute for `winget validate` and the
# README says so; it does catch every structural mistake a schema can express, which is
# most of the ways these files go wrong.
if python3 -c 'import jsonschema, yaml' 2>/dev/null; then
	python3 "$here/validate.py" "$dest"
else
	echo "render.sh: python3 with jsonschema and pyyaml not available; skipped schema validation" >&2
fi
