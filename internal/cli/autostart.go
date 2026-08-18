package cli

import (
	"context"
	"io"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/policy"
)

// ensureDaemon makes sure a containerd is serving before a command that needs one runs, and
// starts the managed one if nothing is.
//
// # Why this is not left to the user
//
// A boks binary on its own cannot start a sandbox; it drives containerd. That was true from
// the beginning, and it meant `boks run` on a machine that had not been prepared failed with
// "containerd socket …: no such file or directory" — an error about a file, for a user who
// asked to run an agent. `boks daemon start` was the answer and nothing said so at the moment
// it was needed. Starting the daemon here is not a convenience; it removes a step that exists
// only because of how Boks is built, which is not the user's problem to hold.
//
// # What it will not do
//
// It starts *the managed daemon*, and only when nothing else is answering. Three cases keep
// their hands off:
//
//   - An address the user named, through --containerd-address or BOKS_CONTAINERD_ADDRESS.
//     Someone who points Boks at a particular containerd has said which one they mean, and
//     starting a second because theirs is down would obey the opposite of the instruction.
//   - A managed daemon that is already running. This is the ordinary case after the first
//     command, and it costs a lock check rather than a connection.
//   - A containerd already serving at the address Boks would use anyway — a distribution's, or
//     one the user runs themselves. Boks used that daemon before this function existed and it
//     still does; autostart is what happens when the alternative is failing, not a preference
//     for Boks' own daemon over a working one.
//
// The daemon it starts is the same one `boks daemon start` starts, with the same output, and
// it outlives the command exactly as it did before — `boks daemon stop` is still what ends it.
// Nothing is installed, and nothing runs at boot.
// The three questions ensureDaemon asks, as variables so that a test can answer them without
// a containerd. Each is the real function in a running boks; nothing else replaces them.
var (
	daemonRunning = daemon.Running
	daemonServing = daemon.Serving
	daemonStart   = daemon.Start
)

func ensureDaemon(ctx context.Context, dev *devFlags, stderr io.Writer) error {
	if dev.addressNamed() {
		return nil
	}
	stateDir := policy.StateDir()
	if daemonRunning(stateDir) {
		// The flag's default already resolved to this, since daemon.DefaultAddress
		// consults the same record. Setting it again is not a correction, it is what
		// makes that dependency local instead of an assumption about flag registration
		// order.
		dev.address = daemon.Address(stateDir)
		return nil
	}
	if daemonServing(ctx, dev.address) {
		return nil
	}
	st, err := daemonStart(ctx, stateDir, stderr)
	if err != nil {
		return err
	}
	dev.address = st.Address
	return nil
}
