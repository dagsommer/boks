package enforce

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
)

// The supervisor is a *process*, and the things most likely to go wrong about it — that it
// detaches, that it reports readiness before the caller starts a VM, that a second command
// reuses it instead of starting a rival, that it cleans up after itself, that a crashed one
// is detected — are only true of a real process. So these tests spawn one.
//
// The binary spawned is this test binary: Ensure re-executes whatever os.Executable() says,
// which under `go test` is the test binary rather than boks. TestMain therefore answers to
// the same call, running the supervisor for real. That keeps the spawn path itself under
// test rather than mocked, at the cost of this small piece of scaffolding.
const supervisorEnv = "BOKS_TEST_SUPERVISOR"

// stopSentinel is how the fake sandbox "stops": the watch function returns when the file
// appears, standing in for the containerd task the real supervisor watches.
const stopSentinel = "task-gone"

func TestMain(m *testing.M) {
	if os.Getenv(supervisorEnv) != "1" {
		os.Exit(m.Run())
	}
	spec, err := ReadSpec(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	err = Serve(context.Background(), spec, os.Stdout, func(ctx context.Context) error {
		sentinel := filepath.Join(spec.StateDir, stopSentinel)
		for {
			if _, err := os.Stat(sentinel); err == nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestSupervisorServesAndIsReusedAndStops walks the whole life of a sandbox's network as
// the CLI sees it: start one, find it answering, have a second command reuse it rather than
// start a rival, then stop it and find nothing left.
func TestSupervisorServesAndIsReusedAndStops(t *testing.T) {
	t.Setenv(supervisorEnv, "1") // inherited by the process Ensure spawns
	spec := testSpec(t, network.ModeNAT)
	// A rule to observe from the other end of the link: the point is that the policy
	// reached the process that enforces it, not merely that something answered.
	setPolicy(t, &spec, policy.PresetOpen, nil, []string{"denied.example.com"})

	state, err := Ensure(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if state.PID == os.Getpid() {
		t.Fatal("the stack is running in this process; it must outlive the command that started it")
	}
	// Readiness means the link socket is bound. It has to, because the VM connects to it
	// while it boots and a socket that appears later is a boot failure.
	if _, err := os.Stat(spec.Plan.Socket); err != nil {
		t.Fatalf("the supervisor reported ready without a link socket: %v", err)
	}

	// It is serving for real: a guest on the far end of the link reaches the proxy.
	guest := attachGuest(t, spec)
	client, err := guest.HTTPClient(state.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://denied.example.com/")
	if err != nil {
		t.Fatalf("the guest could not reach the proxy served by the supervisor: %v", err)
	}
	resp.Body.Close()
	if resp.Header.Get("Boks-Policy") != "deny" {
		t.Errorf("the supervisor's proxy did not apply the policy: %s", resp.Status)
	}
	guest.Close()

	// A second command attaching to the same sandbox reuses the stack. Starting a second
	// one would hand the VM a duplicate address, or fail to bind, depending on the order.
	again, err := Ensure(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Ensure on a running stack: %v", err)
	}
	if again.PID != state.PID {
		t.Errorf("a second stack was started (pid %d, was %d)", again.PID, state.PID)
	}

	if err := Stop(spec.StateDir, spec.Sandbox); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, alive := Lookup(spec.StateDir, spec.Sandbox); alive {
		t.Error("the supervisor is still running after Stop")
	}
	if _, err := os.Stat(StateDir(spec.StateDir, spec.Sandbox)); !os.IsNotExist(err) {
		t.Errorf("the sandbox's network directory survived Stop: %v", err)
	}
	if err := Stop(spec.StateDir, spec.Sandbox); err != nil {
		t.Errorf("Stop twice returned %v; it sits on cleanup paths and must be idempotent", err)
	}
}

// TestSupervisorExitsWithTheSandbox is the property that makes this a supervisor rather
// than a daemon: nothing has to remember to stop it. When the sandbox's task goes, so does
// the stack — and with it the socket, the state file and the process.
func TestSupervisorExitsWithTheSandbox(t *testing.T) {
	t.Setenv(supervisorEnv, "1")
	spec := testSpec(t, network.ModeNAT)

	if _, err := Ensure(context.Background(), spec, io.Discard); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Stand in for the sandbox's task exiting.
	if err := os.WriteFile(filepath.Join(spec.StateDir, stopSentinel), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, alive := Lookup(spec.StateDir, spec.Sandbox); !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the supervisor outlived the sandbox it was serving")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(spec.Plan.Socket); !os.IsNotExist(err) {
		t.Errorf("the link socket survived the sandbox: %v", err)
	}
}

// TestACrashedSupervisorIsDetectedAndReplaced: a state file is a claim about the past. What
// makes it safe to act on is the lock — held by the kernel, released however the holder
// dies — so leftovers from a crash are recognised as leftovers and cleared rather than
// mistaken for a running stack or, worse, signalled at a PID somebody else now owns.
func TestACrashedSupervisorIsDetectedAndReplaced(t *testing.T) {
	t.Setenv(supervisorEnv, "1")
	spec := testSpec(t, network.ModeNAT)

	dir := StateDir(spec.StateDir, spec.Sandbox)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What a crash leaves: a state file naming a PID that is not ours to signal, an
	// unlocked lock file, and a socket nothing is listening on.
	if err := writeState(dir, State{Sandbox: spec.Sandbox, PID: 999999, Mode: "nat"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{lockFile, "net.sock"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, alive := Lookup(spec.StateDir, spec.Sandbox); alive {
		t.Fatal("a crashed supervisor was reported as running")
	}

	state, err := Ensure(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Ensure over the remains of a crashed supervisor: %v", err)
	}
	defer Stop(spec.StateDir, spec.Sandbox)
	if state.PID == 999999 {
		t.Error("the stale state was taken at face value")
	}
	if _, alive := Lookup(spec.StateDir, spec.Sandbox); !alive {
		t.Error("no supervisor is running after Ensure")
	}
}

// TestLockIsExclusive states the property the rest of this file rests on.
func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lockFile)

	if locked(path) {
		t.Error("an absent lock file was reported as held")
	}
	release, err := acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !locked(path) {
		t.Error("a held lock was reported as free")
	}
	if _, err := acquire(path); err == nil {
		t.Error("the lock was acquired twice")
	}
	release()
	if locked(path) {
		t.Error("a released lock was reported as held")
	}
}
