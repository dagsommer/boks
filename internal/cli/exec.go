package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/sandbox"
)

func execCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks exec", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	interactive := fs.Bool("i", false, "keep stdin open")
	tty := fs.Bool("t", false, "allocate a pseudo-terminal")
	// -it is how everyone types it, and the flag package would otherwise read it as a
	// flag named "it" and fail with a message about an unknown flag.
	both := fs.Bool("it", false, "shorthand for -i -t")
	address := addressFlag(fs)
	var envVars stringList
	fs.Var(&envVars, "env", "extra environment variable KEY=VALUE (repeatable)")

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks exec [flags] <name> <command> [args...]

Runs a command inside a running sandbox, alongside whatever is already in it. The command
inherits the sandbox's environment and working directory, and its exit code becomes boks'.

Flags must come before the sandbox name; everything after the name belongs to the guest.

Examples:
  boks exec web ls -l
  boks exec -it web sh
  boks exec web -- git status

Flags:
`)
		fs.PrintDefaults()
	}

	name, command, err := parseLeadingFlags(fs, env.Args)
	if err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}
	if name == "" {
		fs.Usage()
		return fmt.Errorf("a sandbox name is required; run 'boks ls' to see what is running")
	}
	if len(command) == 0 {
		return fmt.Errorf("a command is required; for example 'boks exec %s sh'", name)
	}

	if *both {
		*interactive, *tty = true, true
	}

	cfg := sandbox.ExecConfig{
		Address: *address,
		Name:    name,
		Command: command,
		Env:     envVars,
		TTY:     *tty,
		Stdout:  env.Stdout,
		Stderr:  env.Stderr,
	}
	// Without -i the guest gets no stdin at all, so a command that reads it sees EOF
	// immediately instead of holding the terminal open.
	if *interactive || *tty {
		cfg.Stdin = env.Stdin
	}

	code, err := sandbox.Exec(ctx, cfg)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
