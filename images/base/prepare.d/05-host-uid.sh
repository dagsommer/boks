#!/bin/sh
# Give the running uid an entry in /etc/passwd, because no image can have one for it.
#
# A sandbox with a workspace runs as the uid that owns that workspace ON THE HOST — 502 on a
# typical Mac — so that files the agent creates belong to whoever opens them afterwards
# (internal/sandbox/hostuser.go). No image's passwd knows that number, and a surprising amount
# of software calls getpwuid() and treats "no such user" as fatal rather than as missing
# metadata:
#
#   OpenSSH   "No user exists for uid 502", before it opens a socket — so no ssh, no scp,
#             no git-over-ssh, no deploy.
#   git       falls back to an author of "unknown", or refuses, depending on version.
#   python    getpass.getuser() raises, which takes tooling with it.
#
# HOME is already handled in the image's environment, where no lookup is needed. This is the
# rest of the record, and it can only be written once the uid is known, which is here.
#
# Written to the sandbox's own filesystem, so it persists in the snapshot: `boks exec` does not
# run the entrypoint, and the entry is there for it because this ran when the sandbox started.
set -eu

passwd="${BOKS_PASSWD_FILE:-/etc/passwd}"
group="${BOKS_GROUP_FILE:-/etc/group}"
uid="$(id -u)"
gid="$(id -g)"

# The file is read rather than getent, because getent answers from NSS as configured and this
# has to reason about the file it is going to append to.
known() {
	cut -d: -f3 "$1" 2>/dev/null | grep -qx "$2"
}

if known "$passwd" "$uid"; then
	exit 0
fi

# uid 0 needs no help and must not be handed a second root entry.
if [ "$uid" = "0" ]; then
	exit 0
fi

home="${HOME:-/home/agent}"
shell=/bin/sh
[ -x /bin/bash ] && shell=/bin/bash

# Named for what it is. `agent` is taken by the image's own uid 1000, and two entries with one
# name is worse than an honest second name: `id` would resolve it either way, and a user
# reading `ls -l` deserves to see which one they are.
# 2>/dev/null BEFORE the redirection that can fail: redirections are applied left to right,
# so the other order lets the shell's own "cannot create ...: Permission denied" through first.
if ! printf 'boks:x:%s:%s:boks sandbox user:%s:%s\n' "$uid" "$gid" "$home" "$shell" 2>/dev/null >>"$passwd"; then
	echo "boks: cannot add uid $uid to $passwd, so getpwuid() will keep failing for it." >&2
	echo "      ssh aborts with \"No user exists for uid $uid\"; git and python tooling may too." >&2
	echo "      An image run as a foreign uid needs $passwd writable; see images/base/Dockerfile." >&2
	exit 0
fi

# The group matters less — most callers tolerate a numeric gid — but it costs one line and it
# is the same failure one layer down.
if ! known "$group" "$gid"; then
	printf 'boks:x:%s:\n' "$gid" 2>/dev/null >>"$group" || true
fi
