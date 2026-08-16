package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
)

// What `boks run` does to a sandbox's network when the command it ran is over.
//
// This is about a *process*, so these tests start one. The stack is a real supervisor holding a
// real link socket and a real lock, exactly as the one a sandbox gets, and what is asserted is
// whether that process is still alive afterwards — not whether a function returned true.
//
// The defect these pin was measured on Windows on 2026-08-16: a guest command that exited 127
// took the sandbox's network with it while the sandbox kept running, because the cleanup asked
// whether the *command* had failed rather than whether the *sandbox* was still there. See
// releaseStack.

const stackChildEnv = "BOKS_TEST_STACK_SUPERVISOR"

// TestStackSupervisorChild is the supervisor half of these tests, re-executed as a child
// process. Under an ordinary run the variable is unset and it does nothing.
//
// It is a test function rather than a TestMain so that this file brings its own child process
// without changing how every other test in the package starts. The watch it gives Serve stands
// in for a sandbox that stays up: the real one returns when containerd reports the task gone,
// and there is no containerd, no task and no hypervisor here.
func TestStackSupervisorChild(t *testing.T) {
	if os.Getenv(stackChildEnv) == "" {
		t.Skip("not the child process")
	}
	spec, err := enforce.ReadSpec(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	err = enforce.Serve(context.Background(), spec, os.Stdout, func(ctx context.Context, started func()) error {
		started()
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// liveStack starts a supervisor for one test and returns the sandbox it serves, once the stack
// is up. The state directory is this test's, so the cleanup paths under test — which read it
// through policy.StateDir() — find this stack and no other.
func liveStack(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The supervisor refuses to run where the link cannot be host-terminated, so
		// there would be no process here to keep or kill.
		t.Skip("no host-terminated link on this platform")
	}
	stateDir := shortStateDir(t)
	t.Setenv("BOKS_STATE_DIR", stateDir)

	const name = "boks-test"
	// The CLI's own plan, so the socket lands where the supervisor looks rather than where
	// this test guessed.
	plan, err := (&policyFlags{}).planFor(name, network.ModeNAT)
	if err != nil {
		t.Fatalf("planFor: %v", err)
	}
	resolution, err := (policy.Request{Preset: policy.PresetOpen}).Resolve()
	if err != nil {
		t.Fatalf("resolving the test policy: %v", err)
	}
	payload, err := json.Marshal(enforce.Spec{
		Sandbox:    name,
		Plan:       plan,
		Resolution: &resolution,
		StateDir:   stateDir,
		CADir:      filepath.Join(stateDir, "ca"),
	})
	if err != nil {
		t.Fatal(err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	logPath := filepath.Join(stateDir, "child.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	cmd := exec.Command(self, "-test.run=TestStackSupervisorChild")
	cmd.Env = append(os.Environ(), stackChildEnv+"=1", "BOKS_STATE_DIR="+stateDir)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process on %s: %v", runtime.GOOS, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(20 * time.Second)
	for {
		if state, alive := enforce.Lookup(stateDir, name); alive {
			// The whole point is a process that is not this one. A stack running
			// in the test would make every assertion below about nothing.
			if state.PID == os.Getpid() {
				t.Fatalf("the stack is running in the test process (pid %d)", state.PID)
			}
			if _, err := os.Stat(state.Socket); err != nil {
				t.Fatalf("the supervisor is up without a link socket: %v", err)
			}
			return name
		}
		if time.Now().After(deadline) {
			body, _ := os.ReadFile(logPath)
			t.Fatalf("the test supervisor never came up:\n%s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAFailingCommandKeepsTheNetworkOfARunningSandbox is the Windows defect, as a test.
//
// The sandbox is up; the command that ran inside it is over, whatever it exited with. The
// stack must still be there, because the guest attached to that socket when it booted and will
// never attach to another one.
func TestAFailingCommandKeepsTheNetworkOfARunningSandbox(t *testing.T) {
	name := liveStack(t)
	stateDir := policy.StateDir()
	before, _ := enforce.Lookup(stateDir, name)

	releaseStack(name, false, func() bool { return true }, io.Discard)

	after, alive := enforce.Lookup(stateDir, name)
	if !alive {
		t.Fatal("a run that ended in a non-zero exit took the network away from a running sandbox; " +
			"the guest cannot re-attach, so that sandbox has no network for the rest of its life")
	}
	if after.PID != before.PID {
		t.Errorf("the stack was replaced (pid %d, was %d)", after.PID, before.PID)
	}
	if _, err := os.Stat(after.Socket); err != nil {
		t.Errorf("the link socket the guest is attached to is gone: %v", err)
	}
}

// TestAnInterruptedRunKeepsTheNetworkOfARunningSandbox: Ctrl-C is the case internal/enforce
// says the supervisor exists for — a background build in a sandbox must not lose its network
// because somebody pressed Ctrl-C in another terminal. It reaches releaseStack the same way a
// non-zero exit does, as an error from the command, so it is the same decision.
func TestAnInterruptedRunKeepsTheNetworkOfARunningSandbox(t *testing.T) {
	name := liveStack(t)
	stateDir := policy.StateDir()

	releaseStack(name, false, func() bool { return true }, io.Discard)

	if _, alive := enforce.Lookup(stateDir, name); !alive {
		t.Fatal("an interrupted run disconnected a sandbox that is still running")
	}
}

// TestARunThatNeverStartedASandboxTakesItsStackWithIt is the other half, and the reason the
// cleanup is there at all: a run that failed before the VM came up must not leave a supervisor
// holding a socket for a guest that will never connect.
func TestARunThatNeverStartedASandboxTakesItsStackWithIt(t *testing.T) {
	name := liveStack(t)
	stateDir := policy.StateDir()
	state, _ := enforce.Lookup(stateDir, name)

	releaseStack(name, false, func() bool { return false }, io.Discard)

	if _, alive := enforce.Lookup(stateDir, name); alive {
		t.Fatal("the stack of a sandbox that is not running was left behind")
	}
	if _, err := os.Stat(state.Socket); !os.IsNotExist(err) {
		t.Errorf("the link socket survived a stack nothing is attached to: %v", err)
	}
}

// TestAnUnanswerableStatusKeepsTheStack pins the bias the whole decision rests on: when boks
// cannot find out whether the sandbox is running, it must not conclude that it is not.
//
// Keeping a stack nothing needs costs one process that reaps itself — the supervisor watches
// the task and exits when it goes, or when none appears. Stopping one that is needed costs a
// running guest its network permanently. The asymmetry is the argument, and this is the branch
// that acts on it.
func TestAnUnanswerableStatusKeepsTheStack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a containerd address on Windows is a named pipe, not a path that can be absent")
	}
	var note bytes.Buffer
	gone := filepath.Join(shortStateDir(t), "no-containerd-here.sock")

	if !sandboxIsRunning(context.Background(), gone, "boks-test", &note) {
		t.Error("an unreachable containerd read as a sandbox that is not running, which is how a " +
			"live guest loses its network")
	}
	if !strings.Contains(note.String(), "could not tell") {
		t.Errorf("the reason was not reported:\n%s", note.String())
	}
}

// TestAnEphemeralRunAlwaysTakesItsStackWithIt: --rm removes the sandbox, so its network goes
// too, and the question of whether it is running does not arise.
func TestAnEphemeralRunAlwaysTakesItsStackWithIt(t *testing.T) {
	name := liveStack(t)
	stateDir := policy.StateDir()

	// Answering "yes, it is running" is what an ephemeral run's sandbox looks like at the
	// moment this is decided: the deferred removal has not necessarily happened yet.
	releaseStack(name, true, func() bool { return true }, io.Discard)

	if _, alive := enforce.Lookup(stateDir, name); alive {
		t.Fatal("an ephemeral sandbox left its network stack running")
	}
}
