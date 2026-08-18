package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/policy"
)

// newDaemonCommand manages the containerd Boks runs.
//
// Boks does not contain containerd; it drives one. Until now that meant every user installed,
// configured and started containerd themselves, and the configuration is not guessable — five
// separate settings and directories, each of which fails in a way that names something other
// than itself. `boks daemon start` writes that configuration and runs the daemon with it.
//
// It is not required and it does not take over: the daemon it starts has its own root, state
// and endpoint, so a machine already running containerd for Docker is untouched, and anyone
// with a containerd set up the way they want keeps using it through --containerd-address.
func newDaemonCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the containerd that Boks drives",
		Long: `Starts, stops and inspects a containerd that Boks manages.

A boks binary on its own cannot start a sandbox: it orchestrates containerd, a VM shim, a
hypervisor library and a filesystem tool. containerd is the piece with the most ways to be
present but wrong, and none of them announce themselves — a diff-service order that omits the
erofs differ fails during an image unpack naming a differ, and a ttrpc socket containerd tries
to chown to uid 0 fails at startup naming a file. 'boks daemon start' writes a configuration
that has neither problem and runs containerd with it.

You do not have to run it. 'boks run', 'boks create', 'boks exec' and 'boks start' do it for
you when nothing else is serving, because needing a daemon first is Boks' problem rather than
yours. This command is for starting one on purpose, and for the questions the others cannot
answer: what is running, what it said, and what configuration it was given.

The daemon is Boks' own. Its root, its state and its endpoint are under your state directory,
so it cannot disturb — or be disturbed by — a containerd that Docker or your distribution is
running. Nothing is installed as a service and nothing runs at boot: it is started by a
command you ran, it runs in the background, and 'boks daemon stop' ends it.

Once it is running, Boks talks to it by default. An explicit --containerd-address, or
BOKS_CONTAINERD_ADDRESS, still wins — and pins the choice: Boks starts no daemon of its own
when you have named one.`,
	}
	cmd.AddCommand(
		newDaemonStartCommand(env),
		newDaemonStopCommand(env),
		newDaemonStatusCommand(env),
		newDaemonLogsCommand(env),
		newDaemonConfigCommand(env),
		newDaemonServeCommand(env),
	)
	return cmd
}

func newDaemonStartCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the containerd Boks manages, if it is not already running",
		Long: `Starts a containerd configured for Boks, and waits until it answers its own API before
reporting success — a socket that exists is not a daemon that is serving.

It returns to your prompt: the daemon is a background process with no terminal of its own, and
this command is finished once that process is serving. There is no flag for that because there
is no other mode; 'boks daemon serve' is the one that stays in the foreground, and it exists
for watching a daemon that will not start.

Starting one that is already running is not an error and does not restart it: this command
means "make sure it is up", and a restart would take down every sandbox the daemon is serving.
'boks run' does the same thing by itself, so this is only needed to start one on purpose.

If containerd refuses to start, what it said is printed here rather than left in a log file
for you to find.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir := policy.StateDir()
			if st, running := daemon.Lookup(stateDir); running {
				fmt.Fprintf(env.Stderr, "containerd is already running on %s\n", st.Address)
				return nil
			}
			_, err := daemon.Start(cmd.Context(), stateDir, env.Stderr)
			return err
		},
	}
}

func newDaemonStopCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the containerd Boks manages",
		Long: `Stops the managed containerd. Stopping one that is not running is not an error.

containerd's root is left alone, so the images it has already pulled are still there when it
is started again. Only 'boks daemon' state — the socket and the record of the running process
— is removed. That root is where the disk goes, and 'boks purge' is what removes it.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir := policy.StateDir()
			if _, running := daemon.Lookup(stateDir); !running {
				fmt.Fprintln(env.Stderr, "no boks-managed containerd is running")
				return daemon.Stop(stateDir)
			}
			if err := daemon.Stop(stateDir); err != nil {
				return err
			}
			fmt.Fprintln(env.Stderr, "stopped the boks-managed containerd")
			return nil
		},
	}
}

func newDaemonStatusCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report on the containerd Boks manages",
		Long: `Reports whether a Boks-managed containerd is running, and asks it for its version.

Those are two questions, and this command asks both on purpose. A supervisor holding its lock
says a process is alive; a version returned over the socket says containerd is actually
serving. They can disagree, and a status that collapsed them would call a daemon that answers
nothing "running".

Exits non-zero when no managed daemon is serving, so it can gate a script.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			status := daemon.Query(cmd.Context(), policy.StateDir())
			return writeDaemonStatus(env.Stdout, status)
		},
	}
}

// writeDaemonStatus renders a status and returns the exit this command takes on it.
func writeDaemonStatus(w io.Writer, status daemon.Status) error {
	if !status.Managed {
		fmt.Fprintln(w, "no boks-managed containerd is running")
		fmt.Fprintln(w, "Start one with 'boks daemon start'.")
		return &ExitError{Code: 1}
	}
	st := status.State
	fmt.Fprintf(w, "address    %s\n", st.Address)
	fmt.Fprintf(w, "binary     %s\n", st.Binary)
	if !st.Started.IsZero() {
		fmt.Fprintf(w, "uptime     %s\n", time.Since(st.Started).Round(time.Second))
	}
	fmt.Fprintf(w, "pid        %d (supervisor %d)\n", st.ContainerdPID, st.SupervisorPID)
	fmt.Fprintf(w, "config     %s\n", st.ConfigPath)
	fmt.Fprintf(w, "log        %s\n", st.LogPath)
	if status.Version == "" {
		fmt.Fprintf(w, "serving    no: %v\n", status.Err)
		return &ExitError{Code: 1}
	}
	fmt.Fprintf(w, "serving    containerd %s\n", status.Version)
	return nil
}

func newDaemonLogsCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "logs",
		Short: "Print the managed containerd's log",
		Long: `Prints what the managed containerd has written, which is everything it logs plus anything
the supervisor said on its way up.

The log belongs to the daemon rather than to a run of this command, so it survives the daemon
exiting: a containerd that died is exactly when this is worth reading, and it is truncated
only when a new one starts.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := daemon.LogPath(policy.StateDir())
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("no daemon log at %s; 'boks daemon start' creates one", path)
				}
				return err
			}
			_, err = env.Stdout.Write(data)
			return err
		},
	}
}

func newDaemonConfigCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Print the containerd configuration Boks would write",
		Long: `Prints the containerd configuration for this host, with the reason for every setting.

It is generated rather than shipped, because three of the settings cannot be written down
ahead of time: the uid and gid are yours, the paths are under your state directory, and
whether the erofs differ may be named at all depends on whether mkfs.erofs is installed —
naming it when it is absent takes the whole daemon down.

This prints what 'boks daemon start' would write now, which is not necessarily what a running
daemon was started with. 'boks daemon status' names that file.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := daemon.Config(policy.StateDir())
			if err != nil {
				return err
			}
			_, err = io.WriteString(env.Stdout, text)
			return err
		},
	}
}

// newDaemonServeCommand is the supervisor process itself.
//
// It is a normal command rather than a hidden one, for the same reason 'boks net serve' is:
// running the background process by hand in a terminal is the supported way to watch a daemon
// that will not start, and a process nobody can reproduce is a thing users are right to
// distrust.
func newDaemonServeCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the managed containerd in the foreground",
		Long: `Runs the managed containerd in the foreground and exits when it does. This is the process
'boks daemon start' puts in the background.

Run it by hand to watch a daemon that will not start: containerd's own output arrives on
stderr as it happens, rather than in a log file after the fact. Ctrl-C stops both.

stdout carries one line and nothing else — the marker that says containerd is serving — because
'boks daemon start' reads it.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return daemon.Serve(cmd.Context(), policy.StateDir(), env.Stdout, env.Stderr)
		},
	}
}
