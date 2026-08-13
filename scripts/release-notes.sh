#!/bin/sh
# Generate the release-notes entry for a tag from the commits since the previous tag.
#
# POSIX sh and git, nothing else. A changelog generator is not worth a language runtime, a
# lockfile and a supply chain, and this repository has a no-new-dependency rule that a
# documentation task is a poor reason to break.
#
# # Why the first entry is hand-written
#
# There are no tags yet. The 157 commits before the first one are mostly prefixed, but with
# this project's own vocabulary rather than the conventional one — `cli:`, `policy:`,
# `enforce:`, `ports:` alongside `feat:` and `fix:` — and the merges are prose. Run over all
# of it, this script produces a list that is technically complete and useless to read. So
# 0.1.0's entry is written by hand, once, and generation starts from the tag after it. That
# is the honest split: a generator that would produce a mess is not run over the range where
# it would.
#
# # Cutting a release
#
#   1. Move ImageTag in internal/agent/agent.go to the new version. It is the single place
#      the version is spelled; the Makefile and .github/workflows/images.yml both read it,
#      and images.yml refuses to publish when the git tag does not match it.
#   2. make check && make docs        (docs/cli.md must be current, or the tests fail)
#   3. make release-notes TAG=vX.Y.Z INSERT=--insert
#   4. Read what it wrote. Edit the prose freely — a generated first draft is a draft. What
#      must not change is a claim: this file says what shipped, not what it is hoped to do.
#   5. Commit, then `git tag vX.Y.Z && git push --tags`.
#
# # The vocabulary
#
# feat/fix/docs/test are bucketed. Everything else keeps its subject verbatim under "Other
# changes", including this project's domain prefixes, so nothing is ever dropped for failing
# to match a regular expression. Merge commits are skipped: a merge's subject describes the
# branch whose commits are already listed.

set -eu

usage() {
	cat >&2 <<'EOF'
usage: scripts/release-notes.sh [--insert] TAG [PREVIOUS_TAG]

  --insert   write the entry into docs/release-notes.md instead of printing it
  TAG        the tag being released, e.g. v0.1.1 (need not exist yet: HEAD is used)
  PREVIOUS   the tag to start from (default: the most recent tag before TAG)
EOF
	exit 2
}

insert=no
case "${1:-}" in
--insert)
	insert=yes
	shift
	;;
-h | --help) usage ;;
esac

tag="${1:-}"
[ -n "$tag" ] || usage
prev="${2:-}"

root=$(git rev-parse --show-toplevel)
notes="$root/docs/release-notes.md"
marker='<!-- release-notes: generated entries are inserted directly below this line -->'

# The tag may not exist yet — you write the notes, then tag. Resolve to HEAD if so.
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	head="$tag"
else
	head=HEAD
fi

if [ -z "$prev" ]; then
	prev=$(git describe --tags --abbrev=0 "$head^" 2>/dev/null || true)
fi

if [ -z "$prev" ]; then
	cat >&2 <<EOF
release-notes: no previous tag, so there is no range to generate from.

$tag would be the first release. Its entry is hand-written — see the "Before 0.1.0" section
of docs/release-notes.md and the comment at the top of this script for why. Write it there,
tag, and every release after this one generates.

If you meant to generate from a specific starting point, name it:

  scripts/release-notes.sh $tag <starting-commit-or-tag>
EOF
	exit 1
fi

range="$prev..$head"
date=$(git log -1 --format=%cd --date=short "$head")

# One subject per line, oldest first, merges excluded.
subjects=$(git log --no-merges --reverse --format='%s' "$range")
if [ -z "$subjects" ]; then
	echo "release-notes: no commits in $range" >&2
	exit 1
fi

# section TYPE HEADING — prints the heading and its bullets, or nothing if there are none.
section() {
	type=$1
	heading=$2
	body=$(printf '%s\n' "$subjects" |
		sed -n "s/^${type}\(([^)]*)\)\{0,1\}!\{0,1\}: *//p" |
		sed 's/^/- /')
	[ -n "$body" ] || return 0
	printf '\n### %s\n\n%s\n' "$heading" "$body"
}

# Everything the buckets above did not claim, subject kept exactly as written.
other() {
	body=$(printf '%s\n' "$subjects" |
		grep -vE '^(feat|fix|docs|test)(\([^)]*\))?!?: ' |
		sed 's/^/- /')
	[ -n "$body" ] || return 0
	printf '\n### Other changes\n\n%s\n' "$body"
}

entry=$(
	printf '## %s — %s\n' "$tag" "$date"
	printf '\n[Commits](https://github.com/dagsommer/boks/compare/%s...%s), %s of them.\n' \
		"$prev" "$tag" "$(printf '%s\n' "$subjects" | wc -l | tr -d ' ')"
	section feat "Added"
	section fix "Fixed"
	section docs "Documentation"
	section test "Tests"
	other
)

if [ "$insert" = no ]; then
	printf '%s\n' "$entry"
	exit 0
fi

grep -qF "$marker" "$notes" || {
	echo "release-notes: $notes has no insertion marker; add it back or paste by hand" >&2
	exit 1
}

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
awk -v marker="$marker" -v entry="$entry" '
  { print }
  index($0, marker) == 1 { print ""; print entry }
' "$notes" >"$tmp"
mv "$tmp" "$notes"
echo "release-notes: wrote the $tag entry into docs/release-notes.md — read it before committing" >&2
