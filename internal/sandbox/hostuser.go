package sandbox

import (
	"context"
	"os"
	"runtime"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// withHostUser runs the guest process as the uid and gid of the user who started Boks,
// whenever a workspace is shared.
//
// # The problem it solves
//
// A workspace is a live share of a host directory, and the guest sees the host's real
// ownership: a file created on a Mac by uid 501 reports uid 501 inside the VM. The guest
// kernel then checks that against the process's own uid, and a process running as anything
// else cannot write — `touch` in your own repository fails with EACCES.
//
// libkrun's virtiofs has no id mapping to fix this from the other side. Its macOS passthrough
// offers two permission models, and the one that would help — LinuxSimplified, which reports
// every file as owned by whoever is asking — is a Rust-internal default with no C API to
// select it (Config.semantics, defaulting to LinuxComplete). Docker Sandboxes solves the same
// problem in its own virtiofs, which is not libkrun's. So the only lever available here is
// which uid the guest process runs as, and this is that lever.
//
// # What this breaks in an image, and what an image owes in return
//
// The uid it picks is a HOST uid, and no image's /etc/passwd knows it. That has consequences
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
// look like the sandbox is broken rather than the image being unprepared. Where Boks can, it
// degrades rather than aborting: boks-install-ca warns and continues when the trust store is
// unwritable, since the mounted CA bundle still reaches every runtime that reads an
// environment variable.
//
// # Why it was not needed before, and why that was worse
//
// Until the images were given numeric users, `USER agent` silently became uid 0 off Linux and
// every sandbox ran as root. Root bypasses permission checks, so writes worked — and every
// file an agent created in a user's repository was owned by ROOT, on their host, needing sudo
// to remove. That is the failure this replaces, not a state worth returning to.
//
// # What it does not do
//
// It does not touch ownership anywhere. Writing to an existing file does not change its owner,
// and new files land owned by the user who ran Boks, which is what they would be if the same
// command had been run on the host.
//
// # When it stays out of the way
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
		s.Process.User.UID = uint32(uid)
		s.Process.User.GID = uint32(gid)
		// The primary group belongs in the supplementary set, which is what containerd's
		// own ensureAdditionalGids does after setting a user. Rebuilt rather than appended
		// to: the groups that came from the image belong to a user this no longer is.
		s.Process.User.AdditionalGids = []uint32{uint32(gid)}
		return nil
	}
}
