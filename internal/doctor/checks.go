package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dagsommer/boks/internal/runtimecfg"
)

const probeTimeout = 5 * time.Second

// containerdCheck confirms the daemon is reachable and reports its version. Reachability is
// tested with a real API call rather than a socket stat, because a stale socket file is a
// common failure mode.
func containerdCheck() Check {
	return Check{
		Name: "containerd",
		Run: func(ctx context.Context, env Env) Result {
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()

			client, err := runtimecfg.Connect(ctx, env.ContainerdAddress)
			if err != nil {
				return containerdFailure(env.ContainerdAddress, err)
			}
			defer client.Close()

			version, err := client.Version(ctx)
			if err != nil {
				return containerdFailure(env.ContainerdAddress, err)
			}
			return Result{Status: StatusOK, Detail: fmt.Sprintf("%s at %s", version.Version, env.ContainerdAddress)}
		},
	}
}

func containerdFailure(address string, err error) Result {
	res := Result{Status: StatusFail, Detail: "unreachable at " + address}

	if _, statErr := os.Stat(address); errors.Is(statErr, fs.ErrNotExist) {
		res.Remedy = fmt.Sprintf("No containerd socket at %s.\n"+
			"Install and start containerd, or point Boks elsewhere with\n"+
			"--containerd-address / BOKS_CONTAINERD_ADDRESS.", address)
		return res
	}
	if errors.Is(err, fs.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
		res.Remedy = fmt.Sprintf("Permission denied connecting to %s.\n"+
			"containerd's socket is usually root-owned. Either run Boks with sufficient\n"+
			"privileges or use a rootless containerd, which is the recommended setup.", address)
		return res
	}
	res.Remedy = fmt.Sprintf("Could not talk to containerd at %s: %v", address, err)
	return res
}

// snapshotterCheck confirms the snapshotter the VM runtime needs is present and usable.
// nerdbox requires erofs on macOS and prefers it on Linux, since the guest mounts the
// image as a block-backed filesystem rather than through an overlay on the host.
func snapshotterCheck() Check {
	return Check{
		Name: "snapshotter",
		Run: func(ctx context.Context, env Env) Result {
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()

			client, err := runtimecfg.Connect(ctx, env.ContainerdAddress)
			if err != nil {
				return Result{
					Status: StatusSkip,
					Detail: "not checked (containerd unreachable)",
				}
			}
			defer client.Close()

			available, err := runtimecfg.Snapshotters(ctx, client)
			if err != nil {
				return Result{Status: StatusWarn, Detail: "could not list snapshotters",
					Remedy: fmt.Sprintf("Listing snapshotters failed: %v", err)}
			}
			for _, name := range available {
				if name == env.Snapshotter {
					return Result{Status: StatusOK, Detail: env.Snapshotter + " available"}
				}
			}
			return Result{
				Status: StatusFail,
				Detail: env.Snapshotter + " unavailable",
				Remedy: fmt.Sprintf("The %s runtime needs the %q snapshotter, but containerd offers: %s.\n"+
					"Enable it in containerd's config.toml. erofs also needs mkfs.erofs from\n"+
					"erofs-utils on the host.",
					env.Runtime, env.Snapshotter, strings.Join(available, ", ")),
			}
		},
	}
}

// snapshotterToolsCheck looks for the host binaries a snapshotter shells out to.
//
// containerd reports the erofs snapshotter as initialised even when mkfs.erofs is absent;
// the failure only appears when an image is unpacked, as an opaque exec error deep in a
// pull. Checking up front turns that into an actionable message.
func snapshotterToolsCheck() Check {
	return Check{
		Name: "snapshotter tools",
		Run: func(ctx context.Context, env Env) Result {
			required, ok := snapshotterBinaries[env.Snapshotter]
			if !ok {
				return Result{Status: StatusSkip, Detail: "no host tools needed for " + env.Snapshotter}
			}
			var missing []string
			var found []string
			for _, binary := range required {
				path, err := exec.LookPath(binary)
				if err != nil {
					missing = append(missing, binary)
					continue
				}
				found = append(found, path)
			}
			if len(missing) > 0 {
				return Result{
					Status: StatusFail,
					Detail: strings.Join(missing, ", ") + " not found on PATH",
					Remedy: fmt.Sprintf("The %q snapshotter builds image layers with %s, which is not installed.\n"+
						"Without it, pulling an image fails partway through with an exec error.\n"+
						"Install erofs-utils (apt: erofs-utils, brew: erofs-utils).",
						env.Snapshotter, strings.Join(missing, " and ")),
				}
			}
			return Result{Status: StatusOK, Detail: strings.Join(found, ", ")}
		},
	}
}

// snapshotterBinaries maps a snapshotter to the host executables it requires.
var snapshotterBinaries = map[string][]string{
	"erofs": {"mkfs.erofs"},
}

// runtimeShimCheck looks for the shim binary that implements the VM runtime. containerd
// resolves a runtime handler such as io.containerd.nerdbox.v1 to the executable
// containerd-shim-nerdbox-v1 on its PATH, so absence of that binary is the single most
// common reason a VM-backed run fails.
func runtimeShimCheck() Check {
	return Check{
		Name: "vm runtime",
		Run: func(ctx context.Context, env Env) Result {
			binary := runtimecfg.ShimBinary(env.Runtime)
			if binary == "" {
				return Result{
					Status: StatusWarn,
					Detail: "unrecognised runtime " + env.Runtime,
					Remedy: "Boks cannot derive a shim binary name from this runtime handler,\n" +
						"so it cannot verify the runtime is installed.",
				}
			}

			path, err := exec.LookPath(binary)
			if err != nil {
				return Result{
					Status: StatusFail,
					Detail: binary + " not found on PATH",
					Remedy: fmt.Sprintf("Boks starts sandboxes through the %q runtime, which containerd\n"+
						"resolves to the executable %q on its PATH.\n"+
						"Build it from https://github.com/containerd/nerdbox and install the\n"+
						"binary where containerd can find it.\n"+
						"Note that containerd's PATH is the daemon's, not your shell's.",
						env.Runtime, binary),
				}
			}
			return Result{Status: StatusOK, Detail: path}
		},
	}
}

// hypervisorLibraryCheck looks for libkrun, the VMM the shim links against. The shim can be
// installed without it, and the resulting failure at VM boot is opaque, so it is worth
// reporting separately.
func hypervisorLibraryCheck() Check {
	return Check{
		Name: "hypervisor library",
		Run: func(ctx context.Context, env Env) Result {
			names := hypervisorLibraryNames()
			if len(names) == 0 {
				return Result{Status: StatusSkip, Detail: "not applicable on this platform"}
			}
			for _, dir := range hypervisorLibrarySearchPaths() {
				for _, name := range names {
					candidate := filepath.Join(dir, name)
					if _, err := os.Stat(candidate); err == nil {
						return Result{Status: StatusOK, Detail: candidate}
					}
				}
			}
			return Result{
				Status: StatusWarn,
				Detail: names[0] + " not found",
				Remedy: fmt.Sprintf("Could not find %s in the usual locations.\n"+
					"The VM runtime links against libkrun (>= 1.18) to boot microVMs.\n"+
					"If it is installed elsewhere on the loader's search path this warning\n"+
					"is harmless; Boks does not parse the dynamic loader configuration.",
					strings.Join(names, " or ")),
			}
		},
	}
}

// splitList splits a PATH-style list, dropping empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range filepath.SplitList(value) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
