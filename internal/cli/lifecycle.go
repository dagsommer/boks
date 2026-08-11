package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
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
		// A sandbox that is about to boot needs its link socket held before it does, so
		// the network comes up first. `start` has no policy flags of its own, so it
		// serves the sandbox with the default policy and says so — the rules a `boks
		// run` was given are not recorded anywhere, deliberately: there is no host-side
		// state store, and inventing one for this would be the wrong place to start.
		if err := ensureNetworkForExisting(ctx, name, *address, env.Stderr); err != nil {
			return err
		}
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
		if err := sandbox.Stop(ctx, *address, name); err != nil {
			return err
		}
		// The supervisor would notice the task going away and exit by itself within a
		// poll interval. Ending it here makes `stop` mean the same thing for the network
		// as it does for the sandbox — gone when the command returns — so that a stop
		// followed immediately by a start cannot race for the link socket.
		return enforce.Stop(policy.StateDir(), name)
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
		if err := sandbox.Remove(ctx, *address, name, *force); err != nil {
			return err
		}
		// A removed sandbox must leave nothing behind: not its snapshot, not its link
		// socket, and not the process holding it. The certificate directory goes too —
		// it exists only to be shared into that sandbox.
		if err := enforce.Stop(policy.StateDir(), name); err != nil {
			return err
		}
		return enforce.Forget(policy.StateDir(), name)
	})
}

// ensureNetworkForExisting brings up the network of a sandbox that already exists and is
// about to be started by a command with no policy flags of its own — `boks start`, and
// `boks exec` on a stopped sandbox.
//
// The mode is read back from the container: it was fixed when the sandbox was created, and
// a stack that disagrees with the container's wiring is worse than none.
func ensureNetworkForExisting(ctx context.Context, name, address string, stderr io.Writer) error {
	info, exists, err := sandbox.Find(ctx, address, name)
	if err != nil || !exists {
		// A sandbox that is not there is the next command's error to report, with a
		// better message than this one could give.
		return nil
	}
	if _, alive := enforce.Lookup(policy.StateDir(), name); alive {
		return nil
	}
	mode, wired := network.ModeFromAnnotations(info.Annotations)
	if !wired {
		fmt.Fprint(stderr, unwiredSandboxWarning(name))
		return nil
	}

	var flags policyFlags
	spec, err := flags.enforceSpec(ctx, name, address, mode)
	if err != nil {
		return err
	}
	if mode != network.ModeNone {
		fmt.Fprintf(stderr, "network: starting a %s stack for %s with the default policy (%s); "+
			"per-run rules are not recorded, so 'boks run -allow ...' is where they belong.\n",
			mode, name, policy.DefaultPreset)
	}
	_, _, err = attachNetwork(ctx, spec, info.Status == sandbox.StatusRunning, stderr)
	return err
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
