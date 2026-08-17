package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/purge"
)

// newPurgeCommand removes the host-side state Boks writes.
//
// The name is not `boks daemon purge`, which is what was first proposed. Most of the bytes
// are the daemon's — containerd's content store and its unpacked snapshots — but the
// directory also holds the local certificate authority's private key, the encrypted
// credential store, the policy rules the user typed and the decision log. A subcommand of
// `daemon` that deleted a CA would be filed under the wrong noun, and the one place a user
// must not be surprised by what a command reaches is this one.
//
// It is not `boks uninstall` either. Boks cannot remove the binary that winget, Homebrew,
// dpkg or rpm installed, and a command called uninstall that leaves the program behind is a
// worse lie than no command at all. Uninstalling is two steps and this page says so.
//
// So: a top-level verb, alongside `run`, `rm` and `cp`, that names exactly what it does.
func newPurgeCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "purge [flags]",
		Short: "Remove the host-side state Boks has written, and report what it frees",
		Long: `Removes what Boks has written outside its own installation, and prints what that is and
how much it frees before removing anything.

Almost all of it is containerd's: compressed image blobs in its content store, and those same
layers unpacked by the snapshotter. One 'boks create' of the base image costs about a
gigabyte, and nothing ever collects it. Uninstalling Boks does not, either — a package
manager owns the files it installed, not the ones a program later wrote — so this is the
command that does.

By default it takes containerd's root and the per-sandbox state, and keeps the four things
you would be upset to lose without being asked: the local certificate authority, credentials
stored with 'boks secret set', the rules added with 'boks policy allow', and the decision log.
--all removes those too and leaves nothing of Boks on this machine.

It destroys sandboxes even without --all. containerd keeps image layers and each sandbox's
filesystem in one root, so there is no way to drop the images and keep the sandboxes. Run
'boks ls' first; 'boks rm' is the command for one sandbox.

Removal is refused while the managed containerd is running or a sandbox's network is up, so
that nothing is deleted from under a process still using it. Stop them, or pass --force.

Nothing outside the state directory is ever touched, and neither is anything inside it that
Boks did not write: the entries removed come from a fixed list of names, not from walking the
directory, and everything else is reported as left alone.`,
		Args: noArgs,
		Example: `  boks purge --dry-run     # what is there, and what it would free
  boks purge               # give the disk back, keep the CA and your rules
  boks purge --all         # leave nothing behind, before uninstalling`,
	}
	var (
		all    bool
		dryRun bool
		yes    bool
		force  bool
	)
	cmd.Flags().BoolVar(&all, "all", false,
		"also remove the CA, stored credentials, policy rules and the decision log")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print what would be removed and stop")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false,
		"do not ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false,
		"remove even though the daemon or a sandbox network is running")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		stateDir := policy.StateDir()
		scope := purge.ScopeReclaim
		if all {
			scope = purge.ScopeAll
		}
		plan, err := purge.Inspect(stateDir, scope)
		if err != nil {
			return err
		}
		// The plan is printed first in every path, including the ones that go on to
		// refuse: a user told "no" should still be told what was there.
		plan.Write(env.Stdout)
		if !plan.Exists || plan.Empty() {
			return nil
		}
		// Asked before the --dry-run exit, so that a dry run tells the user about the
		// refusal they are otherwise going to meet on the next command rather than
		// showing them a plan that will not run.
		blockers := whatIsRunning(stateDir)
		if dryRun {
			if len(blockers) > 0 {
				fmt.Fprintf(env.Stderr, "\nthis would be refused while:\n  - %s\n",
					strings.Join(blockers, "\n  - "))
			}
			fmt.Fprintln(env.Stderr, "\n--dry-run: nothing was removed.")
			return nil
		}
		if len(blockers) > 0 && !force {
			return refuseWhileRunning(blockers)
		}
		if !yes {
			if err := confirmPurge(env, plan); err != nil {
				return err
			}
		}
		res, err := purge.Apply(plan)
		if err != nil {
			// Report what did come off before the failure, so a partial purge is
			// not silent.
			fmt.Fprintf(env.Stderr, "removed %s before failing\n", purge.Bytes(res.Freed))
			return err
		}
		fmt.Fprintf(env.Stdout, "\nremoved %s from %s\n", purge.Bytes(res.Freed), plan.Root)
		// Only the directory actually being gone earns the sentence about there being
		// nothing left — a file Boks did not write keeps it, and `ls` would contradict
		// the claim within seconds.
		if res.RootRemoved {
			fmt.Fprintln(env.Stdout, "Boks has no state on this machine any more. The binary is still installed;\n"+
				"see docs/install.md for removing that.")
		} else if len(plan.Unknown) > 0 {
			fmt.Fprintf(env.Stdout, "%s remains: it holds %d file(s) boks did not write.\n",
				plan.Root, len(plan.Unknown))
		}
		return nil
	}
	return cmd
}

// whatIsRunning names every live process using this state directory, in the form the refusal
// prints them.
//
// Two separate questions, because they have two separate answers. containerd holds its root
// open and serves sandboxes from it. A network supervisor is a boks process per sandbox,
// holding that sandbox's link socket, and one can be up when the managed daemon is not —
// somebody running their own containerd through --containerd-address has exactly that.
func whatIsRunning(stateDir string) []string {
	var found []string
	if st, running := daemon.Lookup(stateDir); running {
		found = append(found, fmt.Sprintf(
			"the boks-managed containerd is running on %s\n"+
				"    'boks ls' lists the sandboxes it is serving; 'boks daemon stop' stops it", st.Address))
	}
	if live := enforce.List(stateDir); len(live) > 0 {
		names := make([]string, 0, len(live))
		for _, st := range live {
			names = append(names, st.Sandbox)
		}
		found = append(found, fmt.Sprintf(
			"a sandbox network is up for %s\n"+
				"    'boks stop %s' stops it, 'boks rm' removes the sandbox as well",
			strings.Join(names, ", "), names[0]))
	}
	return found
}

// refuseWhileRunning is the error a purge takes rather than delete files out from under a
// process that is still using them.
func refuseWhileRunning(blockers []string) error {
	return fmt.Errorf("not purging while something is using this state:\n  - %s\n\n"+
		"Stop them first, or pass --force to remove the files anyway. Forcing it leaves\n"+
		"the running processes pointed at directories that no longer exist.",
		strings.Join(blockers, "\n  - "))
}

// confirmPurge asks, and asks harder when the plan takes something no re-run recreates.
//
// A y/N is the right weight for "you will re-download a gigabyte". It is not the right weight
// for deleting a certificate authority's private key and every credential stored against it,
// where the cost of a mis-keyed answer is not measured in minutes — so that case wants the
// user to have typed a word they could only have meant.
func confirmPurge(env Env, plan purge.Plan) error {
	if plan.Unrecoverable() {
		fmt.Fprintf(env.Stderr,
			"\nThis removes %s from %s, including things nothing recreates:\n", purge.Bytes(plan.Freed()), plan.Root)
		for _, e := range plan.Remove {
			if e.Kind == purge.Identity || e.Kind == purge.Configuration {
				fmt.Fprintf(env.Stderr, "  - %s: %s\n", e.Name, e.What)
			}
		}
		fmt.Fprint(env.Stderr, "Type 'purge' to confirm: ")
		var answer string
		fmt.Fscanln(env.Stdin, &answer)
		if answer != "purge" {
			return errors.New("not purging: the confirmation was not the word 'purge'")
		}
		return nil
	}
	fmt.Fprintf(env.Stderr,
		"\nRemove %s from %s? Every sandbox and every pulled image goes with it. [y/N] ",
		purge.Bytes(plan.Freed()), plan.Root)
	var answer string
	fmt.Fscanln(env.Stdin, &answer)
	if answer != "y" && answer != "Y" && answer != "yes" {
		return errors.New("not purging")
	}
	return nil
}
