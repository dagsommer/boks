package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/sandbox"
)

func newCreateCommand(env Env, dev *devFlags) *cobra.Command {
	agents := agent.Builtin()

	cmd := &cobra.Command{
		Use:   "create [flags] [AGENT] [PATH...] [-- AGENT_ARGS...]",
		Short: "Create a sandbox without starting it",
		Long: fmt.Sprintf(`Creates a sandbox without starting it, pulling the image if needed. Use this to get the slow
part out of the way; 'boks run' brings it up and attaches, and 'boks exec' runs commands in
it.

The arguments are the same as 'boks run': the agent first, then the workspaces, which
default to the current directory. Anything after '--' is recorded as the agent's arguments,
and is what 'boks run' executes when it is given none of its own.

--clone belongs here too, and only here: the mode lives in the sandbox's mounts, so it is
fixed when the sandbox is created and cannot be changed afterwards.

Agents:
%s`, agentList(agents)),
		Example: `  boks create shell .
  boks create --name web shell ~/src/site -- npm run dev`,
		Args: cobra.ArbitraryArgs,
	}

	flags := registerSandboxFlags(cmd.Flags(), dev)

	// A sandbox's network mode is fixed when it is created, because it lives in
	// annotations the runtime reads at boot — so `create` is exactly where it has to be
	// decidable. Without these flags here, every sandbox made with `create` would come up
	// on the runtime's default transport, which is the one that reaches host loopback.
	var netFlags policyFlags
	netFlags.register(cmd.Flags())
	netFlags.registerPublish(cmd.Flags())
	var quiet bool
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false,
		"suppress the network summary (a new TLS-interception host is still announced)")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		positional, agentArgs := splitAtDash(cmd, args)
		ctx := cmd.Context()

		if err := dev.requireIsolation(env.Stderr); err != nil {
			return err
		}
		if err := netFlags.checkPublish(); err != nil {
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
		// The agent's own allowlist is part of the policy this sandbox will run under,
		// exactly as it is for `run`.
		netFlags.forAgent(inv.agent)

		cfg, err := flags.config(inv, agentArgs)
		if err != nil {
			return err
		}
		if err := applyCloneMode(flags, inv, &cfg, env); err != nil {
			return err
		}
		cfg.Stderr = env.Stderr

		// Only the wiring, not the stack: nothing starts here, so there is no VM to
		// serve and no socket to hold. `run`, `exec` or `start` brings the network up
		// when the sandbox actually boots.
		mode, err := network.ParseMode(netFlags.mode)
		if err != nil {
			return err
		}
		// The policy selection is recorded on the container alongside the wiring, and
		// for the same reason: `boks start` and `boks exec` have no policy flags, so a
		// sandbox that did not carry its own would come up under the default preset
		// instead of the one it was created with.
		record := netFlags.sandboxRecord()
		// `create` starts nothing, so nothing is bound here: the specifications are
		// recorded on the container and honoured when `run`, `exec` or `start` brings
		// the sandbox up. They are still validated, because a typo should cost a message
		// rather than a sandbox that fails to start tomorrow.
		spec, err := netFlags.enforceSpec(ctx, inv.name, dev.address, mode, record, netFlags.publish, env.Stderr)
		if err != nil {
			return err
		}
		if err := describeNetwork(&netFlags, spec, mode, quiet, env.Stderr); err != nil {
			return err
		}
		guest, err := spec.Prepare()
		if err != nil {
			return err
		}
		cfg.Policy = record
		cfg.Ports = netFlags.publish
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
	return cmd
}
