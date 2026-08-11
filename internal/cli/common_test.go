package cli

import (
	"bytes"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/sandbox"
)

func TestParseLeadingFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantFirst   string
		wantRest    []string
		wantTTY     bool
		wantCombine bool
	}{
		{"name and command", []string{"web", "ls"}, "web", []string{"ls"}, false, false},
		{
			// Guest flags must reach the guest, not our flag set.
			"guest flags", []string{"-t", "web", "ls", "-l", "-t"},
			"web", []string{"ls", "-l", "-t"}, true, false,
		},
		{"combined shorthand", []string{"-it", "web", "sh"}, "web", []string{"sh"}, false, true},
		{"explicit separator", []string{"web", "--", "git", "status"}, "web", []string{"git", "status"}, false, false},
		{"name only", []string{"web"}, "web", nil, false, false},
		{"nothing", nil, "", nil, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			tty := fs.Bool("t", false, "")
			both := fs.Bool("it", false, "")

			first, rest, err := parseLeadingFlags(fs, tt.args)
			if err != nil {
				t.Fatalf("parseLeadingFlags(%v): %v", tt.args, err)
			}
			if first != tt.wantFirst {
				t.Errorf("first = %q, want %q", first, tt.wantFirst)
			}
			if !slices.Equal(rest, tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
			if *tty != tt.wantTTY {
				t.Errorf("-t = %v, want %v", *tty, tt.wantTTY)
			}
			if *both != tt.wantCombine {
				t.Errorf("-it = %v, want %v", *both, tt.wantCombine)
			}
		})
	}
}

func TestSandboxNameFromWorkspace(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := registerSandboxFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	ws, err := (&sandboxFlags{}).workspaces(t.TempDir())
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	got, err := flags.sandboxName(ws[0])
	if err != nil {
		t.Fatalf("sandboxName: %v", err)
	}
	if want := sandbox.DeriveName(ws[0].HostPath); got != want {
		t.Errorf("sandboxName = %q, want the derived name %q", got, want)
	}
}

// An explicit name is what lets one workspace hold several sandboxes, so it must win over
// the derived one — and be rejected early if containerd would not accept it.
func TestSandboxNameExplicit(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := registerSandboxFlags(fs)
	if err := fs.Parse([]string{"-name", "web"}); err != nil {
		t.Fatal(err)
	}

	ws, err := (&sandboxFlags{}).workspaces(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := flags.sandboxName(ws[0])
	if err != nil {
		t.Fatalf("sandboxName: %v", err)
	}
	if got != "web" {
		t.Errorf("sandboxName = %q, want %q", got, "web")
	}

	bad := flag.NewFlagSet("test", flag.ContinueOnError)
	bad.SetOutput(io.Discard)
	badFlags := registerSandboxFlags(bad)
	if err := bad.Parse([]string{"-name", "not a name"}); err != nil {
		t.Fatal(err)
	}
	if _, err := badFlags.sandboxName(ws[0]); err == nil {
		t.Error("sandboxName accepted a name containerd would reject")
	}
}

// A non-VM runtime must never be presented as a sandbox by accident.
func TestRequireIsolation(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := registerSandboxFlags(fs)
	if err := fs.Parse([]string{"-runtime", "io.containerd.runc.v2"}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := flags.requireIsolation(&stderr); err == nil {
		t.Fatal("requireIsolation allowed a container runtime without the opt-out")
	}

	optOut := flag.NewFlagSet("test", flag.ContinueOnError)
	optOut.SetOutput(io.Discard)
	optOutFlags := registerSandboxFlags(optOut)
	if err := optOut.Parse([]string{"-runtime", "io.containerd.runc.v2", "-i-know-this-is-not-isolated"}); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := optOutFlags.requireIsolation(&stderr); err != nil {
		t.Fatalf("requireIsolation with the opt-out: %v", err)
	}
	if !strings.Contains(stderr.String(), "NOT an isolation boundary") {
		t.Errorf("stderr = %q, want a warning that this is not isolation", stderr.String())
	}
}

func TestHumanAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{500 * time.Millisecond, "0s"},
		{90 * time.Second, "1m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		if got := humanAge(tt.d); got != tt.want {
			t.Errorf("humanAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestWriteTable(t *testing.T) {
	now := time.Now()
	var out bytes.Buffer
	writeTable(&out, []sandbox.Info{{
		Name:       "web",
		Status:     sandbox.StatusRunning,
		Image:      "docker.io/library/alpine:latest",
		Created:    now.Add(-2 * time.Hour),
		Workspaces: []sandbox.WorkspaceRef{{HostPath: "/home/alice/src/foo"}},
	}}, now)

	got := out.String()
	for _, want := range []string{"NAME", "web", "running", "alpine", "/home/alice/src/foo", "2h"} {
		if !strings.Contains(got, want) {
			t.Errorf("table = %q, want it to contain %q", got, want)
		}
	}
}

// A sandbox with no workspace still has to render a row.
func TestWriteTableWithoutWorkspace(t *testing.T) {
	var out bytes.Buffer
	writeTable(&out, []sandbox.Info{{Name: "bare", Status: sandbox.StatusStopped}}, time.Now())
	if !strings.Contains(out.String(), "bare") {
		t.Errorf("table = %q, want a row for the sandbox", out.String())
	}
}
