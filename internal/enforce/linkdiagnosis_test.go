package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/proclock"
)

// writeState lays down what a supervisor leaves on disk. held=true takes the same lock a
// running supervisor holds, which is what Lookup reads liveness from — a fabricated PID would
// prove nothing, since Lookup deliberately does not trust one.
func writeSupervisorState(t *testing.T, stateDir, sandbox string, st State, held bool) string {
	t.Helper()
	dir := dirFor(stateDir, sandbox)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if held {
		release, err := proclock.Acquire(filepath.Join(dir, lockFile))
		if err != nil {
			t.Fatalf("holding the supervisor's lock: %v", err)
		}
		t.Cleanup(release)
	}
	return dir
}

// The silent case, and the one that matters most: a healthy stack must not have a network
// explanation attached to a failure that has nothing to do with it.
func TestLinkDiagnosisSaysNothingAboutAHealthyStack(t *testing.T) {
	stateDir := t.TempDir()
	dir := writeSupervisorState(t, stateDir, "s", State{Sandbox: "s", PID: os.Getpid(),
		Socket: filepath.Join(stateDir, "net.sock")}, true)
	if err := os.WriteFile(filepath.Join(stateDir, "net.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = dir
	if got := LinkDiagnosis(stateDir, "s"); got != "" {
		t.Errorf("diagnosed a healthy stack: %s", got)
	}
}

// The failure actually reported from Windows: the supervisor reported ready, then exited and
// took its whole directory with it, and the only surviving symptom was `ttrpc: closed`.
func TestLinkDiagnosisNamesASupervisorThatCleanedUpAndLeft(t *testing.T) {
	stateDir := t.TempDir()
	got := LinkDiagnosis(stateDir, "s")
	if !strings.Contains(got, "gone") || !strings.Contains(got, dirFor(stateDir, "s")) {
		t.Errorf("did not name the missing state directory: %q", got)
	}
}

// A supervisor that died without cleaning up still has its log, which is the one place the
// reason exists. Quoting it is the difference between a diagnosis and a restatement.
func TestLinkDiagnosisQuotesTheLogOfADeadSupervisor(t *testing.T) {
	stateDir := t.TempDir()
	dir := writeSupervisorState(t, stateDir, "s", State{Sandbox: "s", PID: 999999}, false)
	if err := os.WriteFile(filepath.Join(dir, logFile),
		[]byte("boks: the guest never attached\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LinkDiagnosis(stateDir, "s")
	if !strings.Contains(got, "no longer running") {
		t.Errorf("did not report the supervisor as dead: %q", got)
	}
	if !strings.Contains(got, "the guest never attached") {
		t.Errorf("did not quote the log: %q", got)
	}
}

// Alive, but the path the guest connects to is not there. The guest attaches to a socket, not
// to a process, so "the supervisor is running" is not the same answer.
func TestLinkDiagnosisNamesAMissingSocketUnderALiveSupervisor(t *testing.T) {
	stateDir := t.TempDir()
	sock := filepath.Join(stateDir, "vanished.sock")
	writeSupervisorState(t, stateDir, "s", State{Sandbox: "s", PID: os.Getpid(), Socket: sock}, true)
	got := LinkDiagnosis(stateDir, "s")
	if !strings.Contains(got, sock) {
		t.Errorf("did not name the missing socket: %q", got)
	}
}
