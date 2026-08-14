package doctor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportReadyAndFailures(t *testing.T) {
	report := Report{
		Order: []string{"a", "b", "c"},
		Results: map[string]Result{
			"a": {Status: StatusOK},
			"b": {Status: StatusWarn},
			"c": {Status: StatusOK},
		},
	}
	if !report.Ready() {
		t.Error("Ready() = false; warnings alone must not block sandboxes")
	}
	if got := report.Failures(); len(got) != 0 {
		t.Errorf("Failures() = %v, want none", got)
	}

	report.Results["b"] = Result{Status: StatusFail}
	if report.Ready() {
		t.Error("Ready() = true despite a failing check")
	}
	if got := report.Failures(); len(got) != 1 || got[0] != "b" {
		t.Errorf("Failures() = %v, want [b]", got)
	}
}

func TestReportWriteIncludesRemedies(t *testing.T) {
	report := Report{
		Order: []string{"virtualization", "containerd"},
		Results: map[string]Result{
			"virtualization": {Status: StatusFail, Detail: "missing", Remedy: "enable nested virtualisation"},
			"containerd":     {Status: StatusOK, Detail: "v2.2.6"},
		},
	}
	var sb strings.Builder
	report.Write(&sb)
	out := sb.String()

	for _, want := range []string{"virtualization", "fail", "missing", "enable nested virtualisation", "Not ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// A passing check has nothing to remediate, so it must not add noise.
	if strings.Contains(out, "containerd (ok)") {
		t.Errorf("output includes a remedy block for a passing check:\n%s", out)
	}
}

func TestReportWriteSaysReadyWhenClean(t *testing.T) {
	report := Report{
		Order:   []string{"platform"},
		Results: map[string]Result{"platform": {Status: StatusOK, Detail: "linux/arm64"}},
	}
	var sb strings.Builder
	report.Write(&sb)
	if !strings.Contains(sb.String(), "ready") {
		t.Errorf("output does not report readiness:\n%s", sb.String())
	}
}

// Every check must produce a result and explain itself when it is not satisfied, so that
// doctor is never a bare "fail" the user cannot act on.
func TestChecksAlwaysExplainFailures(t *testing.T) {
	env := Env{
		ContainerdAddress: "/nonexistent/containerd.sock",
		Runtime:           "io.containerd.nerdbox.v1",
		Snapshotter:       "erofs",
	}
	report := Run(context.Background(), env)

	if len(report.Order) == 0 {
		t.Fatal("Run produced no checks")
	}
	for _, name := range report.Order {
		res, ok := report.Results[name]
		if !ok {
			t.Errorf("check %q produced no result", name)
			continue
		}
		if res.Status == StatusFail && res.Remedy == "" {
			t.Errorf("check %q failed without a remedy", name)
		}
	}
}

// An unreachable containerd must be reported as such, not as a crash.
func TestContainerdCheckHandlesMissingSocket(t *testing.T) {
	res := containerdCheck().Run(context.Background(), Env{
		ContainerdAddress: "/nonexistent/containerd.sock",
	})
	if res.Status != StatusFail {
		t.Errorf("Status = %v, want fail for a missing socket", res.Status)
	}
	if res.Remedy == "" {
		t.Error("no remedy offered for a missing containerd socket")
	}
}

// A Windows named pipe is not a socket, and telling someone their "containerd socket" is
// missing when the address is \\.\pipe\containerd-containerd names a thing that does not
// exist on their machine. The address decides the noun, so this is testable anywhere.
func TestContainerdFailureNamesTheEndpointCorrectly(t *testing.T) {
	tests := []struct {
		address string
		want    string
	}{
		{`\\.\pipe\containerd-containerd`, "named pipe"},
		{`npipe:////./pipe/containerd-containerd`, "named pipe"},
		{`//./pipe/containerd-containerd`, "named pipe"},
		{"/run/containerd/containerd.sock", "socket"},
		{"/var/run/containerd/containerd.sock", "socket"},
		{"/home/x/pipe/containerd.sock", "socket"},
	}
	for _, tt := range tests {
		if got := containerdEndpointNoun(tt.address); got != tt.want {
			t.Errorf("containerdEndpointNoun(%q) = %q, want %q", tt.address, got, tt.want)
		}
	}

	// The message the user actually reads.
	res := containerdFailure(`\\.\pipe\containerd-containerd`, errors.New("no such file"))
	if !strings.Contains(res.Remedy, "No containerd named pipe at") {
		t.Errorf("remedy calls a named pipe something else:\n%s", res.Remedy)
	}
	res = containerdFailure(filepath.Join(t.TempDir(), "containerd.sock"), errors.New("no such file"))
	if !strings.Contains(res.Remedy, "No containerd socket at") {
		t.Errorf("remedy does not call a Unix socket a socket:\n%s", res.Remedy)
	}
}

// Checks are assembled from a shared set plus per-platform additions; a malformed entry
// would panic at run time rather than fail visibly.
func TestChecksAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Checks() {
		if c.Name == "" {
			t.Error("a check has an empty name")
		}
		if c.Run == nil {
			t.Errorf("check %q has no Run function", c.Name)
		}
		if seen[c.Name] {
			t.Errorf("duplicate check name %q; names are the display key", c.Name)
		}
		seen[c.Name] = true
	}
	for _, required := range []string{"platform", "virtualization", "containerd", "vm runtime"} {
		if !seen[required] {
			t.Errorf("check %q is missing", required)
		}
	}
}

func TestStatusString(t *testing.T) {
	for status, want := range map[Status]string{
		StatusOK: "ok", StatusWarn: "warn", StatusFail: "fail", StatusSkip: "skip",
	} {
		if got := status.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", status, got, want)
		}
	}
}
