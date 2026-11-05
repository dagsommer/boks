package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/secret"
)

// resetOps is what `boks reset` does to the machine, as a seam: the command's decisions —
// what to ask, what to refuse, what order to do things in — are the part worth testing, and
// they cannot be tested through a containerd.
type resetOps struct {
	list         func(ctx context.Context, address string) ([]sandbox.Info, error)
	remove       func(ctx context.Context, address, name string, force bool) error
	stopNetwork  func(stateDir, name string) error
	forget       func(stateDir, name string) error
	networks     func(stateDir string) []string
	wipeSecrets  func(ctx context.Context) ([]string, error)
	isTerminalIn func(env Env) bool
}

var defaultResetOps = resetOps{
	list:        sandbox.List,
	remove:      sandbox.Remove,
	stopNetwork: enforce.Stop,
	forget:      enforce.Forget,
	networks: func(stateDir string) []string {
		var names []string
		for _, st := range enforce.List(stateDir) {
			names = append(names, st.Sandbox)
		}
		return names
	},
	wipeSecrets:  wipeStoredSecrets,
	isTerminalIn: func(env Env) bool { return isTerminal(env.Stdin) },
}

func newResetCommand(env Env, dev *devFlags) *cobra.Command {
	return newResetCommandWith(env, dev, defaultResetOps)
}

func newResetCommandWith(env Env, dev *devFlags, ops resetOps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset [flags]",
		Short: "Stop and remove every sandbox, its network and the stored credentials",
		Long: `Returns Boks to a clean slate: every sandbox is stopped and deleted, every sandbox
network and its per-sandbox state is removed, and the credentials stored with
'boks secret set' are deleted.

It keeps the certificate authority, the policy rules and the decision log, and it leaves the
pulled images and the daemon alone. 'boks purge' is the command that gives the disk back.

Credentials are the one thing here nothing recreates, so --preserve-secrets keeps them.

It asks first, on a terminal. Without one it refuses rather than guess, and --force is the
answer that skips the question for scripts.`,
		Args: noArgs,
		Example: `  boks reset                     # asks, then removes everything above
  boks reset --preserve-secrets  # leaves the stored credentials alone
  boks reset --force             # no prompt, for scripts`,
	}
	var force, preserve bool
	cmd.Flags().BoolVarP(&force, "force", "f", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&preserve, "preserve-secrets", false, "keep the stored credentials")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		stateDir := policy.StateDir()

		// Listing needs a containerd and this command must not start one to delete what
		// is in it. No daemon answering means there is no sandbox to find.
		var infos []sandbox.Info
		if dev.addressNamed() || daemonRunning(stateDir) || daemonServing(ctx, dev.address) {
			var err error
			infos, err = ops.list(ctx, dev.address)
			if err != nil {
				return err
			}
		}
		orphans := ops.networks(stateDir)

		fmt.Fprintf(env.Stderr, "This will stop and delete %d sandbox(es)", len(infos))
		if len(orphans) > 0 {
			fmt.Fprintf(env.Stderr, ", tear down %d sandbox network(s)", len(orphans))
		}
		if preserve {
			fmt.Fprintln(env.Stderr, ", and keep the stored credentials.")
		} else {
			fmt.Fprintln(env.Stderr, ", and delete every stored credential.")
		}
		for _, info := range infos {
			fmt.Fprintf(env.Stderr, "  - %s\n", info.Name)
		}

		if !force {
			if !ops.isTerminalIn(env) {
				return errors.New("not resetting: there is no terminal to ask on; pass --force to reset without confirmation")
			}
			fmt.Fprint(env.Stderr, "Continue? [y/N] ")
			var answer string
			fmt.Fscanln(env.Stdin, &answer)
			if answer != "y" && answer != "Y" && answer != "yes" {
				return errors.New("not resetting")
			}
		}

		failed := false
		report := func(err error) {
			fmt.Fprintf(env.Stderr, "boks: %v\n", err)
			failed = true
		}
		done := map[string]bool{}
		for _, info := range infos {
			// Forced: the point is a clean slate, and a running sandbox is not a reason
			// to leave one behind.
			if err := ops.remove(ctx, dev.address, info.Name, true); err != nil {
				report(err)
				continue
			}
			done[info.Name] = true
			if err := ops.stopNetwork(stateDir, info.Name); err != nil {
				report(err)
				continue
			}
			if err := ops.forget(stateDir, info.Name); err != nil {
				report(err)
				continue
			}
			fmt.Fprintf(env.Stdout, "removed %s\n", info.Name)
		}
		// A network whose sandbox is already gone — one a crashed `run` left behind.
		for _, name := range orphans {
			if done[name] {
				continue
			}
			if err := ops.stopNetwork(stateDir, name); err != nil {
				report(err)
				continue
			}
			if err := ops.forget(stateDir, name); err != nil {
				report(err)
			}
		}

		if !preserve {
			removed, err := ops.wipeSecrets(ctx)
			if err != nil {
				report(fmt.Errorf("removing stored credentials: %w", err))
			} else if len(removed) > 0 {
				fmt.Fprintln(env.Stdout, "removed the stored credentials")
			}
		}
		if failed {
			return &ExitError{Code: 1}
		}
		return nil
	}
	return cmd
}

// wipeStoredSecrets deletes every credential Boks holds, from whichever store it holds them
// in, without needing the file store's passphrase.
//
// The encrypted file is removed as a file, the way 'boks secret reset' does, since a lost
// passphrase must not stand between a user and a clean slate. The OS keyring is emptied name
// by name from the index.
func wipeStoredSecrets(ctx context.Context) ([]string, error) {
	stateDir := policy.StateDir()
	var removed []string
	path := secret.DefaultPath(stateDir)
	if err := os.Remove(path); err == nil {
		removed = append(removed, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return removed, err
	}
	if os.Getenv(secret.DisableKeyringEnv) != "" {
		return removed, nil
	}
	ring, err := secret.OpenKeyring(ctx)
	if err != nil {
		// No keyring on this host: the file was the only store this command can reach.
		if errors.Is(err, secret.ErrNoKeyring) {
			return removed, nil
		}
		return removed, err
	}
	store := secret.NewKeyringStore(ring, secret.DefaultIndexPath(stateDir))
	names, err := store.Names()
	if err != nil {
		return removed, err
	}
	for _, name := range names {
		if err := store.Delete(name); err != nil {
			return removed, err
		}
		removed = append(removed, name)
	}
	return removed, nil
}
