package sandbox

import (
	"context"
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/dagsommer/boks/internal/workspace"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// applyHostUser runs withHostUser as the override path, whatever this machine's shim can do.
// The probe behind guestSupportsIdmappedMounts runs the real installed shim, so without
// pinning it these tests asserted the override on hosts with an unpatched shim and failed on
// hosts with a patched one. The idmap branch has its own tests, which call
// reconcileWorkspaceIdentity with the capability as a parameter.
func applyHostUser(t *testing.T, cfg Config, start specs.User) specs.User {
	t.Helper()
	probe := guestSupportsIdmappedMounts
	guestSupportsIdmappedMounts = func() bool { return false }
	t.Cleanup(func() { guestSupportsIdmappedMounts = probe })
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

// reconcileWorkspaceIdentity is withHostUser's decision, taking the guest's ability to idmap
// as a parameter so both branches can be exercised without a real shim on disk — the same
// idiom usesMetadataImageConfigOn uses in imageconfig.go. These tests build the *.Spec that
// withHostUser would already have (a Process with the image's resolved user, Mounts already
// populated by workspaceMounts) and call the decision directly.

// The point of the idmap mechanism: the image's own user keeps running unchanged, and only
// the mount gains a mapping translating the host's uid to it.
func TestReconcileWorkspaceIdentityIdmapsANonRootImageWhenTheGuestCan(t *testing.T) {
	ws := workspace.Workspace{HostPath: "/tmp/x", GuestPath: "/tmp/x"}
	s := &specs.Spec{
		Process: &specs.Process{User: specs.User{UID: 1000, GID: 1000, AdditionalGids: []uint32{1000, 27}}},
		Mounts:  []specs.Mount{{Type: "bind", Source: ws.HostPath, Destination: ws.GuestPath}},
	}

	reconcileWorkspaceIdentity(s, []workspace.Workspace{ws}, 501, 20, true)

	if s.Process.User.UID != 1000 || s.Process.User.GID != 1000 {
		t.Errorf("Process.User = %d:%d, want the image's 1000:1000 untouched", s.Process.User.UID, s.Process.User.GID)
	}
	if len(s.Process.User.AdditionalGids) != 2 {
		t.Errorf("AdditionalGids = %v, want the image's untouched", s.Process.User.AdditionalGids)
	}
	// ContainerID is the on-disk id and HostID the id the container sees: the kernel's
	// idmap userns direction, the reverse of what the field names suggest. Inverting
	// these made every file read as 65534 in a real guest; see idmapWorkspaceMounts.
	wantUID := []specs.LinuxIDMapping{
		{ContainerID: 0, HostID: 0, Size: 501},
		{ContainerID: 501, HostID: 1000, Size: 1},
		{ContainerID: 502, HostID: 502, Size: 498},
		{ContainerID: 1000, HostID: 501, Size: 1},
		{ContainerID: 1001, HostID: 1001, Size: idSpace - 1001},
	}
	if !reflect.DeepEqual(s.Mounts[0].UIDMappings, wantUID) {
		t.Errorf("Mounts[0].UIDMappings = %+v, want %+v", s.Mounts[0].UIDMappings, wantUID)
	}
	if got := mapID(s.Mounts[0].GIDMappings, 20); got != 1000 {
		t.Errorf("the host's gid 20 is seen as %d, want the container's 1000", got)
	}
}

// Every on-disk id must have a mapping, or the kernel refuses writes to any file owned by it
// — a file in group wheel under /tmp was unwritable with the host's gid as the only entry.
func TestSwapIDMappingCoversEveryIDOneToOne(t *testing.T) {
	for _, c := range []struct{ onDisk, seen uint32 }{{501, 1000}, {1000, 501}, {20, 1000}, {0, 1000}, {1000, 1000}, {idSpace - 1, 0}} {
		m := swapIDMapping(c.onDisk, c.seen)
		var next, total uint32
		for _, e := range m {
			if e.ContainerID != next {
				t.Fatalf("swapIDMapping(%d, %d) = %+v: on-disk ids from %d are unmapped", c.onDisk, c.seen, m, next)
			}
			next, total = e.ContainerID+e.Size, total+e.Size
		}
		if total != idSpace {
			t.Errorf("swapIDMapping(%d, %d) covers %d ids, want %d", c.onDisk, c.seen, total, idSpace)
		}
		for _, id := range []uint32{0, 20, 501, 1000, 65534, c.onDisk, c.seen} {
			want := id
			switch id {
			case c.onDisk:
				want = c.seen
			case c.seen:
				want = c.onDisk
			}
			if got := mapID(m, id); got != want {
				t.Errorf("swapIDMapping(%d, %d) sees on-disk %d as %d, want %d", c.onDisk, c.seen, id, got, want)
			}
		}
	}
}

// mapID is the kernel's lookup of an on-disk id in a mount's mapping.
func mapID(m []specs.LinuxIDMapping, onDisk uint32) uint32 {
	for _, e := range m {
		if onDisk >= e.ContainerID && onDisk-e.ContainerID < e.Size {
			return e.HostID + onDisk - e.ContainerID
		}
	}
	return 65534
}

// Without a guest that can idmap, the mechanism must fall back to exactly today's behavior —
// losing the benefit, never losing correctness.
func TestReconcileWorkspaceIdentityFallsBackWhenTheGuestCannotIdmap(t *testing.T) {
	ws := workspace.Workspace{HostPath: "/tmp/x", GuestPath: "/tmp/x"}
	s := &specs.Spec{
		Process: &specs.Process{User: specs.User{UID: 1000, GID: 1000}},
		Mounts:  []specs.Mount{{Type: "bind", Source: ws.HostPath, Destination: ws.GuestPath}},
	}

	reconcileWorkspaceIdentity(s, []workspace.Workspace{ws}, 501, 20, false)

	if s.Process.User.UID != 501 || s.Process.User.GID != 20 {
		t.Errorf("Process.User = %d:%d, want the host's 501:20", s.Process.User.UID, s.Process.User.GID)
	}
	if s.Mounts[0].UIDMappings != nil || s.Mounts[0].GIDMappings != nil {
		t.Errorf("Mounts[0] carries an idmap (%+v/%+v); the fallback must leave mounts untouched",
			s.Mounts[0].UIDMappings, s.Mounts[0].GIDMappings)
	}
}

// A root-resolved image has no non-root container-side id to idmap to, so it must take the
// override even when the guest could otherwise idmap — asserted with guestCanIdmap: true so a
// mutation that dropped the UID != 0 guard would be caught here, not masked by the guest
// simply never being able to idmap in this test's environment.
func TestReconcileWorkspaceIdentityNeverIdmapsARootImage(t *testing.T) {
	ws := workspace.Workspace{HostPath: "/tmp/x", GuestPath: "/tmp/x"}
	s := &specs.Spec{
		Process: &specs.Process{User: specs.User{UID: 0, GID: 0}},
		Mounts:  []specs.Mount{{Type: "bind", Source: ws.HostPath, Destination: ws.GuestPath}},
	}

	reconcileWorkspaceIdentity(s, []workspace.Workspace{ws}, 501, 20, true)

	if s.Process.User.UID == 0 {
		t.Error("the guest was told to run as root")
	}
	if s.Mounts[0].UIDMappings != nil {
		t.Errorf("a root-resolved image got an idmap instead of the override: %+v", s.Mounts[0].UIDMappings)
	}
}

// A read-only mount has no EACCES-on-write problem to begin with; only the writable one should
// change.
func TestReconcileWorkspaceIdentityOnlyIdmapsWritableMounts(t *testing.T) {
	rw := workspace.Workspace{HostPath: "/tmp/rw", GuestPath: "/tmp/rw", Mode: workspace.ModeReadWrite}
	ro := workspace.Workspace{HostPath: "/tmp/ro", GuestPath: "/tmp/ro", Mode: workspace.ModeReadOnly}
	s := &specs.Spec{
		Process: &specs.Process{User: specs.User{UID: 1000, GID: 1000}},
		Mounts: []specs.Mount{
			{Type: "bind", Source: rw.HostPath, Destination: rw.GuestPath},
			{Type: "bind", Source: ro.HostPath, Destination: ro.GuestPath},
		},
	}

	reconcileWorkspaceIdentity(s, []workspace.Workspace{rw, ro}, 501, 20, true)

	if s.Mounts[0].UIDMappings == nil {
		t.Error("the read-write mount got no idmap")
	}
	if s.Mounts[1].UIDMappings != nil {
		t.Errorf("the read-only mount got an idmap it has no use for: %+v", s.Mounts[1].UIDMappings)
	}
}
