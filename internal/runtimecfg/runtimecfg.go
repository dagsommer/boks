// Package runtimecfg holds the defaults describing how Boks reaches containerd and which
// VM runtime it drives, plus the small helpers shared by the CLI and the doctor checks.
//
// It exists so that the diagnostic path and the execution path agree on what "correct"
// means: doctor probes exactly the runtime, snapshotter and address that a run will use.
package runtimecfg

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// Runtime is the containerd runtime handler that gives each sandbox its own
	// microVM. Anything else is not a VM boundary.
	Runtime = "io.containerd.nerdbox.v1"

	// Snapshotter is the snapshotter the VM runtime needs. The guest mounts the image
	// as an EROFS filesystem rather than through a host overlay.
	Snapshotter = "erofs"

	// Namespace keeps Boks resources separate from other containerd users.
	Namespace = "boks"

	// snapshotterPluginType is the containerd plugin type for snapshotters.
	snapshotterPluginType = "io.containerd.snapshotter.v1"
)

// DefaultAddress returns the containerd socket Boks talks to, honouring
// BOKS_CONTAINERD_ADDRESS.
//
// The default comes from containerd's own defaults rather than a literal, because it is
// platform-specific: /run/containerd on Linux, /var/run/containerd on macOS (which has no
// /run at all), and a named pipe on Windows. Hardcoding the Linux path made every macOS
// run fail to connect.
func DefaultAddress() string {
	if addr, ok := AddressOverride(); ok {
		return addr
	}
	return defaults.DefaultAddress
}

// AddressOverride returns the address the user pinned through the environment, and whether
// they pinned one.
//
// It is separate from DefaultAddress because there is now a third answer between the two —
// the endpoint of a containerd that `boks daemon` is managing, which internal/daemon supplies
// — and it must sit *below* an explicit override rather than above it. Asking this question
// directly is what lets that ordering be written once, in daemon.DefaultAddress, instead of
// being re-derived by comparing DefaultAddress's result against the platform constant.
func AddressOverride() (string, bool) {
	addr := os.Getenv("BOKS_CONTAINERD_ADDRESS")
	return addr, addr != ""
}

// guestOS is the operating system inside every Boks sandbox.
//
// It is a constant rather than the host's because a microVM booting a Linux kernel can
// execute Linux binaries and nothing else. The host decides the *architecture* — an amd64
// machine cannot run an arm64 guest at any useful speed, and an arm64 one cannot run amd64
// at all — but it does not get a say in the operating system.
const guestOS = "linux"

// GuestPlatform is the OCI platform of everything that runs inside a sandbox: the image
// Boks pulls, the rootfs it unpacks, and the spec it generates.
//
// It exists because containerd's own answer is the *host's* platform, and on Windows that
// answer is wrong in a way that fails late and reads like a missing image. platforms.Default()
// returns linux/<arch> on Linux and — because a Mac can run Linux binaries under a VM —
// darwin/<arch> ahead of linux/<arch> on macOS, so both hosts happen to resolve a Linux
// manifest. On Windows it returns windows/<arch> with an OSVersion attached, which matches no
// Linux manifest at all: `boks run` would report that an image every other tool can pull does
// not exist for this platform.
//
// The same value is what the OCI spec is generated for, and that half has already been paid
// for once: generating the spec for the host's platform on macOS produced a Darwin spec with
// no Linux section, and the image config could not be applied to it at all.
//
// Everything that talks to containerd therefore asks for this instead of the host's default —
// see Connect, which sets it on the client so that GetImage, Unpack and image config
// resolution all agree with the pull.
func GuestPlatform() string { return guestOS + "/" + runtime.GOARCH }

// guestPlatformMatcher is GuestPlatform as containerd's manifest matcher.
//
// platforms.Only, not OnlyStrict: Only is what platforms.Default() uses on Unix, and it is
// what makes an arm64 host accept the arm64/v8 spelling and an amd64 host accept a 386 image.
// Matching the strictness of the thing being replaced is the point — this is a change of
// *which* platform is asked for, not of how tolerantly it is matched.
func guestPlatformMatcher() platforms.MatchComparer {
	return platforms.Only(ocispec.Platform{OS: guestOS, Architecture: runtime.GOARCH})
}

// connectTimeout bounds how long a containerd dial may take. Diagnostics should be fast:
// waiting out a full gRPC backoff to learn the daemon is absent is not useful.
const connectTimeout = 3 * time.Second

// Connect opens a containerd client against address.
//
// A missing UNIX socket is reported immediately rather than after a dial timeout, since
// "containerd is not running" is the most common case and the slowest to discover. The stat
// is skipped on Windows, where the address is the named pipe \\.\pipe\containerd-containerd
// rather than a path on disk.
//
// The client's default platform is the guest's, not the host's. That default is what
// GetImage, and therefore IsUnpacked, Unpack and the image config the OCI spec is built from,
// resolve manifests with — and unlike the pull there is no per-call option to override it. A
// client left on platforms.Default() would, on a Windows host, hold an image object that
// cannot find its own Linux manifest.
func Connect(ctx context.Context, address string) (*client.Client, error) {
	if runtime.GOOS != "windows" && !strings.Contains(address, "://") {
		if _, err := os.Stat(address); err != nil {
			return nil, fmt.Errorf("containerd socket %s: %w", address, err)
		}
	}
	return client.New(address,
		client.WithTimeout(connectTimeout),
		client.WithDefaultPlatform(guestPlatformMatcher()),
	)
}

// Snapshotters lists the snapshotters containerd has successfully initialised. Plugins that
// failed to initialise are excluded: containerd reports them, but using one fails at
// snapshot time with a less obvious error.
func Snapshotters(ctx context.Context, c *client.Client) ([]string, error) {
	resp, err := c.IntrospectionService().Plugins(ctx, "type=="+snapshotterPluginType)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range resp.Plugins {
		if p.InitErr == nil {
			names = append(names, p.ID)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ShimBinary derives the shim executable name containerd will look for on its PATH when
// asked for a runtime handler.
//
// containerd maps a handler of the form io.containerd.<name>.<version> to the binary
// containerd-shim-<name>-<version>. Returns "" if the handler does not follow that shape.
func ShimBinary(handler string) string {
	parts := strings.Split(handler, ".")
	if len(parts) < 4 || parts[0] != "io" || parts[1] != "containerd" {
		return ""
	}
	name := strings.Join(parts[2:len(parts)-1], ".")
	version := parts[len(parts)-1]
	if name == "" || version == "" {
		return ""
	}
	binary := fmt.Sprintf("containerd-shim-%s-%s", name, version)
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	return binary
}

// IsolatedRuntime reports whether a runtime handler provides a virtual machine boundary.
//
// Boks refuses to present container-only runtimes as sandboxes without saying so, since the
// entire point of the tool is the hypervisor boundary.
func IsolatedRuntime(handler string) bool {
	return handler == Runtime
}
