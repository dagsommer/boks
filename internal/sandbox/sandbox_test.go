package sandbox

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/dagsommer/boks/internal/workspace"
)

// The mount destination is what makes a workspace appear at its host path inside the
// guest. If this drifts, absolute paths silently stop matching.
func TestWorkspaceMountsPreserveHostPath(t *testing.T) {
	ws := workspace.Workspace{
		HostPath:  "/home/alice/src/foo",
		GuestPath: "/home/alice/src/foo",
		Mode:      workspace.ModeReadWrite,
	}

	mounts := workspaceMounts([]workspace.Workspace{ws})
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	m := mounts[0]
	if m.Source != "/home/alice/src/foo" {
		t.Errorf("Source = %q, want the host path", m.Source)
	}
	if m.Destination != "/home/alice/src/foo" {
		t.Errorf("Destination = %q, want it to equal the host path", m.Destination)
	}
	if m.Type != "bind" {
		t.Errorf("Type = %q, want \"bind\"", m.Type)
	}
	if !slices.Contains(m.Options, "rw") {
		t.Errorf("Options = %v, want \"rw\"", m.Options)
	}
}

func TestWorkspaceMountsReadOnly(t *testing.T) {
	ws := workspace.Workspace{HostPath: "/data", GuestPath: "/data", Mode: workspace.ModeReadOnly}

	mounts := workspaceMounts([]workspace.Workspace{ws})
	if !slices.Contains(mounts[0].Options, "ro") {
		t.Errorf("Options = %v, want \"ro\"", mounts[0].Options)
	}
}

func TestWorkspaceMountsMultiple(t *testing.T) {
	mounts := workspaceMounts([]workspace.Workspace{
		{HostPath: "/a", GuestPath: "/a", Mode: workspace.ModeReadWrite},
		{HostPath: "/b", GuestPath: "/b", Mode: workspace.ModeReadOnly},
	})
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(mounts))
	}
	// Order matters: the first workspace is the process's working directory.
	if mounts[0].Destination != "/a" || mounts[1].Destination != "/b" {
		t.Errorf("destinations = %q, %q; want /a, /b in order",
			mounts[0].Destination, mounts[1].Destination)
	}
}

func TestWorkspaceMountsEmpty(t *testing.T) {
	if got := workspaceMounts(nil); len(got) != 0 {
		t.Errorf("got %d mounts for no workspaces, want 0", len(got))
	}
}

func TestResourceAnnotations(t *testing.T) {
	got := resourceAnnotations(Config{CPUs: 4, MemoryMiB: 8192})
	if got[annotationCPU] != "4" {
		t.Errorf("%s = %q, want \"4\"", annotationCPU, got[annotationCPU])
	}
	if got[annotationMemory] != "8192" {
		t.Errorf("%s = %q, want \"8192\"", annotationMemory, got[annotationMemory])
	}

	// Unset values must be omitted so the runtime applies its own defaults rather
	// than being handed a zero.
	empty := resourceAnnotations(Config{})
	if len(empty) != 0 {
		t.Errorf("annotations = %v, want none when resources are unset", empty)
	}
}

// Regression: the shim guidance used to be chosen by looking the shim up on Boks' own PATH,
// which containerd does not use. Any task failure — here a runtime annotation the shim
// rejected — was then blamed on a missing shim, and the real cause, printed directly above
// it, was contradicted by the advice.
func TestDescribeTaskErrorKeepsUnrelatedFailuresIntact(t *testing.T) {
	cfg := Config{
		Name:    "boks-test",
		Image:   "docker.io/library/alpine:latest",
		Runtime: "io.containerd.nerdbox.v1",
		Command: []string{"sh"},
	}
	underlying := errors.New("failed to create shim task: failed to parse network annotation: invalid port mapping \"8080\"")

	got := describeTaskError(cfg, underlying).Error()
	if strings.Contains(got, "boks doctor") || strings.Contains(got, "daemon's PATH") {
		t.Errorf("unrelated failure was reported as a missing shim:\n%s", got)
	}
	if !strings.Contains(got, "failed to parse network annotation") {
		t.Errorf("the real cause was dropped:\n%s", got)
	}
}

func TestDescribeTaskErrorNamesTheMissingShim(t *testing.T) {
	cfg := Config{Runtime: "io.containerd.nerdbox.v1", Command: []string{"sh"}}
	underlying := errors.New(`failed to start shim: exec: "containerd-shim-nerdbox-v1": executable file not found in $PATH`)

	got := describeTaskError(cfg, underlying).Error()
	if !strings.Contains(got, "containerd-shim-nerdbox-v1") || !strings.Contains(got, "boks doctor") {
		t.Errorf("a missing shim was not explained:\n%s", got)
	}
	// Boks cannot see containerd's PATH, so it must not claim to know the binary is absent.
	if strings.Contains(got, "That binary was not found") {
		t.Errorf("the message asserts something Boks cannot observe:\n%s", got)
	}
	// "sh" is a substring of the shim binary's name; the guest command must not be blamed.
	if strings.Contains(got, "inside the guest image") {
		t.Errorf("a missing shim was misreported as a missing guest command:\n%s", got)
	}
}

func TestDescribeTaskErrorNamesTheMissingGuestCommand(t *testing.T) {
	cfg := Config{Image: "docker.io/library/alpine:latest", Runtime: "io.containerd.runc.v2", Command: []string{"nosuchcommand"}}
	underlying := errors.New(`failed to create containerd task: exec: "nosuchcommand": executable file not found in $PATH`)

	got := describeTaskError(cfg, underlying).Error()
	if !strings.Contains(got, "nosuchcommand") || !strings.Contains(got, "docker.io/library/alpine:latest") {
		t.Errorf("a missing guest command was not explained:\n%s", got)
	}
}

// An interrupted run reports the shell's 128+signal convention rather than a generic
// failure. This regressed once already: a refactor routed `run` through Exec and the
// mapping did not follow, so Ctrl-C started printing a raw gRPC cancellation again.
func TestInterruptedExit(t *testing.T) {
	var none atomic.Int32
	if got := interruptedExit(&none); got != 0 {
		t.Errorf("interruptedExit(no signal) = %d, want 0 so the real exit code is used", got)
	}

	for _, tt := range []struct {
		sig  syscall.Signal
		want int
	}{
		{syscall.SIGINT, 130},
		{syscall.SIGTERM, 143},
	} {
		var received atomic.Int32
		received.Store(int32(tt.sig))
		if got := interruptedExit(&received); got != tt.want {
			t.Errorf("interruptedExit(%v) = %d, want %d", tt.sig, got, tt.want)
		}
	}
}

// A workspace is a live mount of a host directory, so inside the guest it is owned by the
// host user's uid and the process is not that user. Git refuses to touch such a repository,
// which is the first thing a coding agent does — and `git diff` fails as "Not a git
// repository", which an agent may act on.
func TestGitSafeDirectoryEnv(t *testing.T) {
	env := gitSafeDirectoryEnv([]workspace.Workspace{
		{GuestPath: "/private/tmp/one"},
		{GuestPath: "/home/alice/two"},
	})

	want := []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=/private/tmp/one",
		"GIT_CONFIG_KEY_1=safe.directory",
		"GIT_CONFIG_VALUE_1=/home/alice/two",
	}
	if !slices.Equal(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}

	// The count must match the entries, or git reads past the end and ignores the lot.
	if got := len(env); got != 1+2*2 {
		t.Errorf("%d entries for 2 workspaces, want 5", got)
	}
	if len(gitSafeDirectoryEnv(nil)) != 0 {
		t.Error("no workspaces should produce no configuration, not an empty count")
	}
}
