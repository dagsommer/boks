package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/sandbox"
)

// bundleLong explains the one thing clone mode makes harder, and why the answer is a file
// rather than a daemon.
const bundleLong = `Writes the commits from a clone-mode sandbox to a git bundle on the host, then prints the
'git fetch' that reads them into your repository.

In clone mode the guest works on its own clone, so its commits are inside the VM and nothing
on the host has seen them. Docker Sandboxes solves this by serving a git daemon from the
sandbox and fetching over the network. Boks does not: a sandbox has no inbound network, and
opening one so that work can leave would be a hole cut through the boundary the mode exists
to provide. A bundle is a single file that 'git fetch' reads exactly like a remote, and it
travels out over the same channel 'boks cp' uses, which needs no listener at all.

A bundle carries commits. Whatever is uncommitted inside the sandbox is not in it, and this
command says so rather than leaving it to be discovered later.

The printed 'git fetch' writes the sandbox's branches under refs/sandboxes/<sandbox>/, so it
cannot move any branch of yours.`

func newBundleCommand(env Env, dev *devFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle [flags] SANDBOX",
		Short: "Write a clone-mode sandbox's commits to a git bundle on the host",
		Long:  bundleLong,
		Example: `  boks bundle claude-myrepo
  boks bundle claude-myrepo -o /tmp/work.bundle`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usagef("a single sandbox name is required; run 'boks ls' to see what exists")
			}
			return nil
		},
	}
	var output string
	cmd.Flags().StringVarP(&output, "output", "o", "",
		"where to write the bundle (default: ./<sandbox>.bundle)")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		name := args[0]
		info, err := sandbox.Inspect(cmd.Context(), dev.address, name)
		if err != nil {
			return err
		}
		if !info.Filesystem.IsClone() {
			return fmt.Errorf(
				"sandbox %q is in %s mode, so there is nothing to bundle: the guest works on\n"+
					"%s directly and its commits are already in that repository.",
				name, info.Filesystem.Mode, info.Workspace())
		}

		if output == "" {
			output = name + ".bundle"
		}
		hostPath, err := filepath.Abs(output)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", output, err)
		}

		guestPath := "/tmp/boks-bundle-" + name
		if err := writeGuestBundle(cmd.Context(), dev.address, name, info.Filesystem.Clone, guestPath, env); err != nil {
			return err
		}
		defer removeGuestFile(cmd.Context(), dev.address, name, guestPath)

		if err := sandbox.Copy(cmd.Context(), sandbox.CopyConfig{
			Address:   dev.address,
			Name:      name,
			GuestPath: guestPath,
			HostPath:  hostPath,
		}); err != nil {
			return err
		}

		fmt.Fprintln(env.Stdout, hostPath)
		fmt.Fprintf(env.Stderr,
			"\nRead it into your repository with:\n"+
				"  git fetch %s 'refs/heads/*:refs/sandboxes/%s/*'\n"+
				"  git log --oneline --all --glob='refs/sandboxes/%s/*'\n",
			hostPath, name, name)
		return nil
	}
	return cmd
}

// bundleScript builds the bundle inside the guest.
//
// `--all HEAD` rather than `--all`: a sandbox whose clone is on a detached HEAD has commits
// no branch points at, and `--all` would leave them behind — which is precisely the quiet
// data loss this command exists to avoid. The dirty check runs first so its warning is not
// buried under git's progress output.
const bundleScript = `set -e
boks_clone=$0
boks_out=$1
cd "$boks_clone"
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
	echo "boks: this sandbox has uncommitted changes, and a bundle carries commits only." >&2
	echo "      Commit them inside the sandbox, or copy the files out with 'boks cp'." >&2
	git status --short >&2
fi
rm -f "$boks_out"
git bundle create "$boks_out" --all HEAD >&2
`

func writeGuestBundle(ctx context.Context, address, name, clonePath, guestPath string, env Env) error {
	var stderr bytes.Buffer
	code, err := sandbox.Exec(ctx, sandbox.ExecConfig{
		Address: address,
		Name:    name,
		Command: []string{"/bin/sh", "-c", bundleScript, clonePath, guestPath},
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	if err != nil {
		return err
	}
	// The guest's own words are the useful part here: "Refusing to create empty bundle"
	// for a clone with no commits, and the working-tree warning for one with too many.
	fmt.Fprint(env.Stderr, stderr.String())
	if code != 0 {
		return fmt.Errorf("building a bundle in sandbox %q failed (exit %d); see the message above", name, code)
	}
	return nil
}

// removeGuestFile deletes the temporary bundle. A failure is not worth reporting over the
// bundle the user actually asked for: the file is in the guest's /tmp and goes away with the
// sandbox.
func removeGuestFile(ctx context.Context, address, name, guestPath string) {
	_, _ = sandbox.Exec(ctx, sandbox.ExecConfig{
		Address: address,
		Name:    name,
		Command: []string{"/bin/sh", "-c", `rm -f "$1"`, "sh", guestPath},
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
}

// bundleHint is the line clone mode prints when a sandbox is created, so that "how do I get
// my work out" is answered where the question arises rather than only in the documentation.
func bundleHint(name string) string {
	return fmt.Sprintf(
		"note: this sandbox works on its own clone, and nothing it writes reaches your disk.\n"+
			"      Get its commits out with 'boks bundle %s'.\n", name)
}
