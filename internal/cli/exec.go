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

	interactive := fs.Bool("interactive", false, "keep stdin open")
	fs.BoolVar(interactive, "i", false, "alias for -interactive")
	tty := fs.Bool("tty", false, "allocate a pseudo-terminal")
	fs.BoolVar(tty, "t", false, "alias for -tty")
	// -it is how everyone types it, and the flag package would otherwise read it as a
	// flag named "it" and fail with a message about an unknown flag.
	both := fs.Bool("it", false, "shorthand for -i -t")
	workdir := fs.String("workdir", "", "working directory inside the sandbox")
	fs.StringVar(workdir, "w", "", "alias for -workdir")
	user := fs.String("user", "", "user to run as, UID or UID:GID")
	fs.StringVar(user, "u", "", "alias for -user")
	address := addressFlag(fs)
	var envVars stringList
	fs.Var(&envVars, "env", "extra environment variable KEY=VALUE (repeatable)")
	fs.Var(&envVars, "e", "alias for -env")

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks exec [flags] <name> <command> [args...]

Runs a command inside a sandbox, alongside whatever is already in it. The command inherits
the sandbox's environment and working directory, and its exit code becomes boks'. A stopped
sandbox is started first.

Flags must come before the sandbox name; everything after the name belongs to the guest.

Examples:
  boks exec web ls -l
  boks exec -it web sh
  boks exec -w /tmp web -- git status

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
		Cwd:     *workdir,
		User:    *user,
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
