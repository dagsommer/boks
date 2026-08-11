// Package cli implements the boks command line.
//
// Dispatch is hand-written on top of the standard flag package: Boks has few commands, and
// the argument grammar for `run` — a workspace, then flags, then `--`, then an arbitrary
// guest command — is clearer to handle directly than to bend a framework around.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Version is the build version, overridden at link time.
var Version = "dev"

// ExitError carries a specific process exit status, used to propagate the guest command's
// exit code unchanged.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Env holds the streams and arguments a command runs against, so commands stay testable.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env Env) error
}

func commands() []command {
	return []command{
		{"run", "Run a command inside a sandbox", runCommand},
		{"doctor", "Check host prerequisites for running sandboxes", doctorCommand},
		{"version", "Print the boks version", versionCommand},
	}
}

// Main runs the CLI and returns the process exit code.
func Main(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		usage(env.Stderr)
		return 2
	}

	name := env.Args[0]
	switch name {
	case "-h", "--help", "help":
		usage(env.Stdout)
		return 0
	case "-v", "--version":
		name = "version"
		env.Args = []string{"version"}
	}

	for _, cmd := range commands() {
		if cmd.name != name {
			continue
		}
		err := cmd.run(ctx, Env{
			Args:   env.Args[1:],
			Stdin:  env.Stdin,
			Stdout: env.Stdout,
			Stderr: env.Stderr,
		})
		if err == nil {
			return 0
		}
		var exit *ExitError
		if errors.As(err, &exit) {
			return exit.Code
		}
		if errors.Is(err, flagErrHelp) {
			return 0
		}
		fmt.Fprintf(env.Stderr, "boks: %v\n", err)
		return 1
	}

	fmt.Fprintf(env.Stderr, "boks: unknown command %q\n\n", name)
	usage(env.Stderr)
	return 2
}

// flagErrHelp marks a -h request, which is not an error for the user.
var flagErrHelp = errors.New("flag: help requested")

func usage(w io.Writer) {
	fmt.Fprint(w, `boks — run untrusted developer tooling in isolated microVMs

Usage:
  boks <command> [flags]

Commands:
`)
	for _, cmd := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprint(w, `
Run 'boks <command> -h' for details.

Boks is experimental. See docs/security-model.md before trusting it with anything.
`)
}

func versionCommand(ctx context.Context, env Env) error {
	fmt.Fprintf(env.Stdout, "boks %s\n", Version)
	return nil
}
