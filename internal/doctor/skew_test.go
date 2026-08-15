package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/runtimecfg"
)

func skewEnv() Env {
	return Env{
		ContainerdAddress: filepath.Join(os.TempDir(), "boks-absent-containerd.sock"),
		Runtime:           runtimecfg.Runtime,
		Snapshotter:       runtimecfg.Snapshotter,
	}
}

// With no shim there is nothing to compare, and the `vm runtime` line already reports that.
// Saying it twice would make the report longer without making it truer.
func TestRuntimeSkewSkipsWithNoShim(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BOKS_RUNTIME_DIR", filepath.Join(t.TempDir(), "absent"))

	res := runtimeSkewCheck().Run(t.Context(), skewEnv())
	if res.Status != StatusSkip {
		t.Errorf("with no shim, runtime skew = %s (%s), want skip", res.Status, res.Detail)
	}
}

// A shim carrying no Go build information is a real state — a stripped binary, or one not
// built from Go — and it must be reported as "cannot tell" rather than as a pass. A pass here
// would be doctor claiming the set is compatible on the evidence of nothing.
func TestRuntimeSkewWarnsOnAShimItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake shim here is a shell script")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, runtimecfg.ShimBinary(runtimecfg.Runtime))
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BOKS_RUNTIME_DIR", filepath.Join(t.TempDir(), "absent"))

	res := runtimeSkewCheck().Run(t.Context(), skewEnv())
	if res.Status != StatusWarn {
		t.Fatalf("an unreadable shim gave %s (%s), want warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Remedy, "Yunix") {
		t.Errorf("the remedy does not say what the unchecked failure looks like:\n%s", res.Remedy)
	}
}

// A readable shim with no containerd to compare it against is a skip, not a pass: the daemon's
// version is half the comparison and doctor has neither half without it.
func TestRuntimeSkewSkipsWithNoContainerd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe binary here is built for the host")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, runtimecfg.ShimBinary(runtimecfg.Runtime))
	buildProbe(t, shim)

	t.Setenv("PATH", dir)
	t.Setenv("BOKS_RUNTIME_DIR", filepath.Join(t.TempDir(), "absent"))

	res := runtimeSkewCheck().Run(t.Context(), skewEnv())
	if res.Status != StatusSkip {
		t.Errorf("with no containerd, runtime skew = %s (%s), want skip", res.Status, res.Detail)
	}
}

// A skew is a hard failure rather than a warning, because the sandbox will not start: the
// verdict at the bottom of the report has to say the host is not ready.
func TestRuntimeSkewFailsTheVerdict(t *testing.T) {
	report := Report{
		Order:   []string{"runtime skew"},
		Results: map[string]Result{"runtime skew": {Status: StatusFail, Detail: "older than the shim"}},
	}
	verdict := report.Verdict()
	if verdict.Ready {
		t.Error("a host with a version-skewed runtime was reported ready")
	}
	if len(verdict.Failures) != 1 || verdict.Failures[0] != "runtime skew" {
		t.Errorf("the verdict does not name the skew: %v", verdict.Failures)
	}
}
