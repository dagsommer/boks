package sandbox

import (
	"context"
	"fmt"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

// The three repairs in this file have one cause between them: containerd generates the OCI
// spec on the *host*, while the spec describes a Linux guest. Everywhere those two are the
// same machine — which is Linux, and only Linux — the difference is invisible. On a Windows
// host it is not, and none of the three symptoms names itself.

// imageConfigOpt applies the guest image's own configuration — environment, argv, working
// directory and user — to the spec.
//
// On the host where containerd's own oci.WithImageConfig works, this is that option,
// unchanged, because a subtly different reimplementation of a well-exercised one is not worth
// having. That host is Linux, and only Linux; see containerdReadsGuestRootfs.
//
// Windows cannot read it. oci.WithImageConfig finishes by resolving the process's
// supplementary groups out of the image's /etc/group, and it does that by mounting the
// image's snapshot **on the host**: containerd's WithAdditionalGIDs calls
// mount.WithReadonlyTempMount, and mount_windows.go refuses anything whose type is not
// "windows-layer" — a Boks snapshot is a Linux EROFS filesystem. Container creation would
// fail with "invalid windows mount type: erofs" before the VM was ever asked for, which
// names the wrong problem entirely.
//
// containerd already meets this on macOS, where a host mount of a guest filesystem is
// likewise impossible, and answers it by skipping the lookup: WithAdditionalGIDs returns
// early when runtime.GOOS == "darwin", and WithUser records the image's user string in
// Process.User.Username for the guest to resolve instead of resolving it here. Windows is
// absent from those guards only because containerd on Windows builds Windows specs, which
// never reach the Linux branch at all. So the Windows path below is not a new behaviour: it
// is the macOS behaviour, on the other host that cannot mount a Linux filesystem.
//
// macOS now takes this same path rather than containerd's version of it, because containerd's
// skip is only a skip: it abandons the numeric ids too. See containerdReadsGuestRootfs.
//
// What that costs, stated plainly: off Linux the guest process gets no supplementary groups
// from the image. That is not a change — containerd's darwin guard skipped them already.
//
// The Windows branch has never been executed on Windows. It is reasoned from containerd's
// source — mount_windows.go, spec_opts.go — and from the fact that the macOS path it copies
// does work; that is not the same as having watched it create a container.
//
// The metadata-only path is also what Linux would use if it could. containerd's host-side
// resolution is the single reason Boks needs more privilege on Linux than an ordinary user
// has: WithAdditionalGIDs mounts the image's EROFS snapshot on the host, and that mount
// wants CAP_SYS_ADMIN. Rootless containerd does not rescue it — `unshare -Umr mount -t
// erofs` is EPERM, because erofs carries no FS_USERNS_MOUNT. Dropping to the metadata path
// would drop the requirement.
//
// It is gated rather than taken, because taking it early is a silent privilege bug and not
// a missing feature. The metadata path leaves an image's `USER node` in
// Process.User.Username, and nothing downstream reads that field — see
// guestResolvesUsernames. Switching Linux over before a guest exists that resolves it
// would turn every name-form USER into uid 0 with no error anywhere. So the condition is
// the guest's demonstrated ability, not the host's inability, and it currently never holds.
func imageConfigOpt(image client.Image) oci.SpecOpts {
	if usesMetadataImageConfig() {
		return withImageConfigFromMetadata(image)
	}
	return oci.WithImageConfig(image)
}

// usesMetadataImageConfig is the choice imageConfigOpt makes, separated from it so it can
// be asserted on. An oci.SpecOpts is a closure and two of them cannot be compared, so a
// test that went through imageConfigOpt could only check which option it got by running it
// — which needs a containerd, a snapshotter and an image, none of which a unit test has.
func usesMetadataImageConfig() bool {
	return usesMetadataImageConfigOn(runtime.GOOS, guestResolvesUsernames())
}

// usesMetadataImageConfigOn is the rule itself, with the host and the guest's ability as
// parameters so that every host's branch can be exercised from any host. No machine on this
// project runs macOS, and the macOS branch is the one this rule was just corrected on, so a
// rule that could only be tested where it happens to run would be untested where it matters —
// the same reason internal/doctor/libkrun.go takes goos as an argument.
func usesMetadataImageConfigOn(goos string, guestResolves bool) bool {
	return !containerdReadsGuestRootfsOn(goos) || guestResolves
}

// guestResolvesUsernames reports whether the guest can be **confirmed** to turn an image's
// USER name into a uid itself.
//
// Read internal/daemon/compat.go's ShimResolvesUsernames for what the confirmation is worth;
// the short version is that it is a fact about the shim binary used as a proxy for the guest
// rootfs beside it, and that every uncertainty answers false.
//
// Today it is false on every host, because no nerdbox revision resolves that field. The
// effect is that Linux keeps containerd's own oci.WithImageConfig, exactly as before this
// function existed — the switch is written down and inert, which is the point. Making it
// true is a one-line change in compat.go once a guest that does the work can be identified,
// and until then Boks cannot ship the regression by accident.
//
// Computed once. It reads a binary off disk, the answer cannot change while the process
// runs, and this sits on the container-creation path.
var guestResolvesUsernames = sync.OnceValue(func() bool {
	// containerd's PATH rather than this process's: the shim is launched by containerd,
	// and daemon.ContainerdPath is what decides which one it finds.
	return daemon.ShimResolvesUsernames(
		daemon.FindShim(runtimecfg.Runtime, daemon.ContainerdPath(os.Getenv("PATH"))),
	)
})

// containerdReadsGuestRootfs reports whether containerd's own oci.WithImageConfig actually
// opens the image's root filesystem on this host — which is the only thing that makes it
// better than the reimplementation below.
//
// Linux is the only host where it does. There the host mounts the guest's EROFS snapshot for
// real, so `USER node` becomes a uid and /etc/group becomes supplementary groups.
//
// **macOS was on this list until 2026-08-17 and should never have been.** containerd cannot
// mount a Linux filesystem on a Mac either, and its guards say so — but they are guards
// against *reading*, not implementations of anything. WithUser (spec_opts.go:625 at v2.2.6)
// opens with
//
//	if (s.Windows != nil && s.Linux != nil) || runtime.GOOS == "darwin" {
//	        s.Process.User.Username = userstr
//	        return nil
//	}
//
// and that return is before every line that parses the string. So on macOS containerd stored
// the image's USER verbatim and left Process.User.UID at its zero value **for every USER
// value, numeric ones included**: an image saying `USER 1000` ran as root, with the number
// right there in the spec and nobody reading it. WithAdditionalGIDs returns early on darwin
// too, so nothing was resolved there either, and appendOSMounts is a no-op off FreeBSD.
//
// applyImageUser below does parse numeric ids, so moving macOS to the metadata path loses
// nothing containerd was doing and gains every image that numbers its user. Names remain
// unresolvable on both hosts, which is a guest problem — see packaging/nerdbox/patches/.
//
// Linux is decided by inclusion rather than by excluding Windows, because that is the claim
// being made: this host mounts the image. A host that does not belongs on the metadata path.
func containerdReadsGuestRootfs() bool { return containerdReadsGuestRootfsOn(runtime.GOOS) }

// containerdReadsGuestRootfsOn takes the host so the rule can be asserted for every platform
// from whichever one the tests run on. See usesMetadataImageConfigOn.
func containerdReadsGuestRootfsOn(goos string) bool { return goos == "linux" }

// withImageConfigFromMetadata applies the image config from the image's own metadata alone,
// touching no filesystem. See imageConfigOpt for when it is used and why.
func withImageConfigFromMetadata(image client.Image) oci.SpecOpts {
	return func(ctx context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		config, err := image.Spec(ctx)
		if err != nil {
			return fmt.Errorf("reading the configuration of image %s: %w", image.Name(), err)
		}
		return applyImageConfig(s, config.Config)
	}
}

// defaultGuestEnv is what an image that declares no environment gets. It is containerd's
// defaultUnixEnv verbatim: an image with no PATH is otherwise a guest where nothing resolves.
var defaultGuestEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

// applyImageConfig writes an image's configuration into a Linux spec.
//
// It mirrors the Linux branch of containerd's WithImageConfigArgs, minus the step that reads
// the image's root filesystem, and the order of the four fields is that function's order.
// Boks overrides argv and — whenever there is a workspace — the working directory a moment
// later, so the fields that matter in practice are the environment and the user; the other
// two are still set, because a caller that overrides neither must get what the image asked
// for rather than an empty spec.
func applyImageConfig(s *specs.Spec, config ocispec.ImageConfig) error {
	if s.Process == nil {
		s.Process = &specs.Process{}
	}

	env := config.Env
	if len(env) == 0 {
		env = defaultGuestEnv
	}
	// The image's environment is the base and whatever the spec already carries overrides
	// it, which is containerd's order. It matters less than it looks: the default Linux
	// spec sets no environment at all, so this is normally just the image's.
	s.Process.Env = replaceOrAppendEnv(env, s.Process.Env)

	s.Process.Args = append(append([]string{}, config.Entrypoint...), config.Cmd...)

	cwd := config.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	s.Process.Cwd = cwd

	if config.User != "" {
		applyImageUser(s, config.User)
	}
	return nil
}

// applyImageUser writes an image's USER value into the spec, resolving as much of it as can
// be resolved without reading the image's root filesystem.
//
// # Two things are written, and they are for different readers
//
// **The name, verbatim, into Process.User.Username.** That is the field containerd itself
// uses for a user string only the guest can interpret, and it is the only channel through
// which the part the host cannot do can still get done. A guest that resolves it produces
// exactly what a Linux host with a mount would have produced, supplementary groups included.
//
// **Whatever uid and gid the string states outright.** An image is free to say `USER 1000`
// or `USER 1000:1000`, and those need no /etc/passwd — they are already numbers. Every host
// can honour them, and the numbers stand whether or not the guest ever reads Username.
//
// # Why the second one matters more than it looks
//
// Before this, every USER value took the same route: record the name, leave the uid at its
// zero value, and rely on a guest that reads Username. No runtime does read it — crun
// consults uid, gid, umask and additional_gids and nothing else — so on the hosts that use
// this function, Windows and macOS, `USER 1000` produced a container running as **root**.
// Not as uid 1000, and not with an error: as root, silently.
//
// Numeric forms are the common case and the completely unambiguous one, so they are fixed
// here and now, on every host, without waiting for anything in the guest. `uid:gid` becomes
// exactly right. A bare `uid` becomes right in the uid and takes gid 0, which is what
// containerd's own WithUserID settles on when /etc/passwd has no entry for that uid — and
// a guest that later reads Username refines the gid to the passwd one.
//
// What is left is the genuinely irreducible case: a *name*, which no amount of host-side
// parsing can turn into a number. Those still run as uid 0 on Windows and macOS. That is
// unchanged rather than newly introduced, it is the reason for the guest patch in
// packaging/nerdbox/patches/, and it is recorded in docs/verification.md as an open defect
// rather than left for someone to discover.
func applyImageUser(s *specs.Spec, userstr string) {
	s.Process.User.AdditionalGids = nil
	// Kept even when the numbers below are known, so that a capable guest can supply the
	// primary group and the supplementary groups that a bare uid does not carry.
	s.Process.User.Username = userstr

	uid, haveUID, gid, haveGID := numericIDs(userstr)
	if haveUID {
		s.Process.User.UID = uid
	}
	if haveGID {
		s.Process.User.GID = gid
	}
	ensureAdditionalGIDs(s)
}

// numericIDs reports the uid and gid an image's USER string states as numbers.
//
// The OCI image spec allows `user`, `uid`, `user:group`, `uid:gid`, `uid:group` and
// `user:gid`, so either half may be a number or a name and they are decided separately. A
// half that is a name reports false and is left to the guest.
//
// The accepted range is containerd's: 0 to MaxInt32. The kernel would take more, but runc
// does not, and a uid that this accepts and the runtime one layer down rejects would turn a
// silent wrong answer into a confusing failure rather than into a right answer.
func numericIDs(userstr string) (uid uint32, haveUID bool, gid uint32, haveGID bool) {
	const maxID = 1<<31 - 1
	parse := func(s string) (uint32, bool) {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 || v > maxID {
			return 0, false
		}
		return uint32(v), true
	}

	parts := strings.Split(userstr, ":")
	if len(parts) > 2 {
		// Not a shape the image spec defines. Nothing is claimed about it here; the
		// string still travels in Username, where a guest can reject it knowingly.
		return 0, false, 0, false
	}
	uid, haveUID = parse(parts[0])
	if len(parts) == 2 {
		gid, haveGID = parse(parts[1])
	}
	return uid, haveUID, gid, haveGID
}

// ensureAdditionalGIDs keeps the process's primary group in its supplementary set, which is
// what containerd's own ensureAdditionalGids does after setting a user.
func ensureAdditionalGIDs(s *specs.Spec) {
	for _, gid := range s.Process.User.AdditionalGids {
		if gid == s.Process.User.GID {
			return
		}
	}
	s.Process.User.AdditionalGids = append([]uint32{s.Process.User.GID}, s.Process.User.AdditionalGids...)
}

// replaceOrAppendEnv layers overrides onto defaults, following the OCI convention
// containerd implements: a later KEY=VALUE replaces an earlier one in place, and a bare KEY
// with no '=' removes it.
func replaceOrAppendEnv(defaults, overrides []string) []string {
	result := append([]string{}, defaults...)
	index := make(map[string]int, len(defaults))
	for i, entry := range defaults {
		key, _, _ := strings.Cut(entry, "=")
		index[key] = i
	}

	var removed []int
	for _, entry := range overrides {
		key, _, assigns := strings.Cut(entry, "=")
		at, exists := index[key]
		switch {
		case !assigns && exists:
			removed = append(removed, at)
		case !assigns:
			// Unsetting something that was never set: nothing to do, and nothing
			// to append — a bare KEY is not an assignment.
		case exists:
			result[at] = entry
		default:
			index[key] = len(result)
			result = append(result, entry)
		}
	}

	if len(removed) == 0 {
		return result
	}
	drop := make(map[int]bool, len(removed))
	for _, at := range removed {
		drop[at] = true
	}
	kept := result[:0]
	for i, entry := range result {
		if !drop[i] {
			kept = append(kept, entry)
		}
	}
	return kept
}

// withPOSIXCgroupsPath rewrites the cgroups path containerd generated so that it is a guest
// path rather than a host one.
//
// containerd builds it as filepath.Join("/", namespace, id), and filepath follows the *host's*
// separator: on a Windows host the spec would carry `\boks\claude-myrepo` where the guest's
// runtime expects `/boks/claude-myrepo`. It is the same mistake internal/enforce was fixed for
// and the same one internal/workspace's guestPath exists to avoid — a guest path is always
// POSIX, whatever machine wrote it down.
//
// This is a repair rather than a replacement because the value itself is containerd's to
// choose: only its spelling is wrong, and only on one host.
func withPOSIXCgroupsPath() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Linux == nil {
			return nil
		}
		s.Linux.CgroupsPath = posixCgroupsPath(s.Linux.CgroupsPath)
		return nil
	}
}

// withoutWindowsSection removes the `windows` section from a Linux spec.
//
// containerd puts one there on a Windows host and only on a Windows host. The non-Windows
// branch of its generateDefaultSpecWithPlatform (pkg/oci/spec.go) ends with
//
//	if err == nil && runtime.GOOS == "windows" {
//		// To run LCOW we have a Linux and Windows section. Add an empty one now.
//		s.Windows = &specs.Windows{}
//	}
//
// which is a condition on the host's OS and says nothing about the runtime that will read the
// spec. LCOW wants it — the runhcs shim fills LayerFolders in with the container's layer
// paths. A microVM guest running crun does not, and cannot survive it.
//
// `omitempty` on specs.Spec.Windows does not help: the field is a *pointer*, and to
// encoding/json a non-nil pointer to a zero struct is not empty. specs.Windows.LayerFolders
// carries no omitempty of its own, so the section marshals as
//
//	"windows":{"layerFolders":null}
//
// crun parses config.json with libocispec, whose generated parser checks a schema's required
// fields whenever the object holding them is present, and runtime-spec's
// schema/config-windows.json makes layerFolders required. The `windows` object is optional;
// its contents are not. Measured on Windows 11 on 2026-08-14: the VM booted, the guest
// mounted the container's rootfs, and crun then refused the spec with
// `Required field 'layerFolders' not present`.
//
// The section is removed rather than filled in. A Linux guest has no layer folders, and a
// path invented to satisfy a parser would be data the guest has to ignore — worse than the
// error, because it would not be an error.
//
// This runs immediately after the spec is generated, so that from here on the spec is a
// Linux spec and nothing else, whatever host wrote it. That ordering is not the same as
// containerd's own ctr, which must remove the section *last*: oci.WithImageConfig reads
// `s.Windows != nil` as "this is LCOW, the guest resolves users and groups for itself" and
// takes shortcuts on that basis. Boks never calls oci.WithImageConfig on Windows —
// imageConfigOpt above substitutes withImageConfigFromMetadata, which reaches no filesystem
// and consults no platform section — so the shortcut is not one Boks is relying on. Removing
// the section here makes that comment's claim true unconditionally rather than by accident.
func withoutWindowsSection() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Linux == nil {
			// Not a Linux spec. Boks generates nothing else, but the option runs
			// unconditionally, and stripping the only platform section from a spec
			// that is genuinely Windows' would be a worse bug than the one this fixes.
			return nil
		}
		s.Windows = nil
		return nil
	}
}

// posixCgroupsPath converts a cgroups path written with host separators into the POSIX one
// the guest will read. It is the identity on a POSIX host, where the two are already the same
// string.
func posixCgroupsPath(cgroupsPath string) string {
	if cgroupsPath == "" {
		return ""
	}
	// systemd-style paths ("slice:prefix:name") are not filesystem paths at all and carry
	// no separator to correct. containerd does not generate them, but a spec that arrived
	// with one must not be mangled into something else.
	if strings.Contains(cgroupsPath, ":") {
		return cgroupsPath
	}
	return path.Clean(strings.ReplaceAll(cgroupsPath, `\`, "/"))
}
