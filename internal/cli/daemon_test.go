package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/policy"
)

// isolatedState points every command in a test at a state directory of its own, short enough
// that the containerd socket path stays inside containerd's 104-byte limit.
func isolatedState(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(policy.StateDirEnv, dir)
	return dir
}

// A status command whose whole job is to say whether something is running has to exit
// non-zero when it is not, or no script can gate on it.
func TestDaemonStatusExitsNonZeroWhenNothingRuns(t *testing.T) {
	isolatedState(t)
	stdout, _, code := mainExitCode(t, "daemon", "status")
	if code == 0 {
		t.Errorf("`boks daemon status` exited 0 with no daemon running:\n%s", stdout)
	}
	if !strings.Contains(stdout, "boks daemon start") {
		t.Errorf("the status does not say how to start one:\n%s", stdout)
	}
}

// `boks daemon config` is the answer to "what is this daemon actually configured with", so it
// has to work before any daemon exists — that is exactly when somebody asks.
func TestDaemonConfigPrintsAUsableConfig(t *testing.T) {
	stateDir := isolatedState(t)
	stdout, _, err := runCLI(t, "", "daemon", "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"version = 3",
		"[grpc]",
		"io.containerd.service.v1.diff-service",
		daemon.Dir(stateDir),
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("`boks daemon config` never printed %q:\n%s", want, stdout)
		}
	}
}

// A missing log is a normal state, not a crash, and the message has to name the command that
// creates one rather than the errno.
func TestDaemonLogsExplainsAnAbsentLog(t *testing.T) {
	isolatedState(t)
	_, _, err := runCLI(t, "", "daemon", "logs")
	if err == nil {
		t.Fatal("`boks daemon logs` succeeded with no log")
	}
	if !strings.Contains(err.Error(), "boks daemon start") {
		t.Errorf("the error does not say how to get a log: %v", err)
	}
}

func TestDaemonLogsPrintsTheLog(t *testing.T) {
	stateDir := isolatedState(t)
	dir := daemon.Dir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "level=info msg=\"serving...\" address=/x.sock\n"
	if err := os.WriteFile(filepath.Join(dir, "containerd.log"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCLI(t, "", "daemon", "logs")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != body {
		t.Errorf("`boks daemon logs` printed %q, want %q", stdout, body)
	}
}

// Stopping nothing is a success, because the caller wants "make sure it is gone".
func TestDaemonStopIsQuietWhenNothingRuns(t *testing.T) {
	isolatedState(t)
	_, stderr, code := mainExitCode(t, "daemon", "stop")
	if code != 0 {
		t.Errorf("`boks daemon stop` exited %d with no daemon running", code)
	}
	if !strings.Contains(stderr, "no boks-managed containerd is running") {
		t.Errorf("`boks daemon stop` said nothing useful:\n%s", stderr)
	}
}

// The subcommands are the product surface of this feature; a missing one is a regression a
// reader of the help page would notice before any test did.
func TestDaemonHelpListsEverySubcommand(t *testing.T) {
	isolatedState(t)
	stdout, _, err := runCLI(t, "", "daemon", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"start", "stop", "status", "logs", "config", "serve"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("`boks daemon --help` does not list %q:\n%s", want, stdout)
		}
	}
}

// Every daemon subcommand rejects positional arguments, so a typo is a usage error rather
// than a silently ignored word.
func TestDaemonSubcommandsTakeNoArguments(t *testing.T) {
	isolatedState(t)
	for _, sub := range []string{"start", "stop", "status", "logs", "config", "serve"} {
		_, _, code := mainExitCode(t, "daemon", sub, "extra")
		if code != 2 {
			t.Errorf("`boks daemon %s extra` exited %d, want 2 for a usage error", sub, code)
		}
	}
}
