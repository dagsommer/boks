#!/bin/sh
# Answer Claude Code's two first-run questions before it asks them.
#
# In a sandbox both are noise. "Do you trust the files in this folder?" is asked about a
# directory the user named on the command line a moment earlier, and the bypass-permissions
# warning describes a decision `boks run claude` already made for them: the agent's argv is
# `claude --dangerously-skip-permissions`, because the VM is the boundary and asking the agent
# to confirm its own actions inside it adds friction without adding isolation (see
# internal/agent/agent.go). Neither question can be answered any way but yes, and both stop a
# non-interactive run dead.
#
# The two keys are Claude Code's own, read out of the installed binary at 2.1.234 rather than
# guessed: `hasTrustDialogAccepted` under projects/<absolute path>, and
# `bypassPermissionsModeAccepted` at the root. IS_SANDBOX is set alongside them because the
# binary reads that too, and it is true here in the strongest sense available.
#
# MERGED, never written over. ~/.claude.json holds real state after a login, and this script
# runs on every start of a sandbox whose filesystem persists — a clobber would log the user out
# on the second run. Python because the base image has it and merging JSON in sh does not end
# well.
set -eu

config="${HOME}/.claude.json"
workspace="$(pwd)"

python3 - "$config" "$workspace" <<'EOF'
import json, os, sys

path, workspace = sys.argv[1], sys.argv[2]

# A file that exists but is not readable JSON is left completely alone. Overwriting it would
# be destroying state to remove a prompt, which is the wrong trade in every case.
if os.path.exists(path):
    try:
        with open(path) as fh:
            config = json.load(fh)
    except (OSError, ValueError) as exc:
        print(f"boks: leaving {path} alone ({exc})", file=sys.stderr)
        raise SystemExit(0)
    if not isinstance(config, dict):
        print(f"boks: leaving {path} alone (not a JSON object)", file=sys.stderr)
        raise SystemExit(0)
else:
    config = {}

config.setdefault("bypassPermissionsModeAccepted", True)

projects = config.setdefault("projects", {})
if isinstance(projects, dict):
    entry = projects.setdefault(workspace, {})
    if isinstance(entry, dict):
        entry.setdefault("hasTrustDialogAccepted", True)

# Written through a temporary file in the same directory, so an interrupted start cannot leave
# a half-written config where a whole one was.
tmp = path + ".boks-tmp"
with open(tmp, "w") as fh:
    json.dump(config, fh, indent=2)
os.replace(tmp, path)
EOF
