package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The check exists so that the command people run when something is unexpected names the
// directory their disk went into, and names the command that gives it back.
func TestStateCheckReportsTheSizeAndTheRemedy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boks")
	if err := os.MkdirAll(filepath.Join(root, "containerd", "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "containerd", "root", "layer"),
		make([]byte, 3*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}

	res := stateCheck().Run(context.Background(), Env{StateDir: root})
	if res.Status != StatusOK {
		t.Errorf("Status = %v, want ok — a disk-space figure is never a reason a host cannot run", res.Status)
	}
	for _, want := range []string{root, "3.0 MiB", "boks purge"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail %q does not mention %q", res.Detail, want)
		}
	}
}

// A host that has never run Boks has no state directory, and that is not a problem to report.
func TestStateCheckOnAHostThatHasNeverRunBoks(t *testing.T) {
	res := stateCheck().Run(context.Background(), Env{StateDir: filepath.Join(t.TempDir(), "boks")})
	if res.Status != StatusOK {
		t.Errorf("Status = %v, want ok for a state directory that does not exist yet", res.Status)
	}
	if !strings.Contains(res.Detail, "not created yet") {
		t.Errorf("detail = %q, want it to say the directory is not there", res.Detail)
	}
}

// doctor reads no global state, so a caller that resolved no state directory gets a skip
// rather than a check that goes and finds the developer's real one.
func TestStateCheckSkipsWithoutAStateDirectory(t *testing.T) {
	res := stateCheck().Run(context.Background(), Env{})
	if res.Status != StatusSkip {
		t.Errorf("Status = %v, want skip when the caller resolved no state directory", res.Status)
	}
}

// The verdict is what a script exits on. A directory Boks cannot reason about must not be
// able to tell somebody their host cannot start sandboxes.
func TestStateCheckNeverFailsTheVerdict(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this host: %v", err)
	}
	// A home directory is a state directory purge refuses outright, which is the worst
	// answer this check can get.
	res := stateCheck().Run(context.Background(), Env{StateDir: home})
	if res.Status == StatusFail || res.Status == StatusUnknown {
		t.Errorf("Status = %v; this check may only ever be ok, warn or skip", res.Status)
	}
	if res.Status == StatusWarn && res.Remedy == "" {
		t.Error("a warning with no remedy")
	}

	report := Report{
		Results: map[string]Result{"state directory": res},
		Order:   []string{"state directory"},
	}
	if !report.Verdict().Ready {
		t.Error("the state directory check made the host report as not ready")
	}
}
