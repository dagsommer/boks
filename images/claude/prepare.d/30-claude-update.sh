#!/bin/sh
# Bring Claude Code up to date the first time a sandbox starts, in the background.
#
# The image carries the release that was current when it was built, which by the time a
# sandbox is created can be weeks old. `claude update` installs the latest into the agent's
# home (~/.local/bin, ahead of /usr/local/bin on PATH) as a native install, and Claude Code's
# own auto-updater keeps that current from then on — so this only has to happen once per
# sandbox. It runs in the background so the agent starts at once, on the image's version;
# the next start is on the new one.
#
# Never fatal, and quiet: a sandbox whose policy does not allow downloads.claude.ai simply
# stays on the image's version. The output goes to ~/.cache/boks/claude-update.log.
# BOKS_NO_CLAUDE_UPDATE=1 skips it.
set -u

[ "${BOKS_NO_CLAUDE_UPDATE:-}" = "1" ] && exit 0
command -v claude >/dev/null 2>&1 || exit 0

state="${HOME}/.cache/boks"
marker="$state/claude-updated"
[ -e "$marker" ] && exit 0
mkdir -p "$state" 2>/dev/null || exit 0

# setsid and redirected stdio, so the update outlives boks-prepare and holds none of the
# agent's terminal.
setsid sh -c 'claude update && touch "$1"' sh "$marker" \
	>"$state/claude-update.log" 2>&1 </dev/null &
exit 0
