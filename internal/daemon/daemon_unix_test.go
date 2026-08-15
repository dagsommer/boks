//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A containerd that never answers is the case the shutdown test does not reach, and it is the
// one that leaked: Serve used to return from waitReady's error path with the child still
// running, releasing the lock behind a live process holding the socket. Nothing would then be
// tracking it — `boks daemon status` would report nothing, and the next start could not bind.
//
// The stand-in starts, records its pid and sleeps. It binds nothing, so waitReady can only end
// by timing out or being cancelled, and the test cancels.
func TestServeKillsAContainerdThatNeverAnswers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in here is a shell script")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fake := filepath.Join(dir, "containerd")
	script := "#!/bin/sh\necho $$ > " + pidFile + "\nwhile true; do sleep 1; done\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(binaryEnv, fake)

	stateDir := shortStateDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, stateDir, &syncBuffer{}, &syncBuffer{}) }()

	pid := waitForPIDFile(t, pidFile)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned no error for a containerd that never answered")
		}
	case <-time.After(stopGrace + 10*time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}

	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the stand-in containerd (pid %d) outlived Serve", pid)
	}
}

// waitForPIDFile waits for the stand-in to record its own pid.
func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			var pid int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the stand-in containerd never wrote %s", path)
	return 0
}

// Serve must never return while containerd is still running.
//
// An orphan is the one outcome worse than a failure to start: nothing tracks the process, so
// `boks daemon status` reports nothing running while a live containerd holds the socket, and
// the next `boks daemon start` cannot bind it. This constructs the case by cancelling the
// context and then asking the operating system whether the child is gone.
func TestServeLeavesNoOrphanedContainerd(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no containerd to drive on this platform")
	}
	if _, err := exec.LookPath("containerd"); err != nil {
		t.Skip("no containerd on PATH")
	}
	stateDir := shortStateDir(t)

	ctx, cancel := context.WithCancel(t.Context())
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, stateDir, stdout, stderr) }()

	deadline := time.Now().Add(readyTimeout + 10*time.Second)
	for !strings.Contains(stdout.String(), readyMarker) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("containerd never reported ready.\nstderr:\n%s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	pid := Query(ctx, stateDir).State.ContainerdPID
	if pid == 0 {
		t.Fatal("no containerd pid was recorded")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(stopGrace + 5*time.Second):
		t.Fatal("Serve did not return")
	}

	// Serve has returned, so by its contract containerd is gone. Signal 0 asks the kernel
	// rather than trusting the contract.
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("containerd (pid %d) was still running after Serve returned", pid)
	}
}
