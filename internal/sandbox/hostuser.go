package sandbox

import (
	"context"
	"os"
	"runtime"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/dagsommer/boks/internal/workspace"
)

// withHostUser reconciles a shared workspace's host ownership with the guest, whenever a
// workspace is shared.
//
// # The problem it solves
//
// A workspace is a live share of a host directory, and the guest sees the host's real
// ownership: a file created on a Mac by uid 501 reports uid 501 inside the VM. The guest
// kernel then checks that against the process's own uid, and a process running as anything
// else cannot write — `touch` in your own repository fails with EACCES.
//
// # Two mechanisms, picked per sandbox
//
// There are two ways to make that check pass: run the guest process as the host's own uid
// (overrideWithHostUser), or leave the process as the image's own user and instead idmap the
// workspace *mount* so it reports the image's uid as the owner while still writing through as
// the host's (idmapWorkspaceMounts). The second is strictly better when it is available — the
// image's own `USER` keeps working exactly as it would under plain Docker, with no HOME or
// ownership workarounds needed in the image at all — but it needs a shim that tells containerd
// its runtime supports idmapped mounts (patch 0003; see guestSupportsIdmappedMounts and
// packaging/nerdbox/README.md). Without one, the override below is the always-available
// fallback, and the two are not a staged migration — both stay live code, selected per
// sandbox by what the shim on this host reports.
//
// # Why the override picks a HOST uid, and what that breaks in an unprepared image
//
// The override's uid is a HOST uid, and no image's /etc/passwd knows it. That has consequences
// an image has to be built for, and Boks' own images were not until they were measured:
//
//   - **HOME.** crun derives it from the container uid's passwd entry, finds nothing, and
//     leaves "/". The first dotfile write then fails on a doubled-slash path
//     ("Permission denied: '//.claude.json.boks-tmp'"). An image must declare HOME in its own
//     environment, where no lookup is needed; images/base/Dockerfile does.
//   - **Anything the image chowned to its own user.** Ownership cannot help a uid that is not
//     knowable at build time, so those paths need a MODE instead. images/base does that for
//     /home/agent and for the three paths update-ca-certificates writes.
//
// Neither is fixable from here for an image Boks did not build, and both fail in ways that
// look like the sandbox is broken rather than the image being unprepared. The idmap mechanism
// above exists specifically to stop needing either workaround, for every image, once the guest
// can do it.
//
// # Why it was not needed before, and why that was worse
//
// Until the images were given numeric users, `USER agent` silently became uid 0 off Linux and
// every sandbox ran as root. Root bypasses permission checks, so writes worked — and every
// file an agent created in a user's repository was owned by ROOT, on their host, needing sudo
// to remove. That is the failure the override replaces, not a state worth returning to — and
// it is why a container resolved to uid 0 always takes the override branch below rather than
// being idmapped: there is no non-root container-side id to idmap a root image to, and running
// the guest process as root (even behind an idmapped mount) is the thing being avoided.
//
// # When either mechanism stays out of the way
//
// Only when a workspace is shared: with nothing shared from the host there is no ownership to
// agree with, and the image's own user is the right answer.
//
// Never as root. os.Getuid() == 0 means Boks itself is running as root, and honouring that
// would put the agent back to running as root for the sake of matching it.
//
// Never on a platform with no uid semantics. os.Getuid() returns -1 on Windows, and a spec
// asking for uid 4294967295 is worse than one asking for the image's user.
func withHostUser(cfg Config) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if len(cfg.Workspaces) == 0 || runtime.GOOS == "windows" {
			return nil
		}
		uid, gid := os.Getuid(), os.Getgid()
		if uid <= 0 || gid < 0 {
			return nil
		}
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		reconcileWorkspaceIdentity(s, guestShares(cfg), uint32(uid), uint32(gid), guestSupportsIdmappedMounts())
		return nil
	}
}

// reconcileWorkspaceIdentity is withHostUser's decision, with the guest's ability to idmap as
// a parameter rather than read from guestSupportsIdmappedMounts directly — the same idiom
// usesMetadataImageConfigOn uses for the equivalent decision in imageconfig.go, and for the
// same reason: every branch can then be exercised from a test with no real shim on disk.
//
// A root-resolved image (no USER declared, or a name the host could not resolve) has no
// non-root container-side id to idmap to, so it always takes the override — regardless of
// what the guest supports.
func reconcileWorkspaceIdentity(s *specs.Spec, shares []workspace.Workspace, hostUID, hostGID uint32, guestCanIdmap bool) {
	if s.Process.User.UID != 0 && guestCanIdmap {
		idmapWorkspaceMounts(s, shares, hostUID, hostGID)
		return
	}
	overrideWithHostUser(s, hostUID, hostGID)
}

// overrideWithHostUser is the fallback mechanism: it does not touch ownership anywhere.
// Writing to an existing file does not change its owner, and new files land owned by the user
// who ran Boks, which is what they would be if the same command had been run on the host.
func overrideWithHostUser(s *specs.Spec, hostUID, hostGID uint32) {
	s.Process.User.UID = hostUID
	s.Process.User.GID = hostGID
	// The primary group belongs in the supplementary set, which is what containerd's own
	// ensureAdditionalGids does after setting a user. Rebuilt rather than appended to: the
	// groups that came from the image belong to a user this no longer is.
	s.Process.User.AdditionalGids = []uint32{hostGID}
}

// idmapWorkspaceMounts attaches a one-entry uid/gid mapping to every writable mount in shares,
// translating the host's uid/gid (what the guest's virtiofs share actually reports) to the
// container process's own uid/gid (what imageConfigOpt already resolved the image's USER to,
// and which this function leaves untouched). crun applies it with a throwaway user namespace
// scoped to the mount alone — the container needs no user namespace of its own for this.
//
// # The fields read backwards, on purpose
//
// ContainerID is the id ON DISK (the host's) and HostID is the id the container SEES. That
// is the reverse of what the names suggest, and it is not a crun quirk: crun writes each entry
// straight into the throwaway namespace's uid_map as "inside outside size", and an idmapped
// mount translates an on-disk id by treating it as an *inside* id of that namespace. The names
// only make sense when a container has a user namespace of its own, where a mount's mapping
// is normally a copy of the container's. The natural-reading version was shipped to a real
// guest first, on 2026-10-01: every file read back as 65534 and every write failed with
// EOVERFLOW, because the host's 502 was on the side the kernel never looks up.
//
// # Every id is mapped, not just the host's
//
// The mapping covers the whole id range, swapping the host's id with the container's and
// leaving every other id as itself (see swapIDMapping). A one-entry mapping was shipped first,
// and it made any file whose owner or group was some other id unwritable: the kernel refuses a
// write, and answers chmod with EOVERFLOW, on an idmapped inode whose uid *or* gid has no
// mapping — even when its owner is the agent. That is every file in a workspace under /tmp on
// a Mac, where files take the directory's group (wheel, 0) rather than the user's own. Mapped
// to themselves, those ids reach the agent as what they are on disk, and ordinary permission
// bits decide, as they would on the host.
//
// Only writable mounts get one: a read-only mount has no EACCES-on-write problem to begin
// with, and an idmap on a mount nothing writes through changes nothing observable.
func idmapWorkspaceMounts(s *specs.Spec, shares []workspace.Workspace, hostUID, hostGID uint32) {
	containerUID, containerGID := s.Process.User.UID, s.Process.User.GID
	writable := make(map[string]bool, len(shares))
	for _, ws := range shares {
		if !ws.ReadOnly() {
			writable[ws.GuestPath] = true
		}
	}
	for i := range s.Mounts {
		if !writable[s.Mounts[i].Destination] {
			continue
		}
		s.Mounts[i].UIDMappings = swapIDMapping(hostUID, containerUID)
		s.Mounts[i].GIDMappings = swapIDMapping(hostGID, containerGID)
		// crun's own idmap-without-container-userns test (test_mounts.py,
		// test_idmapped_mounts_without_userns) always pairs UIDMappings/GIDMappings with
		// a literal "idmap"/"ridmap" entry in Options; nothing in crun's own source makes
		// that strictly required once UIDMappings is set (it only ever reads the option
		// string for its *recursive* flag, src/libcrun/linux.c get_idmapped_option), but
		// it's the actually-tested shape and workspace mounts are already rbind — mirror
		// that with "ridmap" rather than relying on an untested bare-UIDMappings path.
		s.Mounts[i].Options = append(s.Mounts[i].Options, "ridmap")
	}
}

// idSpace is how many ids a mapping covers: 0 through 4294967294. 4294967295 is (uid_t)-1,
// which is not an id but "no change" to chown, and no uid_map may include it.
const idSpace = 1<<32 - 1

// swapIDMapping maps the whole id space so that onDisk and seen trade places and every other
// id maps to itself, in the field order idmapWorkspaceMounts documents (ContainerID on disk,
// HostID seen). A swap rather than a plain onDisk→seen entry because a mapping must be
// one-to-one: seen's own on-disk id has to go somewhere, and onDisk is the id left free.
func swapIDMapping(onDisk, seen uint32) []specs.LinuxIDMapping {
	if onDisk == seen {
		return []specs.LinuxIDMapping{{ContainerID: 0, HostID: 0, Size: idSpace}}
	}
	lo, hi := min(onDisk, seen), max(onDisk, seen)
	var m []specs.LinuxIDMapping
	if lo > 0 {
		m = append(m, specs.LinuxIDMapping{ContainerID: 0, HostID: 0, Size: lo})
	}
	m = append(m, specs.LinuxIDMapping{ContainerID: lo, HostID: hi, Size: 1})
	if hi-lo > 1 {
		m = append(m, specs.LinuxIDMapping{ContainerID: lo + 1, HostID: lo + 1, Size: hi - lo - 1})
	}
	m = append(m, specs.LinuxIDMapping{ContainerID: hi, HostID: lo, Size: 1})
	if hi < idSpace-1 {
		m = append(m, specs.LinuxIDMapping{ContainerID: hi + 1, HostID: hi + 1, Size: idSpace - hi - 1})
	}
	return m
}
