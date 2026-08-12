// Package cli implements the boks command line.
//
// Dispatch is cobra, and flags are pflag. Boks reproduces sbx's observable behaviour, and
// sbx's command line is cobra's: `Usage:` / `Available Commands:` / `Flags:` /
// `Global Flags:`, `-t, --template`, combined shorthands like `-it`, and a `completion`
// command. Matching that is parity in the most literal sense, and it costs less code than
// the hand-written dispatcher it replaced: pflag parses flags on both sides of the
// positionals by itself, splits `-it` by itself, and records where `--` fell by itself.
//
// Flag names, their short forms and the argument order follow sbx. Boks is meant to feel
// like a drop-in alternative, and a user's muscle memory is part of that interface: `-t` is
// sbx's template flag here for the same reason `ls` also answers to `list`. Short forms
// exist only where sbx has one — inventing others would make the muscle memory wrong.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Version is the build version, overridden at link time.
var Version = "dev"

// ExitError carries a specific process exit status, used to propagate the guest command's
// exit code unchanged.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// usageError marks an error the user made in how they invoked boks, as opposed to one the
// command hit while doing its work. It exits 2 and prints the command's usage, which is the
// convention every other tool follows and the one sbx follows.
type usageError struct {
	err error
	// silent suppresses the message line, for the case where the usage text is the
	// whole answer: bare `boks`.
	silent bool
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usagef builds a usage error from a message.
func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// Env holds the streams and arguments a command runs against, so commands stay testable.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Main runs the CLI and returns the process exit code.
func Main(ctx context.Context, env Env) int {
	root := newRootCommand(env)
	// A nil slice would make cobra fall back to os.Args, which is exactly what a test
	// invoking `boks` with no arguments must not get.
	args := env.Args
	if args == nil {
		args = []string{}
	}
	if err := rejectSingleDashLongFlags(root, args); err != nil {
		fmt.Fprintf(env.Stderr, "boks: %v\n", err)
		return 2
	}

	root.SetArgs(args)
	root.SetContext(ctx)

	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	var usage *usageError
	if errors.As(err, &usage) {
		if !usage.silent {
			fmt.Fprintf(env.Stderr, "boks: %v\n\n", err)
		}
		fmt.Fprint(env.Stderr, cmd.UsageString())
		return 2
	}
	fmt.Fprintf(env.Stderr, "boks: %v\n", err)
	return 1
}

const rootLong = `boks — run untrusted developer tooling in isolated microVMs

The common case is 'boks run [agent] [workspace...]': an agent, in a directory, inside a
microVM of its own.

Four flags for developing Boks itself are accepted by every command and hidden from this
help: --runtime, --snapshotter, --containerd-address and --i-know-this-is-not-isolated.
The last one turns off the refusal to present a runtime with no VM boundary as a sandbox,
and must never be used to run anything untrusted.

Boks is experimental. See docs/security-model.md before trusting it with anything.`

// newRootCommand assembles the command tree.
//
// Every command is constructed against this Env rather than reading os.Std*, so a test can
// drive the whole CLI through buffers.
func newRootCommand(env Env) *cobra.Command {
	root := &cobra.Command{
		Use:           "boks",
		Short:         "Run untrusted developer tooling in isolated microVMs",
		Long:          rootLong,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Bare `boks` is not a no-op that exits 0: there is no dashboard yet, so it is
		// a user who has not said what they want. Usage, exit 2.
		RunE: func(cmd *cobra.Command, args []string) error {
			return &usageError{err: errors.New("a command is required"), silent: true}
		},
		// With a RunE above, cobra would otherwise hand any argument to it rather than
		// reporting that it names no command.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
			if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
				msg += fmt.Sprintf("\n\nDid you mean this?\n\t%s", suggestions[0])
			}
			return &usageError{err: errors.New(msg)}
		},
	}
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetVersionTemplate("boks {{.Version}}\n")

	// A malformed flag is a usage error wherever it happens; cobra looks this up through
	// the parents, so registering it once covers the whole tree.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{err: err}
	})

	dev := &devFlags{}
	dev.register(root.PersistentFlags())

	root.AddCommand(
		newRunCommand(env, dev),
		newExecCommand(env, dev),
		newCreateCommand(env, dev),
		newLsCommand(env, dev),
		newInspectCommand(env, dev),
		newStartCommand(env, dev),
		newStopCommand(env, dev),
		newRmCommand(env, dev),
		newCpCommand(env, dev),
		newPolicyCommand(env),
		newNetCommand(env),
		newProxyCommand(env),
		newSecretCommand(env),
		newCaCommand(env),
		newDoctorCommand(env, dev),
		newVersionCommand(env),
	)
	return root
}

func newVersionCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the boks version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(env.Stdout, "boks %s\n", Version)
			return nil
		},
	}
}

// noArgs rejects positional arguments with a message that says what the command does take,
// rather than cobra's default about unknown commands.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unexpected argument %q; %s takes no arguments", args[0], cmd.CommandPath())
	}
	return nil
}

// rejectSingleDashLongFlags turns `-template` into an error naming the mistake.
//
// The stdlib flag package accepted a long name after one dash; pflag reads it as a cluster
// of shorthands. Usually that fails loudly — `-name` reports `unknown shorthand flag: 'n'`.
// But when every letter happens to be a registered shorthand it succeeds and means something
// else entirely: `-template img` sets --template to "emplate" and leaves img as a workspace,
// so the user is told their *workspace* does not exist. The real mistake is one dash, and
// nothing in that message says so.
//
// This only rejects a token whose name matches a long flag exactly, so genuine clusters like
// `-it` are untouched. It is a validation pass, not a parser: cobra still does the parsing.
func rejectSingleDashLongFlags(root *cobra.Command, args []string) error {
	long := map[string]bool{}
	var collect func(*cobra.Command)
	collect = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) { long[f.Name] = true })
		c.PersistentFlags().VisitAll(func(f *pflag.Flag) { long[f.Name] = true })
		for _, sub := range c.Commands() {
			collect(sub)
		}
	}
	collect(root)

	for _, arg := range args {
		// Everything after `--` belongs to the agent, and its flags are not ours to judge.
		if arg == "--" {
			return nil
		}
		if len(arg) < 3 || arg[0] != '-' || arg[1] == '-' {
			continue
		}
		name, _, _ := strings.Cut(arg[1:], "=")
		if long[name] {
			return fmt.Errorf("unknown shorthand flag in %q; long flags take two dashes, so use --%s", arg, name)
		}
	}
	return nil
}
