package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/dagsommer/boks/internal/workspace"
)

// Labels Boks writes onto every container it creates.
//
// containerd is the state store. A container record already carries the sandbox's name,
// image, runtime, snapshotter, creation time and full OCI spec, so Boks keeps no host-side
// database: there is nothing to fall out of sync, nothing left behind when a sandbox is
// removed by other means, and no per-user file to migrate between machines. These labels
// carry the two facts the container record cannot express — which host directories the
// sandbox was created for, and what command `boks run` should execute in it by default.
const (
	// LabelManaged marks a container as a Boks sandbox. Anything in the namespace
	// without it was created by something else and is left alone.
	LabelManaged = "dev.boks.managed"
	// LabelWorkspaces is the JSON list of workspaces the sandbox was created with.
	// The OCI spec also holds the mounts, but not which of them are workspaces, nor
	// their read-only intent in a form worth re-deriving.
	LabelWorkspaces = "dev.boks.workspaces"
	// LabelCommand is the JSON argv `boks run` executes when given no command of its
	// own: the command from `boks create ... -- cmd`, else the image's default.
	LabelCommand = "dev.boks.command"
	// LabelEphemeral marks a sandbox that is removed when its command exits, so `ls`
	// can say so rather than showing it as an ordinary sandbox about to vanish.
	LabelEphemeral = "dev.boks.ephemeral"
)

// NamePrefix begins every derived sandbox name, so that a name's origin is obvious wherever
// it shows up — including in containerd's own tooling.
const NamePrefix = "boks-"

// nameDigestBytes is how much of the workspace path digest ends up in a derived name.
// Twelve hex characters is far more than enough to keep the directories on one machine
// apart, and short enough to read and type.
const nameDigestBytes = 6

// containerdName is containerd's own identifier grammar. Rejecting a bad name here gives a
// better message than containerd's, which surfaces late and mentions its regexp.
var containerdName = regexp.MustCompile(`^[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$`)

// maxNameLength matches containerd's identifier limit.
const maxNameLength = 76

// DeriveName returns the sandbox name for a workspace path.
//
// Identity is by workspace path, hashed. `boks run ~/src/foo` twice must reach the same
// sandbox — that is the behaviour Docker Sandboxes has and the reason a sandbox is worth
// keeping — and the workspace path is the only thing both invocations share. The path
// cannot be the containerd identifier directly (slashes and spaces are not allowed, and
// the identifier is capped at 76 characters), so its digest is.
//
// An explicit -name overrides this, which is what lets one workspace hold several sandboxes
// and lets a sandbox be reached from any directory. The human-readable path is kept in a
// label so `boks ls` can show it.
func DeriveName(hostPath string) string {
	sum := sha256.Sum256([]byte(hostPath))
	return NamePrefix + hex.EncodeToString(sum[:nameDigestBytes])
}

// ValidateName reports whether a user-supplied sandbox name is usable as a containerd
// identifier.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("sandbox name is empty")
	}
	if len(name) > maxNameLength {
		return fmt.Errorf("sandbox name %q is longer than %d characters", name, maxNameLength)
	}
	if !containerdName.MatchString(name) {
		return fmt.Errorf("sandbox name %q is invalid: use letters and digits, "+
			"separated by '.', '_' or '-'", name)
	}
	return nil
}

// WorkspaceRef is a workspace as recorded on a sandbox. It mirrors workspace.Workspace but
// carries JSON names, since it is both stored in a label and printed by `boks inspect`.
type WorkspaceRef struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	Mode      string `json:"mode"`
}

func workspaceRefs(workspaces []workspace.Workspace) []WorkspaceRef {
	refs := make([]WorkspaceRef, 0, len(workspaces))
	for _, ws := range workspaces {
		refs = append(refs, WorkspaceRef{
			HostPath:  ws.HostPath,
			GuestPath: ws.GuestPath,
			Mode:      string(ws.Mode),
		})
	}
	return refs
}

// encodeLabel serialises a value for a container label. Labels are strings, so structured
// values travel as JSON.
func encodeLabel(v any) (string, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding sandbox metadata: %w", err)
	}
	return string(buf), nil
}

// decodeWorkspaces reads the workspace label. A sandbox with an unreadable or absent label
// is still usable, so a decode failure yields no workspaces rather than an error.
func decodeWorkspaces(labels map[string]string) []WorkspaceRef {
	var refs []WorkspaceRef
	if raw := labels[LabelWorkspaces]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &refs)
	}
	return refs
}

func decodeCommand(labels map[string]string) []string {
	var cmd []string
	if raw := labels[LabelCommand]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &cmd)
	}
	return cmd
}
