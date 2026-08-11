package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/sandbox"
)

func runCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	agents := agent.Builtin()
	flags := registerSandboxFlags(fs)
	detached := fs.Bool("detached", false, "print the sandbox name and exit instead of attaching")
	fs.BoolVar(detached, "d", false, "alias for -detached")
	ephemeral := fs.Bool("rm", false, "destroy the sandbox when the command exits")

	// Network policy flags are accepted and validated here, but not applied: see
	// netflags.go and the warning below. Their definitions live in netflags.go so that
	// `run`, `proxy` and `policy ls` cannot drift apart.
	var netFlags policyFlags
	netFlags.register(fs)

	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `Usage: boks run [flags] [agent] [workspace...] [-- agent args...]

Runs an agent inside an isolated microVM. The agent comes first and decides what the
sandbox contains; the workspaces follow and default to the current directory. Each
workspace is shared into the guest at the same absolute path it has on the host, and the
first one is the process's working directory. Nothing above them is exposed.

The sandbox is named <agent>-<workspace directory> and persists. Running the same agent in
the same directory re-attaches to it, so packages installed and files written inside it are
still there; remove it with 'boks rm'. Pass -rm for a sandbox destroyed when the command
exits, or -name to reach a sandbox from anywhere.

Arguments after '--' are passed to the agent. For the shell agent they are the command to
run, since that is what arguments to a shell are.

Agents:
%s
Examples:
  boks run                              # a shell in the current directory
  boks run shell . -- uname -a
  boks run shell ~/src/foo ~/src/lib:ro
  boks run -name claude-boks            # re-attach by name, from anywhere

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
	if *detached && *ephemeral {
		// -rm destroys the sandbox when the command exits, and -d means nobody is
		// waiting for it to. Together they would leave a sandbox nothing removes.
		return fmt.Errorf("-d and -rm cannot be combined: an ephemeral sandbox is removed when " +
			"its command exits, and a detached run has no command to wait for")
	}

	// Fail on a malformed rule now, before anything is created, so a policy that will one
	// day be enforced is known to be well-formed today; then say plainly that nothing
	// applies it yet.
	if _, err := netFlags.resolve(); err != nil {
		return err
	}
	if _, err := netFlags.credentialRules(); err != nil {
		return err
	}
	if _, err := netFlags.networkPlan(sandboxNameFor(*flags.name)); err != nil {
		return err
	}
	if netFlags.specified() {
		fmt.Fprintf(env.Stderr, "%s\n", notEnforcedWarning)
	}

	inv, err := flags.resolve(ctx, agents, positional, env)
	if err != nil {
		return err
	}
	if *ephemeral && *flags.name == "" {
		// An ephemeral sandbox must not collide with the persistent one for this
		// workspace, nor with another ephemeral run of it.
		if inv.name, err = sandbox.EphemeralName(inv.agent.Name, inv.workspaces[0].HostPath); err != nil {
			return err
		}
		inv.exists = false
	}

	cfg, err := flags.config(inv, agentArgs)
	if err != nil {
		return err
	}
	cfg.Ephemeral = *ephemeral
	cfg.Stdin = env.Stdin
	cfg.Stdout = env.Stdout
	cfg.Stderr = env.Stderr
	// An interactive session needs a pseudo-terminal, and a piped one must not have
	// one: with a pty the guest's output would come back with carriage returns and no
	// distinct stderr. The host terminal is the only thing that can decide this, so it
	// does, rather than a flag the user has to remember.
	cfg.TTY = !*detached && isTerminal(env.Stdin) && isTerminal(env.Stdout)

	if *detached {
		info, err := sandbox.Up(ctx, cfg)
		if err != nil {
			return err
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

// agentList renders the registry for help output.
func agentList(agents *agent.Registry) string {
	var b strings.Builder
	for _, a := range agents.All() {
		note := a.Summary
		if !a.Runnable() {
			note += " (no image yet — needs -template)"
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
