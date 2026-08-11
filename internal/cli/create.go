package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/sandbox"
)

func createCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	flags := registerSandboxFlags(fs)

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks create [flags] <workspace> [-- command [args...]]

Creates a sandbox without starting it, pulling the image if needed. Use this to get the
slow part out of the way; 'boks start' brings it up and 'boks exec' runs commands in it.

A command given here becomes the sandbox's default: 'boks run' with no command of its own
runs it. Without one, the image's default command is recorded instead.

Examples:
  boks create .
  boks create -name web ~/src/site -- npm run dev

Flags:
`)
		fs.PrintDefaults()
	}

	args, command := splitAtDoubleDash(env.Args)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}

	if len(positional) < 1 {
		fs.Usage()
		return fmt.Errorf("a workspace directory is required")
	}
	if len(positional) > 1 {
		return fmt.Errorf("unexpected argument %q; put the sandbox's command after '--'", positional[1])
	}

	workspaces, err := flags.workspaces(positional[0])
	if err != nil {
		return err
	}
	if err := flags.requireIsolation(env.Stderr); err != nil {
		return err
	}
	name, err := flags.sandboxName(workspaces[0])
	if err != nil {
		return err
	}

	cfg, err := flags.config(name, workspaces)
	if err != nil {
		return err
	}
	cfg.Command = command
	cfg.Stderr = env.Stderr

	info, err := sandbox.Create(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, info.Name)
	return nil
}
