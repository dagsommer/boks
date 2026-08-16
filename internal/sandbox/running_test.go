package sandbox_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/sandbox"
)

// Running is what decides whether a sandbox keeps its network when a command that touched it
// ends — see cli.releaseStack, and the run of 2026-08-16 where a guest command exiting 127 cost
// a live sandbox its network for good. Two answers matter and they are not symmetric: "it is
// not running" stops a stack, and "I could not tell" must not be mistaken for it.
//
// Both are exercised here against a **real containerd**, because the failure being guarded
// against is a lookup that reports absence when it means something else. No hypervisor, image
// or network is involved: a containerd that is answering is the whole fixture, which is why
// this is not behind BOKS_INTEGRATION. It follows internal/daemon's own test in that.

// TestRunningIsFalseForASandboxThatIsNotThere: the branch that stops a stack. It has to be
// reachable, and it has to be a plain false rather than an error, or a run that failed before
// it created anything would leave its supervisor holding a socket for nobody.
func TestRunningIsFalseForASandboxThatIsNotThere(t *testing.T) {
	address := liveContainerd(t)

	running, err := sandbox.Running(t.Context(), address, "boks-no-such-sandbox")
	if err != nil {
		t.Fatalf("Running against a container that does not exist returned %v; a missing sandbox "+
			"is an answer, not a failure", err)
	}
	if running {
		t.Error("a sandbox that does not exist reported as running")
	}
}

// TestRunningReportsAnUnreachableContainerdAsAnError is the other half, and the one the bias
// rests on. If this returned (false, nil) the cleanup path would read "not running" from a
// containerd it simply could not reach, and take the network from whatever was up.
func TestRunningReportsAnUnreachableContainerdAsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a containerd address on Windows is a named pipe, not a path that can be absent")
	}
	running, err := sandbox.Running(t.Context(), "/nonexistent/containerd.sock", "boks-test")
	if err == nil {
		t.Fatalf("an unreachable containerd answered running=%v with no error", running)
	}
}

// liveContainerd starts one for this test and returns its address, or skips.
func liveContainerd(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no containerd to drive on this platform")
	}
	if _, err := exec.LookPath("containerd"); err != nil {
		t.Skip("no containerd on PATH")
	}
	// Not t.TempDir(): the test's name goes in that path, and containerd's socket has a
	// 104-byte limit that a long path spends before boks gets to it.
	stateDir, err := os.MkdirTemp("", "bks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Serve(ctx, stateDir, io.Discard, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("containerd did not stop")
		}
	})

	address := daemon.Address(stateDir)
	deadline := time.Now().Add(60 * time.Second)
	for !daemon.Query(ctx, stateDir).Managed {
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(daemon.LogPath(stateDir))
			t.Fatalf("containerd never came up:\n%s", strings.TrimSpace(string(log)))
		}
		select {
		case err := <-done:
			log, _ := os.ReadFile(daemon.LogPath(stateDir))
			t.Skipf("containerd would not start on this host (%v):\n%s", err, strings.TrimSpace(string(log)))
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	return address
}
