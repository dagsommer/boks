// Package cli implements the boks command line.
//
// Dispatch is hand-written on top of the standard flag package: Boks has few commands, and
// the argument grammar for `run` — an agent, then workspaces, with flags anywhere among
// them and the agent's own arguments after `--` — is clearer to handle directly than to
// bend a framework around.
//
// Flag names, their short aliases and the argument order follow sbx. Boks is meant to feel
// like a drop-in alternative, and a user's muscle memory is part of that interface: `-t` is
// sbx's template flag here for the same reason `ls` also answers to `list`.
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
	name string
	// alias is a second spelling sbx accepts, so a habit formed there works here.
	alias   string
	summary string
	run     func(ctx context.Context, env Env) error
}

func (c command) matches(name string) bool {
	return name == c.name || (c.alias != "" && name == c.alias)
}

func commands() []command {
	return []command{
		{name: "run", summary: "Run an agent in a sandbox, creating or re-attaching to it", run: runCommand},
		{name: "exec", summary: "Run an additional command inside a sandbox", run: execCommand},
		{name: "create", summary: "Create a sandbox without starting it", run: createCommand},
		{name: "ls", alias: "list", summary: "List sandboxes", run: lsCommand},
		{name: "inspect", summary: "Print sandbox details as JSON", run: inspectCommand},
		{name: "start", summary: "Start a stopped sandbox", run: startCommand},
		{name: "stop", summary: "Stop a sandbox without deleting it", run: stopCommand},
		{name: "rm", summary: "Delete a sandbox and its filesystem", run: rmCommand},
		{name: "cp", summary: "Copy files between the host and a sandbox", run: cpCommand},
		{name: "policy", summary: "Show network policy rules and recent decisions", run: policyCommand},
		{name: "proxy", summary: "Run the host forward proxy (experimental, not wired into run)", run: proxyCommand},
		{name: "secret", summary: "Manage host-side credentials the guest never receives", run: secretCommand},
		{name: "doctor", summary: "Check host prerequisites for running sandboxes", run: doctorCommand},
		{name: "version", summary: "Print the boks version", run: versionCommand},
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
		if !cmd.matches(name) {
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
  boks run [agent] [workspace...]     the common case: an agent in a directory

Commands:
`)
	for _, cmd := range commands() {
		name := cmd.name
		if cmd.alias != "" {
			name += ", " + cmd.alias
		}
		fmt.Fprintf(w, "  %-10s %s\n", name, cmd.summary)
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
