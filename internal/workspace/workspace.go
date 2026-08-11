// Package workspace resolves host directories into guest mount definitions.
//
// Boks preserves the workspace's absolute host path inside the guest: a project at
// /home/alice/src/foo on the host appears at /home/alice/src/foo in the sandbox. Build
// output, stack traces and tool configuration therefore keep referring to paths that exist.
//
// Only the selected directory is shared. Parent directories exist inside the guest purely
// as mount points and expose nothing from the host.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode controls whether the guest may write to a workspace.
type Mode string

const (
	ModeReadWrite Mode = "rw"
	ModeReadOnly  Mode = "ro"
)

// Workspace is a host directory shared into a sandbox at the same absolute path.
type Workspace struct {
	// HostPath is the absolute, symlink-resolved path on the host.
	HostPath string
	// GuestPath is where the directory appears inside the guest. It equals HostPath
	// whenever exact-path sharing is possible.
	GuestPath string
	Mode      Mode
}

// Parse interprets a CLI workspace argument of the form "path" or "path:ro".
//
// The path is resolved against the current directory, cleaned, and checked to be an
// existing directory. Symlinks are resolved so that the guest path matches what the
// filesystem share actually exposes.
func Parse(arg string) (Workspace, error) {
	path, mode := arg, ModeReadWrite

	// Only treat a trailing ":ro"/":rw" as a mode. A bare colon elsewhere is part of
	// the path, which is legal on Unix.
	if idx := strings.LastIndex(arg, ":"); idx > 0 {
		switch Mode(arg[idx+1:]) {
		case ModeReadOnly:
			path, mode = arg[:idx], ModeReadOnly
		case ModeReadWrite:
			path, mode = arg[:idx], ModeReadWrite
		}
	}

	if path == "" {
		return Workspace{}, fmt.Errorf("empty workspace path")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolving workspace %q: %w", path, err)
	}

	// EvalSymlinks also reports non-existence, but with a less direct message, so check
	// the directory first for a better error.
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return Workspace{}, fmt.Errorf("workspace %q does not exist", abs)
		}
		return Workspace{}, fmt.Errorf("inspecting workspace %q: %w", abs, err)
	}
	if !info.IsDir() {
		// Sharing a single file would expose its whole parent directory to the guest,
		// because that is how the runtime implements file bind mounts. Refuse instead.
		return Workspace{}, fmt.Errorf("workspace %q is not a directory; Boks shares directories only", abs)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolving symlinks for %q: %w", abs, err)
	}

	return Workspace{HostPath: resolved, GuestPath: resolved, Mode: mode}, nil
}

// Root reports the guest path a shell should start in.
func (w Workspace) Root() string { return w.GuestPath }

// ReadOnly reports whether the guest gets a read-only view.
func (w Workspace) ReadOnly() bool { return w.Mode == ModeReadOnly }

// MountOptions returns the OCI mount options for this workspace.
//
// rbind propagates any submounts under the workspace; rprivate stops mount events from
// travelling back to the host.
func (w Workspace) MountOptions() []string {
	opts := []string{"rbind", "rprivate"}
	if w.ReadOnly() {
		return append(opts, "ro")
	}
	return append(opts, "rw")
}
