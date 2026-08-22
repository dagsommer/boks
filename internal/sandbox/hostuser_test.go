package sandbox

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/dagsommer/boks/internal/workspace"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func applyHostUser(t *testing.T, cfg Config, start specs.User) specs.User {
	t.Helper()
	s := &specs.Spec{Process: &specs.Process{User: start}}
	if err := withHostUser(cfg)(context.Background(), nil, nil, s); err != nil {
		t.Fatalf("withHostUser: %v", err)
	}
	return s.Process.User
}

// With a workspace shared, the process must run as the uid that owns the host files. Anything
// else cannot write them: the guest kernel checks the real uid a virtiofs file reports, and
// libkrun offers no id mapping to reconcile the two.
func TestHostUserAppliesWhenAWorkspaceIsShared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no uid semantics")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root, where the override deliberately stays out of the way")
	}
	cfg := Config{Workspaces: []workspace.Workspace{{HostPath: "/tmp/x", GuestPath: "/tmp/x"}}}

	// Starting from the image's user, which is what the image config just set.
	got := applyHostUser(t, cfg, specs.User{UID: 1000, GID: 1000, AdditionalGids: []uint32{1000, 27}})
	if got.UID != uint32(os.Getuid()) || got.GID != uint32(os.Getgid()) {
		t.Errorf("User = %d:%d, want the host's %d:%d", got.UID, got.GID, os.Getuid(), os.Getgid())
	}
	// The image's supplementary groups belonged to a user this no longer is.
	if len(got.AdditionalGids) != 1 || got.AdditionalGids[0] != uint32(os.Getgid()) {
		t.Errorf("AdditionalGids = %v, want just the new primary group", got.AdditionalGids)
	}
}

// With nothing shared from the host there is no ownership to agree with, and the image's own
// user is the right answer. Overriding it anyway would change what every sandbox runs as for
// no reason.
func TestHostUserLeavesAWorkspacelessSandboxAlone(t *testing.T) {
	// A uid nothing on any machine runs as, so "untouched" is distinguishable from
	// "overridden with the host's". The first version of this test started from 1000 —
	// which is what this project's own CI and containers run as — so dropping the
	// workspace check entirely still produced 1000 and the test passed. It asserted
	// nothing, and a mutation proved it.
	const notAnyHostUID = 4242
	before := specs.User{UID: notAnyHostUID, GID: notAnyHostUID, AdditionalGids: []uint32{notAnyHostUID}}

	got := applyHostUser(t, Config{}, before)
	if got.UID != notAnyHostUID || got.GID != notAnyHostUID {
		t.Errorf("User = %d:%d, want the image's %d untouched", got.UID, got.GID, notAnyHostUID)
	}
	if len(got.AdditionalGids) != 1 || got.AdditionalGids[0] != notAnyHostUID {
		t.Errorf("AdditionalGids = %v, want the image's untouched", got.AdditionalGids)
	}
}

// A spec that asks for uid 0 is the state this replaced, not a state to arrive at: it is how
// agents came to leave root-owned files in people's repositories. If Boks itself is running as
// root the override stays out, rather than propagating that to the guest.
func TestHostUserNeverAsksForRoot(t *testing.T) {
	cfg := Config{Workspaces: []workspace.Workspace{{HostPath: "/tmp/x", GuestPath: "/tmp/x"}}}
	got := applyHostUser(t, cfg, specs.User{UID: 1000, GID: 1000})
	if got.UID == 0 {
		t.Errorf("the guest was told to run as root (host uid is %d)", os.Getuid())
	}
}
