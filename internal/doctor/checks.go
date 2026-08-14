package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		res.Remedy = fmt.Sprintf("No containerd %s at %s.\n"+
			"Install and start containerd, or point Boks elsewhere with\n"+
			"--containerd-address / BOKS_CONTAINERD_ADDRESS.",
			containerdEndpointNoun(address), address)
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

// containerdEndpointNoun names what the address is, so that the message does not call a
// Windows named pipe a socket. containerd's default address there is
// \\.\pipe\containerd-containerd; on Linux and macOS it is a Unix socket path.
func containerdEndpointNoun(address string) string {
	normalised := strings.ToLower(address)
	normalised = strings.TrimPrefix(normalised, "npipe://")
	normalised = strings.ReplaceAll(normalised, "/", `\`)
	if strings.HasPrefix(normalised, `\\.\pipe\`) {
		return "named pipe"
	}
	return "socket"
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

// snapshotterToolsCheck looks for the host binaries a snapshotter shells out to, and checks
// that they are new enough.
//
// containerd reports the erofs snapshotter as initialised even when mkfs.erofs is absent;
// the failure only appears when an image is unpacked, as an opaque exec error deep in a
// pull. Checking up front turns that into an actionable message. A too-old mkfs.erofs fails
// the same way and just as late, so presence alone is not enough to report "ok".
func snapshotterToolsCheck() Check {
	return snapshotterToolsCheckWith(runVersionProbe)
}

// snapshotterToolsCheckWith takes the version probe as a parameter so the version rules can
// be tested against constructed output rather than against whatever erofs-utils the machine
// running the tests happens to have.
func snapshotterToolsCheckWith(probe versionProbe) Check {
	return Check{
		Name: "snapshotter tools",
		Run: func(ctx context.Context, env Env) Result {
			required, ok := snapshotterBinaries[env.Snapshotter]
			if !ok {
				return Result{Status: StatusSkip, Detail: "no host tools needed for " + env.Snapshotter}
			}
			var missing []string
			found := map[string]string{}
			var paths []string
			for _, binary := range required {
				path, err := exec.LookPath(binary)
				if err != nil {
					missing = append(missing, binary)
					continue
				}
				found[binary] = path
				paths = append(paths, path)
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

			if path, ok := found["mkfs.erofs"]; ok {
				res := erofsVersionResult(ctx, path, probe)
				if res.Status != StatusOK {
					return res
				}
				return Result{
					Status: StatusOK,
					Detail: fmt.Sprintf("%s (erofs-utils %s)", strings.Join(paths, ", "), res.Detail),
				}
			}
			return Result{Status: StatusOK, Detail: strings.Join(paths, ", ")}
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
//
// It searches exactly the way the shim does — see libkrun.go — because anything else answers
// a different question. This check used to stat a list of prefixes of its own invention, and
// was wrong in both directions: it accepted libkrun.so.1, a name the shim never stats, and so
// gave a clean bill of health to a host that would fail at VM boot; and it never looked at
// PATH or LIBKRUN_PATH, so a libkrun the shim *would* load was reported missing.
func hypervisorLibraryCheck() Check {
	return Check{
		Name: "hypervisor library",
		Run: func(ctx context.Context, env Env) Result {
			return hypervisorLibraryResult(runtime.GOOS, runtime.GOARCH, os.Getenv, hostLibraryFS())
		},
	}
}

func hypervisorLibraryResult(goos, goarch string, getenv func(string) string, fsys libraryFS) Result {
	if goos != "linux" && goos != "darwin" {
		// Windows and everything else: the platform check already says why sandboxes
		// cannot start there, and a libkrun verdict on top of it would be noise about a
		// backend that does not exist yet. See virt_windows.go.
		return Result{Status: StatusSkip, Detail: "not applicable on this platform"}
	}

	names := nerdboxLibraryNames(goos, goarch)
	scan := scanForHypervisorLibrary(goos, goarch, getenv, fsys)
	if scan.Loadable != "" {
		return Result{Status: StatusOK, Detail: scan.Loadable}
	}

	// A miss is a warning rather than a failure for one reason: the search depends on
	// PATH and LIBKRUN_PATH, and the values that decide the outcome are containerd's, not
	// this shell's. doctor cannot read the daemon's environment, so it cannot prove the
	// shim will come up empty.
	const howItSearches = "The shim looks for\n  %s\n" +
		"in each PATH entry, then in LIBKRUN_PATH — or, when that is unset, in\n%s.\n" +
		"It opens the full path it built itself, so ld.so.conf and the SONAME do\n" +
		"not enter into it.\n"
	defaultDirs := "/usr/local/lib, /usr/local/lib64, /usr/lib and /lib"
	if goos == "darwin" {
		defaultDirs = "/usr/local/lib, /usr/local/lib64, /usr/lib, /lib and /opt/homebrew/lib"
	}
	searchText := fmt.Sprintf(howItSearches, strings.Join(names, ", "), defaultDirs)
	envCaveat := "Both variables are read from containerd's environment, since containerd spawns\n" +
		"the shim, and setting LIBKRUN_PATH replaces the defaults above rather than adding\n" +
		"to them. If libkrun is already somewhere the daemon's PATH or LIBKRUN_PATH covers,\n" +
		"this warning is harmless."

	if len(scan.NearMisses) > 0 {
		// A host can have a whole shelf of these; list enough to recognise the shape of
		// the problem and say how many were left out rather than filling the screen.
		const listed = 4
		var lines []string
		for i, miss := range scan.NearMisses {
			if i == listed {
				lines = append(lines, fmt.Sprintf("  … and %d more",
					len(scan.NearMisses)-listed))
				break
			}
			lines = append(lines, fmt.Sprintf("  %s\n    %s", miss.Path, miss.Reason))
		}
		fix := fmt.Sprintf("Point LIBKRUN_PATH at %s.\n",
			filepath.Dir(scan.NearMisses[0].Path))
		if target := scan.NearMisses[0].SymlinkTarget; target != "" {
			fix = fmt.Sprintf("Give it a name and a directory the shim uses — a symlink is enough:\n"+
				"  sudo ln -s %s %s\n"+
				"or point LIBKRUN_PATH at %s.\n",
				scan.NearMisses[0].Path, target, filepath.Dir(scan.NearMisses[0].Path))
		}
		return Result{
			Status: StatusWarn,
			Detail: "found " + scan.NearMisses[0].Path + ", which the shim will not load",
			Remedy: fmt.Sprintf("libkrun is on this host, where the VM runtime will not load it from:\n%s\n%s\n%s%s",
				strings.Join(lines, "\n"), searchText, fix, envCaveat),
		}
	}

	return Result{
		Status: StatusWarn,
		Detail: canonicalLibraryName(goos) + " not found where the shim looks",
		Remedy: fmt.Sprintf("Could not find libkrun anywhere the VM runtime looks.\n%s"+
			"Install libkrun (>= 1.18) into one of those directories.\n%s",
			searchText, envCaveat),
	}
}

// guestImageCheck looks for the two files the microVM actually boots: nerdbox's guest kernel
// and its erofs root filesystem.
//
// Neither is part of the shim, and nothing installs them — nerdbox's releases carry no assets
// and building them needs Docker, so they are the pieces most likely to be missing on a host
// where everything else is in place. Without them the shim aborts in NewInstance with
// "nerdbox-kernel not found in PATH or LIBKRUN_PATH", after doctor has said the host is ready:
// exactly the opaque, late failure this command exists to convert into a named one.
func guestImageCheck() Check {
	return Check{
		Name: "guest image",
		Run: func(ctx context.Context, env Env) Result {
			return guestImageResult(nerdboxSearchPaths(runtime.GOOS, os.Getenv))
		},
	}
}

// guestImageResult takes the directories to scan so tests can point it at a temporary tree
// rather than requiring a machine that has a real guest image installed.
func guestImageResult(dirs []string) Result {
	kernelName := guestKernelName()
	rootfsNames := guestRootfsNames()

	kernel := findInDirs(dirs, []string{kernelName})
	rootfs := findInDirs(dirs, rootfsNames)
	if kernel != "" && rootfs != "" {
		return Result{Status: StatusOK, Detail: kernel + ", " + rootfs}
	}

	var missing []string
	if kernel == "" {
		missing = append(missing, kernelName)
	}
	if rootfs == "" {
		// The unsuffixed name is what nerdbox's own build produces, so it is the one to
		// print; the remedy names the arch-suffixed alternative it also accepts.
		missing = append(missing, rootfsNames[len(rootfsNames)-1])
	}
	return Result{
		Status: StatusFail,
		Detail: strings.Join(missing, " and ") + " not found",
		Remedy: fmt.Sprintf("A sandbox boots nerdbox's guest kernel and erofs root filesystem. The shim\n"+
			"finds them by scanning containerd's PATH and LIBKRUN_PATH — the same scan it\n"+
			"uses for libkrun, not a look next to its own binary — and without them a VM\n"+
			"dies at boot with 'nerdbox-kernel not found in PATH or LIBKRUN_PATH'.\n"+
			"The names it accepts are:\n"+
			"  %s\n"+
			"  %s\n"+
			"Nothing packages these: nerdbox's GitHub releases carry no assets, and building\n"+
			"them is a Linux kernel build driven by 'docker buildx bake'.\n"+
			"Two routes. Download them from the newest successful run of the guest-image\n"+
			"workflow, which builds both on a Linux runner and attaches them with their\n"+
			"checksums — this needs no Docker and is the only practical route on Windows:\n"+
			"  https://github.com/dagsommer/boks/actions/workflows/guest-image.yml\n"+
			"Or build them yourself with scripts/build-nerdbox-guest.sh, which needs Docker\n"+
			"with buildx and takes a while. They are guest artefacts, so building them once\n"+
			"on any Docker host and copying the two files over is fine.\n"+
			"Put both in a directory on containerd's PATH or on LIBKRUN_PATH — on Apple\n"+
			"silicon, $(brew --prefix)/lib is already one. See docs/install.md.\n"+
			"Note that containerd's PATH is the daemon's, not your shell's.",
			kernelName, strings.Join(rootfsNames, " or ")),
	}
}

// guestArch is the architecture spelling nerdbox puts in the guest filenames, which is not
// Go's: its kernelArch() maps amd64 to x86_64 and passes everything else through.
func guestArch() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return runtime.GOARCH
}

// guestKernelName is the only kernel filename the shim looks for. There is no unsuffixed
// fallback, unlike the rootfs.
func guestKernelName() string { return "nerdbox-kernel-" + guestArch() }

// guestRootfsNames are the rootfs filenames the shim accepts, in the order it tries them:
// an arch-suffixed image first, then the unsuffixed name its own bake produces.
func guestRootfsNames() []string {
	return []string{"nerdbox-rootfs-" + guestArch() + ".erofs", "nerdbox-rootfs.erofs"}
}

// nerdboxSearchPaths returns the directories the shim scans for everything it loads at VM
// start — libkrun, the guest kernel, the guest rootfs — in the order it scans them.
//
// This mirrors NewInstance in nerdbox's internal/vm/libkrun/instance.go rather than
// approximating it. A check that looks in different places than the code it is checking for
// reports misses that are not misses and passes hosts that will fail, which is worse than not
// checking. The shim resolves an absolute path from this list and loads that path directly
// (syscall.LoadLibrary on Windows, dlopen on Unix), so for the guest files this list is the
// entire search.

func findInDirs(dirs, names []string) string {
	for _, dir := range dirs {
		for _, name := range names {
			candidate := filepath.Join(dir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}
