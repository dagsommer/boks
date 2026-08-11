package sandbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/workspace"
)

// Re-attach depends entirely on the same path producing the same name.
func TestDeriveNameIsStable(t *testing.T) {
	first := DeriveName("/home/alice/src/foo")
	second := DeriveName("/home/alice/src/foo")
	if first != second {
		t.Errorf("DeriveName is not stable: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, NamePrefix) {
		t.Errorf("DeriveName = %q, want the %q prefix", first, NamePrefix)
	}
	if err := ValidateName(first); err != nil {
		t.Errorf("DeriveName produced a name containerd would reject: %v", err)
	}
}

func TestDeriveNameDistinguishesPaths(t *testing.T) {
	paths := []string{
		"/home/alice/src/foo",
		"/home/alice/src/bar",
		"/home/alice/src/foo/",
		"/home/bob/src/foo",
	}
	seen := map[string]string{}
	for _, p := range paths {
		name := DeriveName(p)
		if other, ok := seen[name]; ok {
			t.Errorf("%q and %q both derive %q", p, other, name)
		}
		seen[name] = p
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{"web", "boks-1a2b", "a", "my.sandbox_1", "A-B.c_d"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",                  // nothing to name
		"has space",         // containerd rejects it
		"slash/name",        // would be read as a path by cp
		"-leading",          // separator at the edge
		"trailing-",         //
		"double--separator", //
		strings.Repeat("a", maxNameLength+1),
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
		}
	}
}

// Labels are the only place workspaces are recorded, so a round trip has to be lossless.
func TestWorkspaceLabelRoundTrip(t *testing.T) {
	workspaces := []workspace.Workspace{
		{HostPath: "/a", GuestPath: "/a", Mode: workspace.ModeReadWrite},
		{HostPath: "/b:c", GuestPath: "/b:c", Mode: workspace.ModeReadOnly},
	}
	encoded, err := encodeLabel(workspaceRefs(workspaces))
	if err != nil {
		t.Fatalf("encodeLabel: %v", err)
	}

	got := decodeWorkspaces(map[string]string{LabelWorkspaces: encoded})
	want := []WorkspaceRef{
		{HostPath: "/a", GuestPath: "/a", Mode: "rw"},
		{HostPath: "/b:c", GuestPath: "/b:c", Mode: "ro"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("workspaces = %v, want %v", got, want)
	}
}

func TestCommandLabelRoundTrip(t *testing.T) {
	encoded, err := encodeLabel([]string{"sh", "-lc", "echo hi && exit 0"})
	if err != nil {
		t.Fatalf("encodeLabel: %v", err)
	}
	got := decodeCommand(map[string]string{LabelCommand: encoded})
	if !slices.Equal(got, []string{"sh", "-lc", "echo hi && exit 0"}) {
		t.Errorf("command = %v", got)
	}
}

// A sandbox created by an older or newer Boks may not carry the labels this one expects.
// Missing or corrupt metadata must degrade to "unknown", never to a failure.
func TestLabelDecodingToleratesGarbage(t *testing.T) {
	labels := map[string]string{LabelWorkspaces: "{not json", LabelCommand: "[1,2"}
	if got := decodeWorkspaces(labels); len(got) != 0 {
		t.Errorf("workspaces = %v, want none", got)
	}
	if got := decodeCommand(labels); len(got) != 0 {
		t.Errorf("command = %v, want none", got)
	}
	if got := decodeWorkspaces(nil); len(got) != 0 {
		t.Errorf("workspaces = %v, want none", got)
	}
}

func TestInfoWorkspace(t *testing.T) {
	info := Info{Workspaces: []WorkspaceRef{{HostPath: "/first"}, {HostPath: "/second"}}}
	if got := info.Workspace(); got != "/first" {
		t.Errorf("Workspace() = %q, want the primary workspace", got)
	}
	if got := (Info{}).Workspace(); got != "" {
		t.Errorf("Workspace() = %q, want empty for a sandbox with none", got)
	}
}
