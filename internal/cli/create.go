package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/sandbox"
)

func createCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	agents := agent.Builtin()
	flags := registerSandboxFlags(fs)

	// A sandbox's network mode is fixed when it is created, because it lives in
	// annotations the runtime reads at boot — so `create` is exactly where it has to be
	// decidable. Without these flags here, every sandbox made with `create` would come up
	// on the runtime's default transport, which is the one that reaches host loopback.
	var netFlags policyFlags
	netFlags.register(fs)

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

	// Only the wiring, not the stack: nothing starts here, so there is no VM to serve and
	// no socket to hold. `run`, `exec` or `start` brings the network up when the sandbox
	// actually boots.
	mode, err := network.ParseMode(netFlags.mode)
	if err != nil {
		return err
	}
	// The policy selection is recorded on the container alongside the wiring, and for the
	// same reason: `boks start` and `boks exec` have no policy flags, so a sandbox that did
	// not carry its own would come up under the default preset instead of the one it was
	// created with.
	record := netFlags.sandboxRecord()
	spec, err := netFlags.enforceSpec(ctx, inv.name, *flags.address, mode, record)
	if err != nil {
		return err
	}
	if err := describeNetwork(&netFlags, spec, mode, env.Stderr); err != nil {
		return err
	}
	guest, err := spec.Prepare()
	if err != nil {
		return err
	}
	cfg.Policy = record
	cfg.Annotations = withNetworkAnnotations(guest.Annotations, cfg.Annotations)
	cfg.Env = append(cfg.Env, guest.Env...)
	cfg.Mounts = guest.Mounts

	info, err := sandbox.Create(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, info.Name)
	return nil
}
