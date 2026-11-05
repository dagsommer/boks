package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/policy"
)

// Bare `boks` with files that are not terminals — a pipe, /dev/null — prints the usage and
// exits 2, as it did before there was a dashboard.
func TestBareBoksOffATerminalPrintsUsage(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	for _, stdin := range []*os.File{devnull, r} {
		var errOut bytes.Buffer
		code := Main(context.Background(), Env{Args: []string{}, Stdin: stdin, Stdout: w, Stderr: &errOut})
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
		if !strings.Contains(errOut.String(), "Available Commands:") || !strings.Contains(errOut.String(), "tui") {
			t.Fatalf("usage not printed, or tui missing from it:\n%s", errOut.String())
		}
	}
}

func TestTUICommandRefusesWithoutATerminal(t *testing.T) {
	_, stderr, code := mainExitCode(t, "tui")
	if code != 1 || !strings.Contains(stderr, "needs a terminal") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestDashboardLogEntriesCollapsePerSandboxHostAndOutcome(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	got := logEntries([]policy.Decision{
		{Time: t0, Sandbox: "a", Host: "x.com", Port: 443, Allowed: true},
		{Time: t0.Add(time.Second), Sandbox: "a", Host: "x.com", Port: 80, Allowed: true},
		{Time: t0.Add(2 * time.Second), Sandbox: "a", Host: "x.com", Port: 443, Allowed: false, Rule: "deny x.com"},
		{Time: t0.Add(3 * time.Second), Sandbox: "b", Host: "x.com", Port: 443, Allowed: true},
	})
	if len(got) != 3 {
		t.Fatalf("got %d rows: %+v", len(got), got)
	}
	if got[0].Sandbox != "b" || got[1].Allowed || got[1].Rule != "deny x.com" || got[2].Hits != 2 {
		t.Fatalf("rows not collapsed or not newest first: %+v", got)
	}
}

func TestDashboardRulesKnowWhichAreRemovable(t *testing.T) {
	cases := []struct {
		scope   string
		stored  bool
		sandbox string
	}{
		{"global", true, ""},
		{"sandbox web", true, "web"},
		{"preset standard", false, ""},
		{"agent claude", false, ""},
		{"profile team", false, ""},
	}
	for _, c := range cases {
		r := tuiRule(policy.RuleSpec{Action: policy.Deny, Spec: "x.com", Scope: c.scope})
		if r.Stored != c.stored || r.Sandbox != c.sandbox || r.Allow {
			t.Errorf("%s: %+v", c.scope, r)
		}
	}
	http := tuiRule(policy.RuleSpec{Action: policy.Allow, Spec: "api.x.com", Methods: []string{"get"}, Scope: "global"})
	if !http.IsHTTP() || http.Path != "/**" || http.Methods[0] != "GET" {
		t.Errorf("an HTTP rule shows its canonical selector: %+v", http)
	}
}

func TestDashboardForwardsOnlyTheDevFlagsGiven(t *testing.T) {
	fs := pflag.NewFlagSet("x", pflag.ContinueOnError)
	dev := &devFlags{}
	dev.register(fs)
	if got := devFlagArgs(dev); len(got) != 0 {
		t.Fatalf("no flags given, got %v", got)
	}
	if err := fs.Parse([]string{"--containerd-address", "/tmp/c.sock"}); err != nil {
		t.Fatal(err)
	}
	if got := devFlagArgs(dev); len(got) != 1 || got[0] != "--containerd-address=/tmp/c.sock" {
		t.Fatalf("got %v", got)
	}
}
