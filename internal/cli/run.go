package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/sandbox"
)

func newRunCommand(env Env, dev *devFlags) *cobra.Command {
	agents := agent.Builtin()

	cmd := &cobra.Command{
		Use:   "run [flags] [AGENT] [PATH...] [-- AGENT_ARGS...]",
		Short: "Run an agent in a sandbox, creating or re-attaching to it",
		Long: fmt.Sprintf(`Runs an agent inside an isolated microVM. The agent comes first and decides what the
sandbox contains; the workspaces follow and default to the current directory. Each
workspace is shared into the guest at the same absolute path it has on the host, and the
first one is the process's working directory. Nothing above them is exposed. A workspace
may carry a ':ro' suffix for a read-only share.

The sandbox is named <agent>-<workspace directory> and persists. Running the same agent in
the same directory re-attaches to it, so packages installed and files written inside it are
still there; remove it with 'boks rm'. Pass --rm for a sandbox destroyed when the command
exits, or --name to reach a sandbox from anywhere.

Arguments after '--' are passed to the agent. For the shell agent they are the command to
run, since that is what arguments to a shell are.

Agents:
%s`, agentList(agents)),
		Example: `  boks run                              # a shell in the current directory
  boks run shell . -- uname -a
  boks run shell ~/src/foo ~/src/lib:ro
  boks run --name claude-boks           # re-attach by name, from anywhere`,
		Args: cobra.ArbitraryArgs,
	}

	flags := registerSandboxFlags(cmd.Flags(), dev)
	var (
		detached  bool
		ephemeral bool
	)
	cmd.Flags().BoolVarP(&detached, "detached", "d", false,
		"print the sandbox name and exit instead of attaching")
	cmd.Flags().BoolVar(&ephemeral, "rm", false, "destroy the sandbox when the command exits")

	// The network policy flags decide what the sandbox's network is and what may cross
	// it. Their definitions live in netflags.go so that `run`, `proxy` and `policy ls`
	// cannot drift apart.
	var netFlags policyFlags
	netFlags.register(cmd.Flags())

	cmd.RunE = func(cmd *cobra.Command, args []string) (err error) {
		positional, agentArgs := splitAtDash(cmd, args)

		if err := dev.requireIsolation(env.Stderr); err != nil {
			return err
		}
		if detached && ephemeral {
			// --rm destroys the sandbox when the command exits, and -d means nobody
			// is waiting for it to. Together they would leave a sandbox nothing
			// removes.
			return usagef("-d and --rm cannot be combined: an ephemeral sandbox is removed when " +
				"its command exits, and a detached run has no command to wait for")
		}

		// Fail on a malformed rule before anything is created or pulled, so that a
		// mistyped pattern costs a message rather than a sandbox.
		requestedMode, err := network.ParseMode(netFlags.mode)
		if err != nil {
			return err
		}
		if _, err := netFlags.resolve(); err != nil {
			return err
		}
		if _, err := netFlags.credentialRules(); err != nil {
			return err
		}
		if _, err := netFlags.networkPlan(sandboxNameFor(flags.name)); err != nil {
			return err
		}
		if err := netFlags.checkPublish(); err != nil {
			return err
		}

		ctx := cmd.Context()
		inv, err := flags.resolve(ctx, agents, positional, env)
		if err != nil {
			return err
		}
		if ephemeral && flags.name == "" {
			// An ephemeral sandbox must not collide with the persistent one for
			// this workspace, nor with another ephemeral run of it.
			if inv.name, err = sandbox.EphemeralName(inv.agent.Name, inv.workspaces[0].HostPath); err != nil {
				return err
			}
			inv.exists = false
		}

		cfg, err := flags.config(inv, agentArgs)
		if err != nil {
			return err
		}

		// The network is decided, described and started before the sandbox: its
		// annotations have to be on the container when it is created, and the host-side
		// stack has to be holding the link socket before the VM boots and connects to it.
		started, err := attachSandboxNetwork(ctx, &netFlags, inv, &cfg, requestedMode, env)
		if err != nil {
			return err
		}
		if started {
			defer func() {
				// An ephemeral sandbox leaves nothing behind, its network
				// included. A run that failed must not leave a stack holding a
				// socket for a VM that will never connect. A persistent sandbox
				// that started successfully keeps its stack — that it outlives
				// this process is the whole point.
				if ephemeral || err != nil {
					stopNetworkQuietly(inv.name, env.Stderr)
				}
			}()
		}
		if ephemeral {
			// Deferred after the stack's teardown, so it runs once the socket
			// directory is gone: an ephemeral sandbox also takes with it the copy of
			// the CA that existed only to be shared into it.
			defer forgetNetworkQuietly(inv.name, env.Stderr)
		}
		cfg.Ephemeral = ephemeral
		cfg.Stdin = env.Stdin
		cfg.Stdout = env.Stdout
		cfg.Stderr = env.Stderr
		// An interactive session needs a pseudo-terminal, and a piped one must not have
		// one: with a pty the guest's output would come back with carriage returns and
		// no distinct stderr. The host terminal is the only thing that can decide this,
		// so it does, rather than a flag the user has to remember.
		cfg.TTY = !detached && isTerminal(env.Stdin) && isTerminal(env.Stdout)

		if detached {
			// Assign rather than declare: the deferred network cleanup above reads
			// this function's error, and a shadowed one would make a failed run look
			// successful to it and leave a stack behind.
			info, upErr := sandbox.Up(ctx, cfg)
			if upErr != nil {
				return upErr
			}
			fmt.Fprintln(env.Stdout, info.Name)
			return nil
		}

		code, err := sandbox.Run(ctx, cfg)
		if err != nil {
			return err
		}
		if code != 0 {
			return &ExitError{Code: code}
		}
		return nil
	}
	return cmd
}

// agentList renders the registry for help output.
func agentList(agents *agent.Registry) string {
	var b strings.Builder
	for _, a := range agents.All() {
		note := a.Summary
		if !a.Runnable() {
			note += " (no image yet — needs --template)"
		}
		fmt.Fprintf(&b, "  %-14s %s\n", a.Name, note)
	}
	return b.String()
}

// isTerminal reports whether a stream is the user's terminal. Anything that is not an
// *os.File — a pipe, a buffer in a test — is not.
func isTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
