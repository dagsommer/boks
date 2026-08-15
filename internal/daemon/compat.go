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
