package cli

import (
	"fmt"

	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

// cloneFlagHelp is the one-line description of --clone. It leads with the property rather
// than the mechanism, because the property is why anyone would want it.
const cloneFlagHelp = "keep guest writes off your disk: work on a git clone made inside the " +
	"guest, with the host repository shared read-only at " + workspace.SourcePath

// applyCloneMode decides how a sandbox's workspace reaches its guest, and says so.
//
// It is the whole of the --clone decision, in one place, because the flag has three quite
// different meanings depending on what it is pointed at: a request that has to be validated,
// a no-op on a sandbox that already exists, and — for a repository with submodules or LFS —
// a request that is granted with a caveat. Spreading that across `run` and `create` would
// eventually give the two commands different answers.
func applyCloneMode(f *sandboxFlags, inv invocation, cfg *sandbox.Config, env Env) error {
	if !f.clone {
		return nil
	}
	// A sandbox already in clone mode is a harmless no-op worth acknowledging. One in
	// direct mode never reaches here: checkFixedAtCreation refuses it, alongside every
	// other flag that is fixed when a sandbox is created, because silently ignoring
	// --clone hands the guest a read-write share of the repository the flag exists to
	// keep it out of.
	if inv.exists {
		if inv.info.Filesystem.IsClone() {
			fmt.Fprint(env.Stderr, cloneIgnoredNote(inv))
		}
		return nil
	}
	if len(inv.workspaces) == 0 {
		return fmt.Errorf("--clone needs a workspace to clone")
	}

	repo, err := workspace.InspectRepo(inv.workspaces[0].HostPath)
	if err != nil {
		return err
	}
	// One writable share anywhere in the sandbox would undo the property --clone exists
	// for, and a mode that holds "except for the second directory you passed" is not a
	// mode anyone can reason about. Refusing is louder than silently downgrading it.
	for _, ws := range inv.workspaces[1:] {
		if !ws.ReadOnly() {
			return fmt.Errorf(
				"--clone keeps guest writes off your disk, but %s would still be shared\n"+
					"read-write, which would undo that. Share it read-only by writing it as\n"+
					"%s:ro, or drop --clone.", ws.HostPath, ws.HostPath)
		}
	}

	for _, note := range repo.Notes() {
		fmt.Fprintf(env.Stderr, "note: %s.\n", note)
	}
	fmt.Fprint(env.Stderr, bundleHint(inv.name))
	cfg.Clone = true
	return nil
}

// cloneIgnoredNote is what a --clone on an existing sandbox gets.
//
// The mode lives in the container's OCI mounts, which are written once and never revisited,
// so it cannot be changed on a sandbox that exists — the same reason a workspace cannot. The
// two cases are told apart because only one of them is a surprise worth acting on: a
// re-attach to a *direct* sandbox is about to let the guest write to the user's disk after
// they asked for the opposite.
func cloneIgnoredNote(inv invocation) string {
	if inv.info.Filesystem.IsClone() {
		return fmt.Sprintf(
			"note: sandbox %q is already in clone mode; --clone does nothing on a re-attach.\n",
			inv.name)
	}
	// Unreachable since 2026-08-15: a direct-mode sandbox is refused by
	// checkFixedAtCreation before this is called. Kept so the caller has a total
	// function, and because a future caller reaching it would be a bug worth seeing
	// rather than an empty string.
	return fmt.Sprintf(
		"warning: --clone is ignored: sandbox %q exists and is in DIRECT mode, so the guest\n"+
			"         writes to %s on your disk. The mode is fixed when a sandbox is created.\n"+
			"         Remove it with 'boks rm %s' and run again to get clone mode.\n",
		inv.name, inv.info.Workspace(), inv.name)
}
