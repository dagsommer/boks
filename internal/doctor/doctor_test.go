package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// A check that is not satisfied must count towards the verdict however the report is
// assembled, and the closing line must name it.
//
// This is the regression test for a `boks doctor` that exited 0 under its own "Not ready"
// summary. The exit status and the summary were two separate traversals of one report —
// Ready() over the results map, the summary over the display order — so a check that was in
// one collection and not the other was counted by one and silently dropped by the other. Two
// of these three shapes were reported as a ready host before Verdict became the only place
// that decides.
func TestReportCountsEveryUnsatisfiedCheck(t *testing.T) {
	tests := []struct {
		name   string
		report Report
	}{
		{
			name: "a failing check",
			report: Report{
				Order:   []string{"containerd", "virtualization"},
				Results: map[string]Result{"containerd": {Status: StatusOK}, "virtualization": {Status: StatusFail}},
			},
		},
		{
			// StatusOK used to be the zero value, so a name with nothing recorded
			// against it read as a satisfied requirement.
			name: "a check with no result recorded",
			report: Report{
				Order:   []string{"containerd", "virtualization"},
				Results: map[string]Result{"containerd": {Status: StatusOK}},
			},
		},
		{
			// A failure the table never lists still keeps a sandbox from starting,
			// and a summary that cannot name it is not a summary.
			name: "a failure outside the display order",
			report: Report{
				Order:   []string{"containerd"},
				Results: map[string]Result{"containerd": {Status: StatusOK}, "virtualization": {Status: StatusFail}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.report.Ready() {
				t.Error("Ready() = true; a host that cannot start a sandbox was reported as able to")
			}
			if got := tt.report.Failures(); len(got) != 1 || got[0] != "virtualization" {
				t.Errorf("Failures() = %v, want [virtualization]", got)
			}
			var sb strings.Builder
			tt.report.Write(&sb)
			out := sb.String()
			if strings.Contains(out, "Host looks ready") {
				t.Errorf("the summary calls the host ready:\n%s", out)
			}
			if !strings.Contains(out, "Not ready: virtualization must be fixed") {
				t.Errorf("the summary does not name the failing check:\n%s", out)
			}
		})
	}
}

// The zero Result must not read as a passing check.
//
// This pins the order of the Status constants, which is load-bearing rather than cosmetic.
// StatusOK was the zero value, so a Result nobody filled in — a check that recorded nothing,
// a map entry defaulted into existence by a lookup — arrived as a satisfied requirement, and
// a satisfied requirement is one the exit status does not count. That is precisely how a
// host that cannot start a sandbox comes to be reported as ready. Moving StatusOK off zero
// is what makes "nothing was said about this" and "this is fine" different values; without
// this test nothing would notice them becoming the same again.
func TestTheZeroResultIsNotAPass(t *testing.T) {
	var nothingSaid Result
	if nothingSaid.Status == StatusOK {
		t.Error("the zero Status is StatusOK; a check that reported nothing now reads as a check that passed")
	}
	if got := nothingSaid.Status.String(); got != "unknown" {
		t.Errorf("the zero Status prints as %q, want \"unknown\"", got)
	}

	report := Report{
		Order:   []string{"containerd", "virtualization"},
		Results: map[string]Result{"containerd": {Status: StatusOK}, "virtualization": {}},
	}
	if report.Ready() {
		t.Error("Ready() = true with a check that said nothing; a host was called ready by omission")
	}
	if got := report.Failures(); len(got) != 1 || got[0] != "virtualization" {
		t.Errorf("Failures() = %v, want [virtualization]", got)
	}
	var sb strings.Builder
	verdict := report.Write(&sb)
	if verdict.Ready {
		t.Errorf("the printed verdict calls the host ready:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "Not ready: virtualization must be fixed") {
		t.Errorf("the summary does not name the check that said nothing:\n%s", sb.String())
	}
}

// The exit status and the printed summary are one decision, taken once. Write hands back the
// verdict it printed so a caller cannot ask a second question and get a second answer.
func TestWriteReturnsTheVerdictItPrinted(t *testing.T) {
	tests := []struct {
		name      string
		report    Report
		wantReady bool
	}{
		{
			name: "ready",
			report: Report{
				Order:   []string{"platform"},
				Results: map[string]Result{"platform": {Status: StatusOK, Detail: "linux/arm64"}},
			},
			wantReady: true,
		},
		{
			name: "not ready",
			report: Report{
				Order:   []string{"platform", "virtualization"},
				Results: map[string]Result{"platform": {Status: StatusFail}, "virtualization": {Status: StatusFail}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			verdict := tt.report.Write(&sb)
			if verdict.Ready != tt.wantReady {
				t.Errorf("Verdict.Ready = %v, want %v", verdict.Ready, tt.wantReady)
			}
			if verdict.Ready != tt.report.Ready() {
				t.Errorf("Write's verdict (%v) disagrees with Ready() (%v)", verdict.Ready, tt.report.Ready())
			}
			out := strings.TrimRight(sb.String(), "\n")
			lines := strings.Split(out, "\n")
			if last := lines[len(lines)-1]; last != verdict.Summary {
				t.Errorf("the printed summary %q is not the verdict's %q", last, verdict.Summary)
			}
		})
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
	for _, required := range []string{"platform", "virtualization", "containerd", "vm runtime", "guest image"} {
		if !seen[required] {
			t.Errorf("check %q is missing", required)
		}
	}
}

// The guest image checks below drive guestImageResult with directories the test creates, not
// the host's search path: the point of the check is to report a machine that lacks the real
// files, so a test that needed them present could only ever run on a machine it cannot assume.

func TestGuestImageResultFindsKernelAndRootfs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, guestKernelName()))
	writeFile(t, filepath.Join(dir, "nerdbox-rootfs.erofs"))

	res := guestImageResult([]string{dir})
	if res.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want ok with both files present", res.Status, res.Detail)
	}
	for _, want := range []string{guestKernelName(), "nerdbox-rootfs.erofs"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("Detail = %q, want it to name %s", res.Detail, want)
		}
	}
}

// The shim tries an arch-suffixed rootfs before the unsuffixed one, so both must satisfy the
// check; accepting only the name nerdbox's own bake writes would fail a working host.
func TestGuestImageResultAcceptsArchSuffixedRootfs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, guestKernelName()))
	writeFile(t, filepath.Join(dir, "nerdbox-rootfs-"+guestArch()+".erofs"))

	if res := guestImageResult([]string{dir}); res.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want ok for an arch-suffixed rootfs", res.Status, res.Detail)
	}
}

func TestGuestImageResultNamesWhatIsMissing(t *testing.T) {
	empty := t.TempDir()
	withKernel := t.TempDir()
	writeFile(t, filepath.Join(withKernel, guestKernelName()))

	for _, tc := range []struct {
		name    string
		dirs    []string
		missing []string
		present []string
	}{
		{name: "neither", dirs: []string{empty}, missing: []string{guestKernelName(), "nerdbox-rootfs.erofs"}},
		{
			name:    "rootfs only",
			dirs:    []string{withKernel},
			missing: []string{"nerdbox-rootfs.erofs"},
			present: []string{guestKernelName()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := guestImageResult(tc.dirs)
			if res.Status != StatusFail {
				t.Fatalf("Status = %v, want fail: the VM cannot boot without these", res.Status)
			}
			for _, want := range tc.missing {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("Detail = %q, want it to name the missing %s", res.Detail, want)
				}
			}
			// A file that is present must not be reported as missing, or the user goes
			// looking for something they already have.
			for _, unwanted := range tc.present {
				if strings.Contains(res.Detail, unwanted) {
					t.Errorf("Detail = %q reports %s missing, but it is present", res.Detail, unwanted)
				}
			}
			if res.Remedy == "" {
				t.Fatal("no remedy offered for a missing guest image")
			}
			// The remedy is only actionable if it says how to obtain the files and where
			// to put them; both are non-obvious and neither is packaged anywhere.
			for _, want := range []string{"scripts/build-nerdbox-guest.sh", "LIBKRUN_PATH"} {
				if !strings.Contains(res.Remedy, want) {
					t.Errorf("remedy does not mention %q:\n%s", want, res.Remedy)
				}
			}
		})
	}
}

// The search must be the shim's, since a check that looks elsewhere reports on a machine that
// does not exist. nerdbox scans PATH first, then LIBKRUN_PATH.
func TestNerdboxSearchPathsScansPATHThenLIBKRUNPATH(t *testing.T) {
	onPath := t.TempDir()
	onLibkrunPath := t.TempDir()
	t.Setenv("PATH", onPath)
	t.Setenv("LIBKRUN_PATH", onLibkrunPath)

	dirs := nerdboxSearchPaths(runtime.GOOS, os.Getenv)
	pathIdx, libkrunIdx := indexOfDir(dirs, onPath), indexOfDir(dirs, onLibkrunPath)
	if pathIdx < 0 {
		t.Errorf("PATH entry %q missing from the search: %v", onPath, dirs)
	}
	if libkrunIdx < 0 {
		t.Errorf("LIBKRUN_PATH entry %q missing from the search: %v", onLibkrunPath, dirs)
	}
	if pathIdx >= 0 && libkrunIdx >= 0 && pathIdx > libkrunIdx {
		t.Errorf("LIBKRUN_PATH is searched before PATH; the shim does the opposite: %v", dirs)
	}
}

// An empty PATH element means "." to the shim, which is how a guest image in the working
// directory is found at all. Dropping it, as splitList does, would hide that.
func TestNerdboxSearchPathsMapsEmptyElementToDot(t *testing.T) {
	t.Setenv("PATH", string(os.PathListSeparator)+t.TempDir())
	t.Setenv("LIBKRUN_PATH", t.TempDir())

	if indexOfDir(nerdboxSearchPaths(runtime.GOOS, os.Getenv), ".") < 0 {
		t.Errorf("an empty PATH element was not searched as \".\": %v", nerdboxSearchPaths(runtime.GOOS, os.Getenv))
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("test fixture"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func indexOfDir(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
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
