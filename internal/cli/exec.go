package cli

import (
	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/sandbox"
)

func newExecCommand(env Env, dev *devFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec [flags] SANDBOX COMMAND [ARG...]",
		Short: "Run an additional command inside a sandbox",
		Long: `Runs a command inside a sandbox, alongside whatever is already in it. The command inherits
the sandbox's environment and working directory, and its exit code becomes boks'. A stopped
sandbox is started first.

Flags must come before the sandbox name; everything after the name belongs to the guest, so
'boks exec web ls -l' sends -l to ls rather than to boks.`,
		Example: `  boks exec web ls -l
  boks exec -it web sh
  boks exec -w /tmp web -- git status`,
	}

	var (
		interactive bool
		tty         bool
		workdir     string
		user        string
		envVars     []string
	)
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a pseudo-terminal")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the sandbox")
	cmd.Flags().StringVarP(&user, "user", "u", "", "user to run as, UID or UID:GID")
	cmd.Flags().StringArrayVarP(&envVars, "env", "e", nil,
		"extra environment variable KEY=VALUE (repeatable)")

	// Everything after the sandbox name belongs to the guest, flags included. pflag with
	// interspersed parsing turned off stops at the first positional and hands the rest
	// back untouched, which is exactly this grammar — and it splits `-it` into -i -t on
	// the way, so there is no combined-shorthand special case to write.
	cmd.Flags().SetInterspersed(false)

	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return usagef("a sandbox name is required; run 'boks ls' to see what is running")
		}
		if len(commandFor(args)) == 0 {
			return usagef("a command is required; for example 'boks exec %s sh'", args[0])
		}
		return nil
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		name, command := args[0], commandFor(args)

		// `boks exec` starts a stopped sandbox, so it can be the command that boots the
		// VM — and a VM that boots without something holding its link socket comes up
		// with a NIC connected to nothing. The stack it starts outlives this command, as
		// the sandbox does.
		if err := ensureNetworkForExisting(cmd.Context(), name, dev.address, env.Stderr); err != nil {
			return err
		}

		cfg := sandbox.ExecConfig{
			Address: dev.address,
			Name:    name,
			Command: command,
			Env:     envVars,
			Cwd:     workdir,
			User:    user,
			TTY:     tty,
			Stdout:  env.Stdout,
			Stderr:  env.Stderr,
		}
		// Without -i the guest gets no stdin at all, so a command that reads it sees EOF
		// immediately instead of holding the terminal open.
		if interactive || tty {
			cfg.Stdin = env.Stdin
		}

		code, err := sandbox.Exec(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		if code != 0 {
			return &ExitError{Code: code}
		}
		return nil
	}
	return cmd
}

// commandFor returns the guest command from `exec`'s positionals.
//
// Parsing stops at the sandbox name, so a "--" written after it is still in the arguments
// rather than recorded by pflag. Dropping one leading separator makes both
// `boks exec web -- ls -l` and `boks exec web ls -l` mean the same thing.
func commandFor(args []string) []string {
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return rest
}
