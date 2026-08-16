#!/usr/bin/env bash
#
# Render the Homebrew tap for one release, in the layout dagsommer/homebrew-boks expects.
#
#   packaging/homebrew/render.sh <version> <source-sha256-or-file> <guest-sha256-or-file> [outdir]
#
#   packaging/homebrew/render.sh 0.1.0 dist/v0.1.0.tar.gz dist/SHA256SUMS
#   packaging/homebrew/render.sh 0.1.0 a1b2…64-hex… 6df1…64-hex…
#
# Output goes to <outdir>, which is the *root* of the tap repository — `Formula/boks.rb`,
# `Formula/nerdbox.rb` and `README.md` — so the rendered tree can be copied over a
# homebrew-boks checkout wholesale. Default outdir is dist/homebrew.
#
# Nothing here is hand-editable on purpose. The three values a release changes — the version,
# the source tarball's SHA-256 and the guest archive's SHA-256 — appear in the templates as
# {{VERSION}}, {{SHA256}} and {{GUEST_SHA256}} and nowhere else, so stamping a release is this
# script and no diff review.
#
# Template lines beginning `#|` are our own notes and are stripped, so what reaches the tap is
# a formula rather than a formula plus an essay about it. A `.rb.in` is still valid Ruby —
# `#|` is a comment and the placeholders live inside string literals — so `ruby -c` checks the
# template as well as the rendered file.
#
# The two digests this needs:
#
#   source  the GitHub-generated tag tarball, which is not in the release's SHA256SUMS
#           because GitHub generates it rather than this project:
#             curl -sL https://github.com/dagsommer/boks/archive/refs/tags/v<version>.tar.gz | shasum -a 256
#   guest   boks-guest_<version>_arm64.tar.gz, which *is* in SHA256SUMS. Pass the SHA256SUMS
#           file and this script will read the right line out of it.
#
# What this script does not do: prove the formulae install. No one on this project has a macOS
# machine. See packaging/homebrew/README.md for exactly what has been run against them and
# what has not.
set -euo pipefail

version="${1:?usage: render.sh <version> <source-sha256-or-file> <guest-sha256-or-file> [outdir]}"
source_arg="${2:?usage: render.sh <version> <source-sha256-or-file> <guest-sha256-or-file> [outdir]}"
guest_arg="${3:?usage: render.sh <version> <source-sha256-or-file> <guest-sha256-or-file> [outdir]}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
outdir="${4:-$root/dist/homebrew}"

# A version with a leading v would produce a formula whose `version` disagrees with every
# other artifact in the release, and Homebrew treats it as an opaque string, so nothing
# downstream would catch it.
case "$version" in
v*) echo "render.sh: version must not carry a leading v (got '$version')" >&2; exit 1 ;;
esac

# macOS has shasum and no sha256sum; Linux has both. This is the one place the difference
# shows, and the maintainer runs this on macOS.
sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum -- "$1" | cut -d' ' -f1
	else
		shasum -a 256 -- "$1" | cut -d' ' -f1
	fi
}

# sha256_of <arg> <name-in-SHA256SUMS>
#
# Either a 64-hex-digit digest, a file to hash, or a SHA256SUMS listing to look the name up
# in. Accepting only the digest would mean a human transcribing it, which is the one step in a
# release worth removing.
sha256_of() {
	local arg="$1" want="$2" out
	if [ -f "$arg" ]; then
		if head -n1 -- "$arg" | grep -Eq '^[0-9a-fA-F]{64} '; then
			out="$(awk -v n="$want" '$2 == n || $2 == "*" n { print $1 }' "$arg")"
			if [ -z "$out" ]; then
				echo "render.sh: $arg has no line for $want" >&2
				exit 1
			fi
			echo "render.sh: read $want from $arg" >&2
		else
			out="$(sha256_file "$arg")"
			echo "render.sh: computed sha256 of $arg" >&2
		fi
	else
		out="$arg"
	fi
	out="$(printf '%s' "$out" | tr '[:upper:]' '[:lower:]')"
	case "$out" in
	*[!0-9a-f]* | "")
		echo "render.sh: '$arg' is neither a file nor a hex digest" >&2
		exit 1
		;;
	esac
	if [ "${#out}" -ne 64 ]; then
		echo "render.sh: sha256 must be 64 hex characters, got ${#out}" >&2
		exit 1
	fi
	printf '%s' "$out"
}

source_sha256="$(sha256_of "$source_arg" "v${version}.tar.gz")"
guest_sha256="$(sha256_of "$guest_arg" "boks-guest_${version}_arm64.tar.gz")"

mkdir -p "$outdir/Formula"

for template in "$here"/tap/Formula/*.rb.in; do
	name="$(basename "$template" .in)"
	# `#|` lines are notes to us. They are deleted first, so a note may mention a
	# placeholder without the substitutions below turning it into nonsense — and so the
	# file that reaches the tap carries a formula and not our reasoning about it.
	sed \
		-e '/^#|/d' \
		-e "s|{{VERSION}}|${version}|g" \
		-e "s|{{SHA256}}|${source_sha256}|g" \
		-e "s|{{GUEST_SHA256}}|${guest_sha256}|g" \
		"$template" >"$outdir/Formula/$name"
done

# The tap's README is not templated — it describes the tap rather than a release — but it is
# part of the tree the tap repository needs, so it is copied here rather than left to be
# remembered.
cp "$here/tap/README.md" "$outdir/README.md"

# A template that still contains a placeholder is a formula that will fail far away from here,
# at a checksum mismatch with no explanation. Catch it now.
if grep -l '{{[A-Z_0-9]*}}' "$outdir"/Formula/*.rb "$outdir/README.md"; then
	echo "render.sh: the files above still contain unsubstituted placeholders" >&2
	exit 1
fi

echo "rendered $outdir"
find "$outdir" -type f | sed "s|^$outdir/||" | sort

# Ruby syntax is the floor and it is always checkable. `brew style` and `brew audit` are the
# two that catch what a human reviewer would, so run them when a brew is on PATH — including
# a Homebrew on Linux, which loads and lints a macOS-only formula perfectly well even though
# it can never install one.
status=0
if command -v ruby >/dev/null 2>&1; then
	for f in "$outdir"/Formula/*.rb; do
		ruby -c "$f" >/dev/null || status=1
	done
	echo "render.sh: ruby -c passed"
else
	echo "render.sh: ruby not available; skipped syntax check" >&2
fi

if command -v brew >/dev/null 2>&1; then
	tap_dir="$(brew --repository dagsommer/boks 2>/dev/null || true)"
	if [ -z "$tap_dir" ] || [ ! -d "$tap_dir" ]; then
		echo "render.sh: dagsommer/boks is not tapped locally; skipped brew style/audit" >&2
		echo "render.sh: run 'brew tap-new dagsommer/boks --no-git' to enable them" >&2
	elif [ -e "$tap_dir/.git" ]; then
		# A tap with a git repository is the real dagsommer/homebrew-boks clone, and
		# overwriting somebody's checkout as a side effect of linting is not this
		# script's business. `brew tap-new --no-git` makes the scratch tap these
		# checks want.
		echo "render.sh: $tap_dir is a git checkout of the real tap; skipped brew style/audit" >&2
		echo "render.sh: 'brew untap dagsommer/boks && brew tap-new dagsommer/boks --no-git' to lint here" >&2
	else
		mkdir -p "$tap_dir/Formula"
		cp "$outdir"/Formula/*.rb "$tap_dir/Formula/"
		brew style dagsommer/boks || status=1
		# --new is the strictest set Homebrew has for a formula nobody has seen
		# before, and naming the formulae (rather than passing --tap) is what makes
		# audit run RuboCop too. Until the tag is pushed the source URL 404s and audit
		# says so; that finding is expected before a release and a real one after it.
		brew audit --strict --new dagsommer/boks/boks dagsommer/boks/nerdbox || status=1
	fi
else
	echo "render.sh: brew not available; skipped brew style/audit" >&2
fi

exit "$status"
