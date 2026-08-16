package daemon

import (
	"debug/buildinfo"
	"fmt"
	"strconv"
	"strings"
)

// Whether the pieces Boks found are compatible with each other, not merely present.
//
// # The failure this exists for
//
// Measured on WSL2 on 2026-08-15. The CI-built nerdbox shim links containerd v2.3.3 and emits
// **version-3 bootstrap parameters**. Ubuntu's containerd 2.2.2 does not know that encoding,
// falls back to reading the whole protobuf reply as an address, and fails with:
//
//	unsupported protocol: Yunix
//
// "Yunix" is three control bytes from the protobuf framing rendered as letters. Nothing in that
// error names a version, a shim or a protocol revision, and there is no way to reach the right
// conclusion from it. Both binaries were individually fine; `boks doctor` reported `containerd
// ok` and `vm runtime ok`, because both were true. The set was wrong.
//
// # Why this is cheap
//
// A Go binary records its own module graph, which is what `go version -m` prints, and
// debug/buildinfo reads it from a file with no subprocess and no network. So the containerd
// version a shim was built against is a local, offline, exact fact about a file on disk — and
// the containerd the daemon runs answers its version over its own API. Comparing the two is
// the whole check.
//
// # What it does not check
//
// Three other skews are known and are not covered here, because each needs a different
// technique and only the first has an incident behind it:
//
//   - **libkrun's exported symbols.** nerdbox binds all nineteen entry points eagerly at
//     dlopen, so a libkrun missing any of them fails to load entirely rather than failing at
//     the call. Measured 2026-08-15: at the revision the Windows port pins (2.0.0-dev) four of
//     the nineteen are absent, and Windows only works because this project's patches re-export
//     them — which also means Linux and Windows need different libkrun revisions. Checking it
//     means reading the dynamic symbol table of an ELF, a Mach-O and a PE. That is stdlib
//     (debug/elf, debug/macho, debug/pe) and is the obvious next check.
//   - **The guest kernel and rootfs against the shim that boots them.** Neither file carries a
//     version, so there is nothing to compare without publishing a manifest alongside them.
//   - **libkrun's API against the nerdbox revision.** krun_set_exec returning -ENOTSUP and
//     krun_add_vsock_port returning -ENODEV were both upstream removals between the revision
//     nerdbox targets and the one this project pinned. A symbol table would catch the second
//     kind (removed) and not the first (present but refusing).

// containerdModule is the module path a shim links when it links containerd.
const containerdModule = "github.com/containerd/containerd/v2"

// nerdboxModule is the module path of the shim's own source tree.
const nerdboxModule = "github.com/containerd/nerdbox"

// ShimNerdbox returns the nerdbox source revision the shim binary at path was built from,
// or "" if that cannot be established.
//
// This is the same technique as ShimContainerd and a weaker one. ShimContainerd reads a
// *dependency's* version out of the module graph, which is recorded for every Go build.
// nerdbox is the shim's own main module, so there is no dependency entry to read; what
// there is instead is the VCS stamp the toolchain writes when it builds a main package
// from inside a checkout. `go build` in a git tree sets it, which is what
// .github/workflows/linux-runtime.yml does. A build from an unpacked tarball, or one with
// -buildvcs=false, does not, and answers "" here.
func ShimNerdbox(path string) string {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	if info.Main.Path != nerdboxModule {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}

// nerdboxRevisionsResolvingUsernames lists the nerdbox revisions whose vminitd resolves
// Process.User.Username against the guest's own /etc/passwd.
//
// It is empty. That is the state of the world and not a placeholder left unfilled: no
// released nerdbox resolves that field, the patch that would is carried unapplied in
// packaging/nerdbox/patches/, and neither guest-image.yml nor linux-runtime.yml applies it
// — both build the pinned revision pristine and assert as much. So there is no revision
// that belongs in this map yet, and ShimResolvesUsernames below is false for every input.
//
// A map of exact revisions rather than a "this version or newer" comparison, which is what
// CheckSkew does for containerd. The difference is that containerd's bootstrap encoding is
// monotonic — a newer reader understands every older encoding — and this is not a
// compatibility relation at all but the presence or absence of a feature in a source tree.
// nerdbox has no release carrying it, so there is no version boundary to name, and
// inventing one would be a claim about revisions nobody has looked at.
var nerdboxRevisionsResolvingUsernames = map[string]bool{}

// ShimResolvesUsernames reports whether the guest booted by the shim at path resolves an
// image's USER name into a uid, rather than leaving Process.User.Username for a runtime
// that ignores it.
//
// # Why the caller needs this
//
// An OCI image may say `USER node`. Resolving the name means reading /etc/passwd inside the
// image, which a host can only do by mounting the image — needing CAP_SYS_ADMIN on Linux
// and being impossible on Windows and macOS, where the guest filesystem is not one the host
// can mount. containerd's answer on those hosts is to record the name in
// Process.User.Username and stop. Nothing downstream reads that field: crun consults only
// uid, gid, umask and additional_gids, so the uid stays 0 and the container runs as root
// without saying so. A caller that would skip the host-side resolution has to know whether
// the guest will pick the work up, and the honest default is that it will not.
//
// # What this actually proves, and what it does not
//
// It answers about the **shim**, and the code that would do the resolving is in **vminitd**,
// inside the guest rootfs — a different artifact, built from the same nerdbox revision by
// convention rather than by anything checkable at runtime. packaging/nerdbox/NERDBOX_REV is
// a single pin for exactly that reason, and guest-image.yml and linux-runtime.yml both read
// it, so in every build this project produces the two do agree. A guest image swapped in by
// hand can still disagree, and nothing here would notice: the rootfs carries no version, no
// manifest and no embedded revision, which compat.go's own list of uncovered skews already
// records as the gap it is.
//
// That weakness is survivable only because of the direction it fails in. An unknown shim,
// an unstamped build, a hand-built guest — all of them answer false, which keeps the
// caller on the path that works everywhere. The check can withhold a capability that is
// present; it cannot invent one that is absent, and only the second direction is unsafe.
func ShimResolvesUsernames(path string) bool {
	if path == "" {
		return false
	}
	revision := ShimNerdbox(path)
	return revision != "" && nerdboxRevisionsResolvingUsernames[revision]
}

// ShimContainerd returns the containerd version the binary at path was built against, or ""
// if the file is not a Go binary or does not link containerd at all.
//
// A replace directive is honoured: what matters is the code that ended up in the binary, which
// is the replacement's version, not the requirement's.
func ShimContainerd(path string) string {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, dep := range info.Deps {
		if dep == nil || dep.Path != containerdModule {
			continue
		}
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version
		}
		return dep.Version
	}
	return ""
}

// version is a major.minor pair, which is the granularity every claim here is made at.
type version struct{ major, minor int }

// parseVersion reads a containerd version string. Both spellings occur: "v2.3.3" from a module
// graph and "2.2.6" from the daemon's own API, and the build in packaging/containerd-windows
// stamps "2.2.6+boks-erofs", so a build metadata suffix must not defeat the parse.
func parseVersion(s string) (version, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	for _, cut := range []string{"+", "-"} {
		if i := strings.Index(s, cut); i >= 0 {
			s = s[:i]
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return version{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return version{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return version{}, false
	}
	return version{major, minor}, true
}

func (v version) String() string { return fmt.Sprintf("%d.%d", v.major, v.minor) }

// olderThan compares at major.minor.
func (v version) olderThan(other version) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	return v.minor < other.minor
}

// Skew describes an incompatibility between the daemon and the shim it would launch.
type Skew struct {
	// Daemon and Shim are the two containerd versions, as found.
	Daemon, Shim string
	// Detail is a one-line statement of the mismatch.
	Detail string
	// Remedy explains what to do.
	Remedy string
}

// CheckSkew compares the containerd a daemon reports with the containerd a shim links.
//
// The rule is directional and narrow: a daemon **older** than the shim's containerd is a
// problem, and a newer one is not. That follows from what the shim and the daemon each do with
// the bootstrap parameters — the shim writes them and the daemon reads them, and a reader
// understands every encoding up to its own. It is deliberately stated at major.minor: the
// measured failure was 2.3 against 2.2, and no patch-level incompatibility has been observed.
//
// It returns nil when the versions are compatible, when either is unknown, or when either
// cannot be parsed. An unreadable version is not evidence of a problem, and a check that
// guessed would produce warnings on hosts that are fine — which is how a warning becomes
// something people learn to ignore.
func CheckSkew(daemonVersion, shimVersion string) *Skew {
	if daemonVersion == "" || shimVersion == "" {
		return nil
	}
	d, okD := parseVersion(daemonVersion)
	s, okS := parseVersion(shimVersion)
	if !okD || !okS {
		return nil
	}
	if !d.olderThan(s) {
		return nil
	}
	return &Skew{
		Daemon: daemonVersion,
		Shim:   shimVersion,
		Detail: fmt.Sprintf("containerd %s is older than the %s the shim was built against", d, s),
		Remedy: fmt.Sprintf(
			"The runtime shim was built against containerd %s and this daemon is %s.\n"+
				"A shim emits bootstrap parameters in its own containerd's encoding, and an\n"+
				"older daemon cannot decode them. Measured on 2026-08-15 with a shim linking\n"+
				"2.3.3 against containerd 2.2.2, the sandbox fails at task start with\n\n"+
				"    unsupported protocol: Yunix\n\n"+
				"which is protobuf framing read as an address and names nothing that is wrong.\n"+
				"Upgrade containerd to %s or later, or rebuild the shim against %s.",
			s, d, s, d),
	}
}
