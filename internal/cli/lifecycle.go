package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/sandbox"
)

func startCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks start", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	address := addressFlag(fs)

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks start [flags] <name>...

Brings stopped sandboxes up, with the filesystem they had when they were stopped. Starting
a sandbox that is already running does nothing.

Flags:
`)
		fs.PrintDefaults()
	}

	names, err := sandboxNames(fs, env, "start")
	if err != nil {
		return err
	}
	return eachSandbox(names, env, func(name string) error {
		return sandbox.Start(ctx, *address, name)
	})
}

func stopCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks stop", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	address := addressFlag(fs)

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks stop [flags] <name>...

Shuts sandboxes down without destroying them. Anything written inside a sandbox is still
there when it starts again; 'boks rm' is what deletes it.

Flags:
`)
		fs.PrintDefaults()
	}

	names, err := sandboxNames(fs, env, "stop")
	if err != nil {
		return err
	}
	return eachSandbox(names, env, func(name string) error {
		return sandbox.Stop(ctx, *address, name)
	})
}

func rmCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks rm", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("f", false, "remove a running sandbox, killing whatever is inside it")
	fs.BoolVar(force, "force", false, "alias for -f")
	address := addressFlag(fs)

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks rm [flags] <name>...

Deletes sandboxes and their filesystems. This is not reversible: everything written inside
a sandbox that is not in a shared workspace is gone.

Flags:
`)
		fs.PrintDefaults()
	}

	names, err := sandboxNames(fs, env, "rm")
	if err != nil {
		return err
	}
	return eachSandbox(names, env, func(name string) error {
		return sandbox.Remove(ctx, *address, name, *force)
	})
}

// sandboxNames parses flags and returns the sandbox names a command was given.
func sandboxNames(fs *flag.FlagSet, env Env, verb string) ([]string, error) {
	names, err := parseInterspersed(fs, env.Args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil, flagErrHelp
		}
		return nil, err
	}
	if len(names) == 0 {
		fs.Usage()
		return nil, fmt.Errorf("a sandbox name is required; run 'boks ls' to see what you can %s", verb)
	}
	return names, nil
}

// eachSandbox applies an operation to every named sandbox, printing the ones that worked
// and continuing past the ones that did not.
//
// Stopping early would leave a `boks rm a b c` half-done with no indication of where it got
// to, so every failure is reported and the exit status reflects that something failed.
func eachSandbox(names []string, env Env, op func(name string) error) error {
	failed := false
	for _, name := range names {
		if err := op(name); err != nil {
			fmt.Fprintf(env.Stderr, "boks: %v\n", err)
			failed = true
			continue
		}
		fmt.Fprintln(env.Stdout, name)
	}
	if failed {
		// The individual errors are already on stderr; this only sets the exit code.
		return &ExitError{Code: 1}
	}
	return nil
}
