#!/bin/sh
# Every in-site link that names a heading points at a heading that exists.
#
# This is the failure the site is most prone to, and the one nothing else would catch: a
# document links to a section of another document by its slug, a later edit renames the
# heading, and the link silently starts landing at the top of a long page. No build has any
# reason to complain, and the reader is the one who finds out.
#
# Usage: sh site/check-anchors.sh [_site directory]
set -eu

root=${1:-site/_site}
errors=$(mktemp)
trap 'rm -f "$errors"' EXIT

# The site's own links only. Where an external one points is not this build's business.
# -a because the rendered parity matrix contains bytes grep calls binary.
grep -rahoE 'href="/[a-z0-9./-]*#[A-Za-z0-9_-]+"' "$root" |
	sed 's/^href="//; s/"$//' | sort -u |
	while IFS= read -r link; do
		page=${link%%#*}
		anchor=${link#*#}
		case "$page" in
		*/) file="$root${page}index.html" ;;
		*) file="$root$page" ;;
		esac
		if [ ! -f "$file" ]; then
			echo "  $link — no such page" >>"$errors"
		elif ! grep -qa "id=\"$anchor\"" "$file"; then
			echo "  $link — no heading with that id" >>"$errors"
		fi
	done

if [ -s "$errors" ]; then
	echo "cross-document links point at headings that are not there:" >&2
	cat "$errors" >&2
	exit 1
fi
echo "cross-document anchors: every one resolves"
