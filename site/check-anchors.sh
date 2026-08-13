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

# The site is served under a base path (/boks on project pages, possibly empty under a
# custom domain), and every generated link carries it. Rather than hardcoding it twice,
# read it off the landing page's own stylesheet link, which Liquid prefixed at build time.
base=$(grep -o 'href="[^"]*/assets/site.css"' "$root/index.html" | head -n1 |
	sed 's/^href="//; s|/assets/site.css"$||')

# The site's own links only. Where an external one points is not this build's business.
# -a because rendered pages can contain bytes grep calls binary.
grep -rahoE 'href="/[a-z0-9./-]*#[A-Za-z0-9_-]+"' "$root" |
	sed 's/^href="//; s/"$//' | sort -u |
	while IFS= read -r link; do
		case "$link" in
		"$base"/*) rel=${link#"$base"} ;;
		*)
			echo "  $link — outside the site's base path '$base'" >>"$errors"
			continue
			;;
		esac
		page=${rel%%#*}
		anchor=${rel#*#}
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
