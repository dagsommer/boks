package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/doctor"
)

// `boks doctor` exists to be believed by a script. Its exit status must therefore say the
// same thing as the line the user reads, for every shape of report — including the shapes
// that should be impossible, because a host that cannot start a sandbox being reported as
// able to is the one outcome this command must never produce.
func TestDoctorExitStatusMatchesItsSummary(t *testing.T) {
	tests := []struct {
		name   string
		report doctor.Report
		want   int
	}{
		{
			name: "a clean host",
			report: doctor.Report{
				Order:   []string{"platform"},
				Results: map[string]doctor.Result{"platform": {Status: doctor.StatusOK}},
			},
			want: 0,
		},
		{
			name: "warnings and skips do not block",
			report: doctor.Report{
				Order: []string{"hypervisor library", "vm runtime"},
				Results: map[string]doctor.Result{
					"hypervisor library": {Status: doctor.StatusWarn, Remedy: "install libkrun"},
					"vm runtime":         {Status: doctor.StatusSkip},
				},
			},
			want: 0,
		},
		{
			// The Windows report that prompted this: most checks pass once
			// containerd is up, and the two that cannot pass at all are the ones
			// that decide whether a sandbox can start.
			name: "failing checks among passing ones",
			report: doctor.Report{
				Order: []string{"platform", "virtualization", "containerd", "vm runtime"},
				Results: map[string]doctor.Result{
					"platform":       {Status: doctor.StatusFail, Remedy: "not on Windows yet"},
					"virtualization": {Status: doctor.StatusFail, Remedy: "no VM backend"},
					"containerd":     {Status: doctor.StatusOK},
					"vm runtime":     {Status: doctor.StatusOK},
				},
			},
			want: 1,
		},
		{
			name: "a check that recorded nothing",
			report: doctor.Report{
				Order:   []string{"platform", "virtualization"},
				Results: map[string]doctor.Result{"platform": {Status: doctor.StatusOK}},
			},
			want: 1,
		},
		{
			// A result nobody filled in is not a requirement that was met, and
			// must not become an exit status that says it was.
			name: "a result that says nothing",
			report: doctor.Report{
				Order:   []string{"platform", "virtualization"},
				Results: map[string]doctor.Result{"platform": {Status: doctor.StatusOK}, "virtualization": {}},
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportHealth(&out, tt.report)

			code := 0
			var exit *ExitError
			if errors.As(err, &exit) {
				code = exit.Code
			} else if err != nil {
				t.Fatalf("reportHealth returned an unexpected error: %v", err)
			}
			if code != tt.want {
				t.Errorf("exit code = %d, want %d; output:\n%s", code, tt.want, out.String())
			}

			// The point is not the number on its own but that it cannot contradict
			// the text printed above it.
			notReady := strings.Contains(out.String(), "Not ready")
			if notReady != (code != 0) {
				t.Errorf("the summary and the exit code disagree: exit %d under:\n%s", code, out.String())
			}
		})
	}
}

// The same invariant through the real command and the real process exit code, against
// whatever this host happens to be.
func TestDoctorCommandExitCodeAgreesWithItsReport(t *testing.T) {
	// A socket that cannot exist, so the run says something about a host rather than
	// depending on the containerd of whoever is running the tests.
	stdout, _, code := mainExitCode(t, "doctor", "--containerd-address", "/nonexistent/boks-doctor-test.sock")

	switch {
	case strings.Contains(stdout, "Not ready"):
		if code == 0 {
			t.Errorf("boks doctor exited 0 under its own \"Not ready\" summary:\n%s", stdout)
		}
	case strings.Contains(stdout, "Host looks ready"):
		if code != 0 {
			t.Errorf("boks doctor exited %d while calling the host ready:\n%s", code, stdout)
		}
	default:
		t.Errorf("boks doctor printed no verdict:\n%s", stdout)
	}
}
