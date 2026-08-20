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

By default the guest writes straight to those directories. --clone changes that: the
workspace must be a git repository, it is shared read-only at /run/sandbox/source, and the
agent works on a clone made inside the guest, so nothing it writes reaches your disk. The
clone carries committed history only, the mode is fixed when the sandbox is created, and
'boks bundle' is how commits come back out.

The sandbox is named <agent>-<workspace directory> and persists. Running the same agent in
the same directory re-attaches to it, so packages installed and files written inside it are
still there; remove it with 'boks rm'. Pass --rm for a sandbox destroyed when the command
exits, or --name to reach a sandbox from anywhere.

What a sandbox is made of is fixed when it is created — the agent, the image, the vCPUs, the
memory, the environment and the network mode all live in the container the runtime builds the
VM from. Passing one of those to a sandbox that already exists is refused rather than quietly
dropped, and the refusal names the value the sandbox has. Remove it, or name a new one.

Arguments after '--' are passed to the agent. For the shell agent they are the command to
run, since that is what arguments to a shell are.

An interactive run clears the terminal first, so the agent's own interface has the window
rather than drawing over your shell history. Set BOKS_NO_CLEAR to keep the screen as it is.

Agents:
%s`, agentList(agents)),
		Example: `  boks run                              # a shell in the current directory
  boks run shell . -- uname -a
  boks run shell ~/src/foo ~/src/lib:ro
  boks run --clone claude ~/src/foo     # the agent works on a clone; your files are read-only
  boks run --name claude-boks           # re-attach by name, from anywhere`,
		Args: cobra.ArbitraryArgs,
	}

	flags := registerSandboxFlags(cmd.Flags(), dev)
	var (
		detached  bool
		ephemeral bool
		quiet     bool
		verbose   bool
	)
	cmd.Flags().BoolVarP(&detached, "detached", "d", false,
		"print the sandbox name and exit instead of attaching")
	cmd.Flags().BoolVar(&ephemeral, "rm", false, "destroy the sandbox when the command exits")
	// -v adds detail; it does not gate anything a user needs to be told. The two things
	// that are news rather than chatter — a host whose TLS boks is about to terminate for
	// the first time, and a policy that has changed since this sandbox last ran — are
	// printed at every level. Asking for less output is not consent to being decrypted
	// silently, nor to a rule set changing without a word. See internal/cli/notice.go.
	//
	// This used to be --quiet, defaulting to loud. It was the wrong default: the summary
	// is identical on every run of an unchanged sandbox, so the routine case paid for the
	// exceptional one and people learned to skip past the network lines — which is exactly
	// how a NEW interception host goes unread.
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false,
		"describe what is happening: the image, the kit, the command, and the network summary")
	// Kept so that a script written against 0.1.5 keeps working. It is now the default,
	// so it does nothing; hidden because advertising two spellings of one idea is worse
	// than a flag that quietly still parses.
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "")
	_ = cmd.Flags().MarkHidden("quiet")
	_ = cmd.Flags().MarkDeprecated("quiet", "output is quiet by default; use -v for detail")

	// The network policy flags decide what the sandbox's network is and what may cross
	// it. Their definitions live in netflags.go so that `run`, `proxy` and `policy ls`
	// cannot drift apart.
	var netFlags policyFlags
	netFlags.register(cmd.Flags())
	netFlags.registerPublish(cmd.Flags())

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		positional, agentArgs := splitAtDash(cmd, args)

		// Early, so the background refresh has the whole of a VM start to finish in.
		// It never blocks and never fails; see internal/cli/update.go.
		noticeUpdate(env.Stderr)

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
		// Before the daemon is touched and before anything is created: a kit that does
		// not parse should cost nothing, and its rules decide the policy the sandbox is
		// built with.
		if err := netFlags.loadKit(env.Stderr); err != nil {
			return err
		}
		// A `kind: sandbox` kit defines an agent, so it has to be in the registry before
		// the positional arguments are read — `boks run udi-copilot-default --kit …`
		// names an agent that exists only in the file on the same command line, and
		// splitAgent decides what the first positional IS by asking the registry.
		//
		// The TTY test is made here rather than inside, because it is the same one
		// cfg.TTY makes below and the two must agree: a kit whose command.interactive
		// was chosen for a terminal that then is not allocated would run the wrong argv.
		if err := registerKitAgent(agents, netFlags.kitSpec,
			isTerminal(env.Stdin) && isTerminal(env.Stdout)); err != nil {
			return err
		}

		ctx := cmd.Context()
		// Before the first thing that talks to containerd, and after everything that can
		// be decided without one: a mistyped flag should not start a daemon.
		if err := ensureDaemon(ctx, dev, env.Stderr); err != nil {
			return err
		}
		inv, err := flags.resolve(ctx, agents, positional, env)
		if err != nil {
			return err
		}
		// The agent decides a layer of the policy — the destinations its own
		// definition says it cannot work without — so the policy flags have to know
		// which agent this is before they resolve anything.
		netFlags.forAgent(inv.agent)
		if ephemeral && flags.name == "" {
			// An ephemeral sandbox must not collide with the persistent one for
			// this workspace, nor with another ephemeral run of it.
			if inv.name, err = sandbox.EphemeralName(inv.agent.Name, inv.workspaces[0].HostPath); err != nil {
				return err
			}
			inv.exists = false
		}
		// Everything a sandbox fixes when it is created is decided here, before anything
		// is pulled, created or started: a flag that cannot be honoured costs a message
		// rather than a sandbox that is not the one the command line described. An
		// ephemeral run has just been given a name of its own, so it creates and this
		// says nothing.
		if err := checkFixedAtCreation(flags, &netFlags, inv); err != nil {
			return err
		}

		cfg, err := flags.config(inv, agentArgs)
		if err != nil {
			return err
		}
		// Decided before anything is pulled or started, so that a --clone against a
		// directory Boks will not clone costs a message rather than a sandbox.
		if err := applyCloneMode(flags, inv, &cfg, env); err != nil {
			return err
		}

		// The network is decided, described and started before the sandbox: its
		// annotations have to be on the container when it is created, and the host-side
		// stack has to be holding the link socket before the VM boots and connects to it.
		started, err := attachSandboxNetwork(ctx, &netFlags, inv, &cfg, requestedMode, verbose, env)
		if err != nil {
			return err
		}
		if started {
			defer func() {
				// An ephemeral sandbox leaves nothing behind, its network
				// included, and a stack whose sandbox never came up must not be
				// left holding a socket for a VM that will never connect. A
				// sandbox that is running keeps its stack, however this command
				// ended — that the stack outlives this process is the whole
				// point of it being a process. See releaseStack for the run that
				// proved this had to be decided by the sandbox's state rather
				// than by this command's error.
				releaseStack(inv.name, ephemeral, func() bool {
					return sandboxIsRunning(ctx, cfg.Address, inv.name, env.Stderr)
				}, env.Stderr)
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
		// The image pull reports itself WITHOUT -v. It is the longest thing `boks run`
		// does — hundreds of megabytes on a cold machine — and a silent wait is
		// indistinguishable from a hang, which is how it was reported: a blank terminal
		// with "nothing happening". Detail is optional; a several-minute pause is not.
		cfg.Progress = env.Stderr
		if verbose {
			// Before the image, because it is the fastest thing to establish and the
			// order reads as a sequence: what will run, out of what, with which rules.
			fmt.Fprintf(env.Stderr, "sandbox: %s (agent %s, %s)\n",
				inv.name, inv.agent.Name, cfg.Image)
			if len(cfg.Command) > 0 {
				fmt.Fprintf(env.Stderr, "command: %s\n", strings.Join(cfg.Command, " "))
			}
			netFlags.describeKit(env.Stderr)
			for _, ws := range cfg.Workspaces {
				fmt.Fprintf(env.Stderr, "workspace: %s → %s (%s)\n",
					ws.HostPath, ws.GuestPath, ws.Mode)
			}
		}
		// An interactive session needs a pseudo-terminal, and a piped one must not have
		// one: with a pty the guest's output would come back with carriage returns and
		// no distinct stderr. The host terminal is the only thing that can decide this,
		// so it does, rather than a flag the user has to remember.
		cfg.TTY = !detached && isTerminal(env.Stdin) && isTerminal(env.Stdout)

		if detached {
			info, err := sandbox.Up(ctx, cfg)
			if err != nil {
				return err
			}
			fmt.Fprintln(env.Stdout, info.Name)
			return nil
		}

		// Hand the agent a clean window — but only once there IS an agent to hand it to.
		//
		// This has been wrong twice. Clearing before the network summary hid the image
		// pull, so a long download looked like a hang. Clearing before sandbox.Run hid
		// the task's own failure, so an image whose layers could not be mounted was
		// reported as a terminal that went blank and said nothing. Both were the same
		// mistake: clearing at a point where the run could still fail, and taking the
		// evidence with it.
		//
		// OnGuestReady fires after the guest's process has started and before its
		// terminal is attached. Nothing can fail between there and the agent drawing, so
		// nothing can be lost. A run that never gets that far never clears, and its error
		// stays where the user can read it.
		//
		// The visible screen only, never the scrollback: taking the window is reasonable,
		// taking the history is not.
		if cfg.TTY && os.Getenv("BOKS_NO_CLEAR") == "" {
			cfg.OnGuestReady = func() { fmt.Fprint(env.Stderr, "\033[2J\033[H") }
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
