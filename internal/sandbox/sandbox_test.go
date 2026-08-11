package sandbox

import (
	"slices"
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
