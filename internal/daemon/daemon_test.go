package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/proclock"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

// shortStateDir gives a state directory whose socket path fits containerd's 104-byte limit.
// t.TempDir builds a name out of the test's own, which for a long test name plus
// /containerd/containerd.sock.ttrpc can exceed it — and then the test would fail on the check
// rather than on what it is testing.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// The ordering is the behaviour a user meets first: `boks daemon start` has to change what
// every other command talks to, or it has done nothing.
func TestDefaultAddressPrefersTheManagedDaemon(t *testing.T) {
	stateDir := shortStateDir(t)
	t.Setenv("BOKS_CONTAINERD_ADDRESS", "")

	// Nothing running: containerd's own platform default.
	if got, want := DefaultAddress(stateDir), runtimecfg.DefaultAddress(); got != want {
		t.Errorf("with no managed daemon, DefaultAddress() = %q, want %q", got, want)
	}

	// A held lock and a state file is a running managed daemon.
	release := pretendRunning(t, stateDir)
	if got, want := DefaultAddress(stateDir), Address(stateDir); got != want {
		t.Errorf("with a managed daemon, DefaultAddress() = %q, want %q", got, want)
	}

	// An explicit override is more specific than either, and still wins.
	t.Setenv("BOKS_CONTAINERD_ADDRESS", "/somewhere/else.sock")
	if got := DefaultAddress(stateDir); got != "/somewhere/else.sock" {
		t.Errorf("BOKS_CONTAINERD_ADDRESS did not win: DefaultAddress() = %q", got)
	}
	release()
}

// pretendRunning creates the two things Lookup consults — a state file and a held lock —
// without starting containerd, so the state machine can be tested on a host that has none.
func pretendRunning(t *testing.T, stateDir string) func() {
	t.Helper()
	dir := Dir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dir, State{Address: Address(stateDir), ContainerdPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	release, err := proclock.Acquire(filepath.Join(dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	if !Running(stateDir) {
		release()
		t.Fatal("a held lock and a state file did not read as a running daemon")
	}
	return release
}

// A state file with no lock behind it is the trace of a crash, and must never be reported as
// a running daemon — that is the whole reason liveness is a lock rather than the PID in it.
func TestLookupDoesNotTrustAStateFileAlone(t *testing.T) {
	stateDir := shortStateDir(t)
	dir := Dir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dir, State{Address: "/x.sock", ContainerdPID: 999999}); err != nil {
		t.Fatal(err)
	}
	st, running := Lookup(stateDir)
	if running {
		t.Error("Lookup reported a daemon running with nothing holding the lock")
	}
	// The record is still returned, so a caller can say where the log was.
	if st.Address != "/x.sock" {
		t.Errorf("Lookup discarded the record of a dead daemon: %+v", st)
	}
}

func TestLookupSurvivesATruncatedStateFile(t *testing.T) {
	stateDir := shortStateDir(t)
	dir := Dir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, running := Lookup(stateDir); running {
		t.Error("a truncated state file read as a running daemon")
	}
}

// Callers of Stop want "make sure it is gone", so stopping nothing is a success.
func TestStopIsQuietWhenNothingIsRunning(t *testing.T) {
	if err := Stop(shortStateDir(t)); err != nil {
		t.Errorf("Stop with no daemon running returned %v", err)
	}
}

// The failure without this check names the socket, not the state directory that made it long
// — and the state directory is the only part the user can change.
func TestSocketPathLimitNamesTheFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a named pipe has no path length limit")
	}
	if err := checkSocketPath("/tmp/bd/containerd/containerd.sock"); err != nil {
		t.Errorf("a short socket path was refused: %v", err)
	}
	long := "/" + strings.Repeat("d", 120) + "/containerd.sock"
	err := checkSocketPath(long)
	if err == nil {
		t.Fatal("a socket path over containerd's limit was accepted")
	}
	if !strings.Contains(err.Error(), "BOKS_STATE_DIR") {
		t.Errorf("the refusal does not name what to change:\n%v", err)
	}
}

// The log tail is what `boks daemon start` prints when containerd refuses, so it must contain
// containerd's words and not the supervisor's own advice, which is longer and comes first.
func TestLogTailReportsContainerdNotTheSupervisor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "containerd.log")
	body := "boks: shim socket directory: /run/containerd is not writable by you\n" +
		"  a long remedy\n  spanning several lines\n" +
		logMarker + " /usr/bin/containerd --config /x\n" +
		`level=fatal msg="needed differ not loaded: erofs"` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := logTail(path, 12)
	if !strings.Contains(tail, "needed differ not loaded") {
		t.Errorf("the tail lost containerd's error:\n%s", tail)
	}
	if strings.Contains(tail, "a long remedy") {
		t.Errorf("the tail reported the supervisor's own advice as containerd's:\n%s", tail)
	}
}

// A log with nothing after the marker still has to produce something rather than an empty
// error, because "it exited" with no reason is the message this package exists to replace.
func TestStartFailureAlwaysSaysSomething(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "containerd.log")
	if err := os.WriteFile(path, []byte(logMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := startFailure(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("startFailure on an empty log = %v, want something naming %s", err, path)
	}
}

// --- against a real containerd ------------------------------------------------------------

// TestServeRunsARealContainerd drives Serve against the containerd on this host.
//
// It exercises everything except the detach: the generated configuration is written, containerd
// is started with it, the wait for it to answer its own API succeeds, the state file appears,
// and a shutdown request takes it down. Start's own path — spawning `boks daemon serve` from
// os.Executable() — cannot be exercised from a test binary, which has no such subcommand.
//
// Skipped where there is no containerd. It is not gated behind BOKS_INTEGRATION because it
// needs no hypervisor, no image and no network: it is a local process that answers a socket.
func TestServeRunsARealContainerd(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no containerd to drive on this platform")
	}
	if _, err := exec.LookPath("containerd"); err != nil {
		t.Skip("no containerd on PATH")
	}
	stateDir := shortStateDir(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, stateDir, stdout, stderr) }()

	deadline := time.Now().Add(readyTimeout + 10*time.Second)
	for !strings.Contains(stdout.String(), readyMarker) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("containerd never reported ready.\nstderr:\n%s", stderr.String())
		}
		select {
		case err := <-done:
			t.Fatalf("Serve returned before ready: %v\nstderr:\n%s", err, stderr.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}

	// It is serving, and Query proves it by asking containerd rather than by reading a file.
	status := Query(ctx, stateDir)
	if !status.Managed {
		t.Fatal("a running daemon did not read as managed")
	}
	if status.Version == "" {
		t.Fatalf("containerd answered no version: %v", status.Err)
	}
	if status.State.ContainerdPID == 0 || status.State.SupervisorPID != os.Getpid() {
		t.Errorf("the state file records the wrong processes: %+v", status.State)
	}

	// The configuration it was started with is on disk and is the one this host needs.
	config, err := os.ReadFile(status.State.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := "default = ['" + strings.Join(diffOrder(runtime.GOOS, HasEROFS()), "', '") + "']"
	if !strings.Contains(string(config), wantOrder) {
		t.Errorf("the running config does not contain %q", wantOrder)
	}

	// And stopping it works: the context is the supervisor's shutdown request.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v on shutdown", err)
		}
	case <-time.After(stopGrace + 5*time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
	if Running(stateDir) {
		t.Error("the daemon still reads as running after Serve returned")
	}
	if _, err := os.Stat(filepath.Join(Dir(stateDir), stateFile)); err == nil {
		t.Error("the state file survived a clean shutdown")
	}
}

// A second Serve against the same state directory must refuse rather than start a second
// containerd on the same socket.
func TestServeRefusesASecondDaemon(t *testing.T) {
	stateDir := shortStateDir(t)
	release := pretendRunning(t, stateDir)
	defer release()

	err := Serve(t.Context(), stateDir, &syncBuffer{}, &syncBuffer{})
	if err == nil {
		t.Fatal("Serve started a second daemon over a held lock")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}
