package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/policy"
)

// autostartStubs replaces the three questions ensureDaemon asks and records whether it tried
// to start anything, so the decision can be tested without a containerd anywhere.
type autostartStubs struct {
	running  bool
	serving  bool
	started  int
	startErr error
	// startedAddress is what the daemon reports itself listening on, so the test can tell
	// "ensureDaemon adopted the daemon it started" from "the address happened to match".
	startedAddress string
}

func (s *autostartStubs) install(t *testing.T) {
	t.Helper()
	oldRunning, oldServing, oldStart := daemonRunning, daemonServing, daemonStart
	t.Cleanup(func() { daemonRunning, daemonServing, daemonStart = oldRunning, oldServing, oldStart })

	daemonRunning = func(string) bool { return s.running }
	daemonServing = func(context.Context, string) bool { return s.serving }
	daemonStart = func(context.Context, string, io.Writer) (daemon.State, error) {
		s.started++
		if s.startErr != nil {
			return daemon.State{}, s.startErr
		}
		return daemon.State{Address: s.startedAddress}, nil
	}
}

// newDevFlags builds a devFlags the way the root command does, so that the flagset it keeps
// is a real one and Changed() answers for real.
func newDevFlags(t *testing.T, args ...string) *devFlags {
	t.Helper()
	dev := &devFlags{}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	dev.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return dev
}

// The whole decision table. The three "hands off" cases are the ones with teeth: each is a
// situation where starting a daemon would be wrong, and a guard that stopped working would
// otherwise cost a stranger's machine a containerd it did not ask for.
func TestEnsureDaemonDecides(t *testing.T) {
	const managed = "/state/containerd/containerd.sock"

	tests := []struct {
		name string
		// how the world is
		args    []string
		envAddr string
		running bool
		serving bool
		// what must happen. wantAddress empty means "left as it was"; wantManaged means
		// the managed daemon's endpoint, which is only known once the state directory is.
		wantStarted bool
		wantManaged bool
		wantAddress string
	}{
		{
			name: "starts one when nothing is serving",
			// The case this function exists for: a fresh host, and `boks run`
			// used to fail here with an error about a missing socket.
			wantStarted: true,
			wantAddress: managed,
		},
		{
			name:        "leaves a managed daemon alone",
			running:     true,
			wantStarted: false,
			wantManaged: true,
		},
		{
			name: "leaves somebody else's containerd alone",
			// A distribution's daemon, or one the user runs. Boks talked to it
			// before autostart existed and must still: starting a second
			// containerd because the first is not ours would be a preference,
			// not a fix.
			serving:     true,
			wantStarted: false,
		},
		{
			name: "will not start one when --containerd-address named a daemon",
			// Naming an address says which containerd is meant. If it is down,
			// that is the answer — starting a different one obeys the opposite
			// of the instruction.
			args:        []string{"--containerd-address", "/somewhere/else.sock"},
			wantStarted: false,
			wantAddress: "/somewhere/else.sock",
		},
		{
			name:        "will not start one when BOKS_CONTAINERD_ADDRESS named a daemon",
			envAddr:     "/from/the/environment.sock",
			wantStarted: false,
			wantAddress: "/from/the/environment.sock",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A state directory of its own, so that the managed daemon's endpoint is
			// something this test knows rather than something the developer's machine
			// decides.
			t.Setenv(policy.StateDirEnv, t.TempDir())
			if tc.envAddr != "" {
				t.Setenv("BOKS_CONTAINERD_ADDRESS", tc.envAddr)
			}
			stubs := &autostartStubs{running: tc.running, serving: tc.serving, startedAddress: managed}
			stubs.install(t)

			dev := newDevFlags(t, tc.args...)
			before := dev.address
			if err := ensureDaemon(context.Background(), dev, io.Discard); err != nil {
				t.Fatal(err)
			}

			if started := stubs.started > 0; started != tc.wantStarted {
				t.Errorf("started a daemon = %v, want %v", started, tc.wantStarted)
			}
			if stubs.started > 1 {
				t.Errorf("started %d daemons", stubs.started)
			}
			want := tc.wantAddress
			switch {
			case tc.wantManaged:
				want = daemon.Address(policy.StateDir())
			case want == "":
				want = before
			}
			if dev.address != want {
				t.Errorf("address = %q, want %q", dev.address, want)
			}
		})
	}
}

// A daemon that cannot start is the command's failure, and the reason must survive: it is the
// only thing that says what is missing. containerd's own startup errors are the reason
// internal/daemon has a supervisor at all, and swallowing them here would undo that.
func TestEnsureDaemonReportsAFailureToStart(t *testing.T) {
	sentinel := errors.New("containerd exited before it was ready")
	stubs := &autostartStubs{startErr: sentinel}
	stubs.install(t)

	dev := newDevFlags(t)
	err := ensureDaemon(context.Background(), dev, io.Discard)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
}

// Starting a background process that outlives the command is a thing the user is entitled to
// hear about, and the writer ensureDaemon is handed is how they do. It is stderr in every
// caller, so the output cannot be mistaken for the guest's.
func TestEnsureDaemonPassesTheProgressWriterThrough(t *testing.T) {
	var got io.Writer
	oldStart := daemonStart
	oldRunning, oldServing := daemonRunning, daemonServing
	t.Cleanup(func() { daemonStart, daemonRunning, daemonServing = oldStart, oldRunning, oldServing })
	daemonRunning = func(string) bool { return false }
	daemonServing = func(context.Context, string) bool { return false }
	daemonStart = func(_ context.Context, _ string, w io.Writer) (daemon.State, error) {
		got = w
		return daemon.State{Address: "/x.sock"}, nil
	}

	var progress bytes.Buffer
	if err := ensureDaemon(context.Background(), newDevFlags(t), &progress); err != nil {
		t.Fatal(err)
	}
	if got != io.Writer(&progress) {
		t.Errorf("daemon.Start was given %v, not the writer ensureDaemon was handed", got)
	}
}
