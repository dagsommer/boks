package doctor

import (
	"context"
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

func TestStatusString(t *testing.T) {
	for status, want := range map[Status]string{
		StatusOK: "ok", StatusWarn: "warn", StatusFail: "fail", StatusSkip: "skip",
	} {
		if got := status.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", status, got, want)
		}
	}
}
