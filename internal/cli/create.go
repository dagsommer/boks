package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/sandbox"
)

func createCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	agents := agent.Builtin()
	flags := registerSandboxFlags(fs)

	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `Usage: boks create [flags] [agent] [workspace...] [-- agent args...]

Creates a sandbox without starting it, pulling the image if needed. Use this to get the slow
part out of the way; 'boks run' brings it up and attaches, and 'boks exec' runs commands in
it.

The arguments are the same as 'boks run': the agent first, then the workspaces, which
default to the current directory. Anything after '--' is recorded as the agent's arguments,
and is what 'boks run' executes when it is given none of its own.

Agents:
%s
Examples:
  boks create shell .
  boks create -name web shell ~/src/site -- npm run dev

Flags:
`, agentList(agents))
		fs.PrintDefaults()
	}

	args, agentArgs := splitAtDoubleDash(env.Args)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}
	if err := flags.requireIsolation(env.Stderr); err != nil {
		return err
	}

	inv, err := flags.resolve(ctx, agents, positional, env)
	if err != nil {
		return err
	}
	if inv.exists {
		return fmt.Errorf("a sandbox named %q already exists; use 'boks run' to attach to it, "+
			"or 'boks rm %s' first", inv.name, inv.name)
	}

	cfg, err := flags.config(inv, agentArgs)
	if err != nil {
		return err
	}
	cfg.Stderr = env.Stderr

	info, err := sandbox.Create(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, info.Name)
	return nil
}
