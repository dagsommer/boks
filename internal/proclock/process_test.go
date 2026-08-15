package proclock

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// shortLivedProcess starts something that exits immediately, for the tests that need a PID
// which is certainly no longer running.
//
// It re-executes the test binary with a flag that makes it exit at once, rather than shelling
// out: there is no command spelled the same way on every platform, and a test that skipped on
// Windows would leave the platform whose Terminate has never been executed with no coverage
// of even this much.
func shortLivedProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	cmd := exec.Command(self, "-test.run=TestExitImmediately")
	cmd.Env = append(os.Environ(), exitNowEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process on %s: %v", runtime.GOOS, err)
	}
	return cmd
}

const exitNowEnv = "BOKS_PROCLOCK_TEST_EXIT_NOW"

// TestExitImmediately is the child half of shortLivedProcess. Under an ordinary test run the
// variable is unset and it does nothing.
func TestExitImmediately(t *testing.T) {
	if os.Getenv(exitNowEnv) == "" {
		t.Skip("not the child process")
	}
	os.Exit(0)
}
