// Package workspace resolves host directories into guest mount definitions.
//
// Boks preserves the workspace's absolute host path inside the guest: a project at
// /home/alice/src/foo on the host appears at /home/alice/src/foo in the sandbox. Build
// output, stack traces and tool configuration therefore keep referring to paths that exist.
//
// On a Windows host that is impossible rather than merely awkward, because C:\Users\dag\src\foo
// is not a Linux path. There the host path is translated instead — C:\Users\dag\src\foo
// appears at /c/Users/dag/src/foo — reversibly, so the two sides still name each other, but
// absolute paths produced inside the sandbox are no longer openable on the host. guestpath.go
// is the only place that knows this; everything else reads Workspace.GuestPath.
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

// Workspace is a host directory shared into a sandbox, at the same absolute path wherever the
// host path is also a guest path.
type Workspace struct {
	// HostPath is the absolute, symlink-resolved path on the host.
	HostPath string
	// GuestPath is where the directory appears inside the guest. It equals HostPath
	// whenever exact-path sharing is possible, which is everywhere but Windows; see
	// guestPath.
	GuestPath string
	Mode      Mode
}

// Parse interprets a CLI workspace argument of the form "path" or "path:ro".
//
// The path is resolved against the current directory, cleaned, and checked to be an
// existing directory. Symlinks are resolved so that the guest path matches what the
// filesystem share actually exposes — and, on Windows, so that the mapping to a guest path
// sees a spelling the operating system has already canonicalised.
func Parse(arg string) (Workspace, error) {
	style := hostStyle()
	path, mode := splitMode(arg, style)

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

	// EvalSymlinks is load-bearing beyond symlinks on Windows: it is documented to return a
	// unique spelling, and its implementation uppercases the drive letter and replaces every
	// component with its on-disk case. That is what stops C:\Users\Dag\Repo and
	// c:\users\dag\repo — one directory on a case-insensitive filesystem — from becoming two
	// different paths in a case-sensitive guest. guestPath folds only the drive letter and
	// preserves the rest of the case, because by this point the rest is already canonical.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolving symlinks for %q: %w", abs, err)
	}

	guest, err := guestPath(resolved, style)
	if err != nil {
		return Workspace{}, err
	}

	return Workspace{HostPath: resolved, GuestPath: guest, Mode: mode}, nil
}

// splitMode separates a trailing ":ro"/":rw" from a workspace argument.
//
// Only a trailing mode counts. A bare colon elsewhere is part of the path, which is legal on
// Unix, and on Windows the colon at index 1 is the drive separator: reading `C:ro` as drive C
// shared read-only would share the whole drive instead of the drive-relative directory called
// "ro" that was asked for, which is a wrong answer rather than an error.
func splitMode(arg string, style pathStyle) (string, Mode) {
	idx := strings.LastIndex(arg, ":")
	if idx <= 0 || (style == styleWindows && idx == 1) {
		return arg, ModeReadWrite
	}
	switch Mode(arg[idx+1:]) {
	case ModeReadOnly:
		return arg[:idx], ModeReadOnly
	case ModeReadWrite:
		return arg[:idx], ModeReadWrite
	}
	return arg, ModeReadWrite
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
