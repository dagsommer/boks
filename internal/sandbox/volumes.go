package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/workspace"
)

// Volume is a mount a sandbox is created with, from a kit's `volumes`.
//
// Fixed at creation: it becomes part of the container's OCI spec, so a kit added to an
// existing sandbox cannot attach one.
type Volume struct {
	// Path is the absolute guest path.
	Path string
	// Tmpfs selects a RAM-backed mount. Otherwise the volume is a directory under the state
	// directory, shared into the guest: it survives stop/start and is deleted by Remove.
	Tmpfs bool
	// Size is the limit in bytes, 0 for none. Enforced for tmpfs only; a shared directory
	// has no quota, which the CLI says when a kit asks for one.
	Size int64
	// Mode is the octal permission (with sticky/setuid bits), 0 for the default.
	Mode uint32
}

// volumesRoot is where every sandbox's block-backed volumes live.
func volumesRoot(stateDir string) string { return filepath.Join(stateDir, "volumes") }

// VolumeDir is the host directory backing a block volume. One directory per sandbox, one
// subdirectory per guest path, named by a hash of it so any path is a safe file name.
func VolumeDir(stateDir, sandbox, guestPath string) string {
	sum := sha256.Sum256([]byte(guestPath))
	return filepath.Join(volumesRoot(stateDir), sandbox, hex.EncodeToString(sum[:6]))
}

// volumeShares are the block volumes as writable shares, so they take the same mount options
// and the same uid/gid reconciliation as a workspace.
func volumeShares(cfg Config) []workspace.Workspace {
	var shares []workspace.Workspace
	for _, v := range cfg.Volumes {
		if v.Tmpfs {
			continue
		}
		shares = append(shares, workspace.Workspace{
			HostPath:  VolumeDir(policy.StateDir(), cfg.Name, v.Path),
			GuestPath: v.Path,
			Mode:      workspace.ModeReadWrite,
		})
	}
	return shares
}

// tmpfsMounts are the RAM-backed volumes as OCI tmpfs mounts.
func tmpfsMounts(vols []Volume) []specs.Mount {
	var out []specs.Mount
	for _, v := range vols {
		if !v.Tmpfs {
			continue
		}
		opts := []string{"nosuid", "nodev"}
		if v.Size > 0 {
			opts = append(opts, "size="+strconv.FormatInt(v.Size, 10))
		}
		if v.Mode != 0 {
			opts = append(opts, "mode="+strconv.FormatUint(uint64(v.Mode), 8))
		}
		out = append(out, specs.Mount{Type: "tmpfs", Source: "tmpfs", Destination: v.Path, Options: opts})
	}
	return out
}

// prepareVolumes creates the host directories for block volumes. Idempotent.
func prepareVolumes(cfg Config) error {
	for _, v := range cfg.Volumes {
		if v.Tmpfs {
			continue
		}
		dir := VolumeDir(policy.StateDir(), cfg.Name, v.Path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating volume %s: %w", v.Path, err)
		}
		if v.Mode != 0 {
			if err := os.Chmod(dir, os.FileMode(v.Mode&0o777)|modeBits(v.Mode)); err != nil {
				return fmt.Errorf("setting the mode of volume %s: %w", v.Path, err)
			}
		}
	}
	return nil
}

// modeBits maps the octal setuid/setgid/sticky bits (04000, 02000, 01000) to os.FileMode's.
func modeBits(m uint32) os.FileMode {
	var f os.FileMode
	if m&0o4000 != 0 {
		f |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		f |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		f |= os.ModeSticky
	}
	return f
}

// removeVolumes deletes a sandbox's block volumes. A missing directory is not an error, and
// a name that is not a single path element is ignored rather than trusted.
func removeVolumes(stateDir, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return nil
	}
	return os.RemoveAll(filepath.Join(volumesRoot(stateDir), name))
}
