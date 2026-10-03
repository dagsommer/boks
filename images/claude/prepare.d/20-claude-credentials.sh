#!/bin/sh
# Put the adopted credential where Claude Code looks for it.
#
# Boks mounts a rendered credential file read-only at /etc/boks/credentials/ and names it in
# BOKS_CREDENTIAL_FILE_CLAUDE_CODE. It cannot mount it at ~/.claude/.credentials.json directly,
# and internal/secret/import.go says why: Boks shares DIRECTORIES, so doing that would mount
# the whole of ~/.claude read-only and break everything else the agent keeps there. The same
# comment says the copy "belongs to the image" — this is that copy, which did not exist until
# 2026-08-29, so an adopted credential was mounted, never installed, and Claude Code asked the
# user to log in inside the sandbox as if nothing had been adopted at all.
#
# Nobody noticed because until 0.1.16 the CA was mounted over /etc/boks, which hid
# /etc/boks/prepare.d, so no script here ran.
#
# The file holds SENTINELS, not tokens: the proxy swaps them for the real values on the way
# out (internal/secret). Copying it into the guest therefore puts nothing secret in the
# sandbox's filesystem, which is the property that makes this safe to do at every start.
set -eu

src="${BOKS_CREDENTIAL_FILE_CLAUDE_CODE:-}"
[ -n "$src" ] || exit 0
[ -r "$src" ] || exit 0

case "${HOME:-}" in
"" | "/")
	echo "boks: HOME is ${HOME:-unset}, so Claude Code's credential cannot be installed." >&2
	exit 0
	;;
esac

dest="$HOME/.claude/.credentials.json"

# Rewritten on every start rather than only when absent. The mounted file is re-rendered each
# time the sandbox starts, and after a login performed inside the sandbox it carries different
# sentinels — so "leave it alone if it exists" would pin the agent to a stale pair. Nothing is
# lost by overwriting: what this replaces is the same sentinels or an older set of them.
if ! mkdir -p "$HOME/.claude" 2>/dev/null; then
	echo "boks: cannot create $HOME/.claude; Claude Code will ask you to log in." >&2
	exit 0
fi

tmp="$dest.boks-tmp.$$"
if ! cat "$src" 2>/dev/null >"$tmp"; then
	rm -f "$tmp" 2>/dev/null || true
	echo "boks: cannot write $dest; Claude Code will ask you to log in." >&2
	exit 0
fi
chmod 0600 "$tmp" 2>/dev/null || true
mv "$tmp" "$dest"

# With the full login installed, CLAUDE_CODE_OAUTH_TOKEN has to go. Claude Code reads that
# variable first and takes it for a `claude setup-token` long-lived token, and a long-lived
# token cannot use remote control ("only works with a normal login") — although the file just
# installed is a normal login, scopes and all. The variable stays for an image that installs
# no file, where it is the only way in; here it is redundant, and it costs a feature.
# Reported 2026-10-03.
if [ -n "${BOKS_UNSET_ENV_FILE:-}" ]; then
	echo CLAUDE_CODE_OAUTH_TOKEN >>"$BOKS_UNSET_ENV_FILE"
fi
