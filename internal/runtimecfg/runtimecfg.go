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
	if addr := os.Getenv("BOKS_CONTAINERD_ADDRESS"); addr != "" {
		return addr
	}
	return defaults.DefaultAddress
}

// connectTimeout bounds how long a containerd dial may take. Diagnostics should be fast:
// waiting out a full gRPC backoff to learn the daemon is absent is not useful.
const connectTimeout = 3 * time.Second

// Connect opens a containerd client against address.
//
// A missing UNIX socket is reported immediately rather than after a dial timeout, since
// "containerd is not running" is the most common case and the slowest to discover.
func Connect(ctx context.Context, address string) (*client.Client, error) {
	if runtime.GOOS != "windows" && !strings.Contains(address, "://") {
		if _, err := os.Stat(address); err != nil {
			return nil, fmt.Errorf("containerd socket %s: %w", address, err)
		}
	}
	return client.New(address, client.WithTimeout(connectTimeout))
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
