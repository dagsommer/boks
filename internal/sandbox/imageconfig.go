package sandbox

import (
	"context"
	"fmt"
	"path"
	"runtime"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The two repairs in this file have one cause between them: containerd generates the OCI
// spec on the *host*, while the spec describes a Linux guest. Everywhere those two are the
// same machine — which is Linux, and only Linux — the difference is invisible. On a Windows
// host it is not, and neither symptom names itself.

// imageConfigOpt applies the guest image's own configuration — environment, argv, working
// directory and user — to the spec.
//
// On every host where containerd's own oci.WithImageConfig works, this is that option,
// unchanged, because a subtly different reimplementation of a well-exercised one is not worth
// having and Linux and macOS already work.
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
// is the macOS behaviour, which Boks already ships and runs, on the other host that cannot
// mount a Linux filesystem.
//
// What that costs, stated plainly: on Windows the guest process gets no supplementary groups
// from the image, exactly as on macOS today.
//
// The Windows branch has never been executed on Windows. It is reasoned from containerd's
// source — mount_windows.go, spec_opts.go — and from the fact that the macOS path it copies
// does work; that is not the same as having watched it create a container.
func imageConfigOpt(image client.Image) oci.SpecOpts {
	if containerdReadsGuestRootfs() {
		return oci.WithImageConfig(image)
	}
	return withImageConfigFromMetadata(image)
}

// containerdReadsGuestRootfs reports whether containerd's own oci.WithImageConfig can be
// used as it stands on this host.
//
// It can on Linux, where the host mounts the guest's filesystem for real, and on macOS, where
// containerd's internal guards skip the attempt. Only Windows is left, and only because
// containerd never anticipated a Linux spec being generated there. Deciding by what is
// *excluded* rather than by what is included is deliberate: adding a host to this list is a
// claim about a platform, and the platform this project can make claims about is the one it
// leaves alone.
func containerdReadsGuestRootfs() bool { return runtime.GOOS != "windows" }

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
		// Handed over verbatim rather than resolved. Username is the field containerd
		// itself uses for exactly this case — an image user that only the guest can
		// interpret — and guessing a uid here would be the one failure that must not
		// happen quietly: an image saying "USER node" run as root instead.
		s.Process.User.Username = config.User
		s.Process.User.AdditionalGids = nil
		ensureAdditionalGIDs(s)
	}
	return nil
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
