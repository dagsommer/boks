package sandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dagsommer/boks/internal/policy"
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
	// LabelAgent is the agent the sandbox was created for. It is the half of the
	// sandbox's identity the container record cannot express, and `ls` has a column for
	// it; it is also what lets `boks run -name x` work without naming an agent.
	LabelAgent = "dev.boks.agent"
	// LabelPolicy is how the sandbox's network policy was chosen: the profile, the
	// preset, and the per-run allow and deny rules it was created with, as JSON.
	//
	// Without it, `boks start` and `boks exec` had nothing to go on and served a sandbox
	// the default preset instead of the policy it was created under — a containment that
	// silently widened on restart. The stored global and per-sandbox rules are
	// deliberately *not* copied here: they are re-read from the policy store, so a rule
	// added after a sandbox was created still reaches it.
	//
	// It holds destinations and preset names. It holds no credential and no secret.
	LabelPolicy = "dev.boks.policy"
	// LabelPorts is the JSON list of `-p/--publish` specifications the sandbox was
	// created with, as the user typed them.
	//
	// It is recorded for the same reason the policy is: `boks start` has no port flags,
	// and a sandbox that forgot its published ports on restart would leave the user
	// wondering why a bookmark stopped working. It is the *request*, not the result — a
	// specification with no host port asks for an ephemeral one, and which one it gets is
	// decided afresh each time the sandbox comes up.
	LabelPorts = "dev.boks.ports"
	// LabelFilesystem is how the workspace reaches the guest: direct, or a clone of the
	// host repository made inside the guest. It is a versioned JSON record; see
	// Filesystem in clone.go.
	//
	// Absent means direct mode, which is what every sandbox created before clone mode
	// existed is in, and is also the reading that claims the least. The mode is fixed
	// when a sandbox is created because it lives in the OCI mounts, so this label is
	// both what `boks ls` reports and what a re-attach checks a `--clone` against.
	LabelFilesystem = "dev.boks.filesystem"
)

// maxLabelBytes is containerd's limit on the size of one label's key and value together.
// Nothing here is normally near it — a policy record is a few dozen bytes — but a run with
// a hundred `-allow` flags would be, and a sandbox that failed to record its policy would be
// a sandbox that quietly forgot it. The limit is met with an error rather than with
// truncation, which would be the same failure with the evidence removed.
const maxLabelBytes = 4096

// containerdName is containerd's own identifier grammar: alphanumeric runs joined by single
// '.', '_' or '-' separators. Rejecting a bad name here gives a better message than
// containerd's, which surfaces late and mentions its regexp.
var containerdName = regexp.MustCompile(`^[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$`)

// maxNameLength matches containerd's identifier limit.
const maxNameLength = 76

// digestChars is how much of a workspace path digest ends up in a name that needs one. Six
// hex characters distinguish the directories on one machine and stay short enough to type.
const digestChars = 6

// DeriveName returns the readable sandbox name for an agent and a workspace:
// `<agent>-<workspace directory name>`, which is the name sbx uses.
//
// Naming and re-attach are the same mechanism. `boks run claude ~/src/foo` twice must reach
// one sandbox, and it does so by deriving one name from the two things both invocations
// share — the agent and the workspace. There is no separate identity: the name *is* the
// identity, which is why it must be derived rather than generated.
//
// Case, dots and existing hyphens are kept, so ~/git_repos/finndato.no gives
// claude-finndato.no. Anything containerd would reject is folded away by sanitiseSegment.
// An explicit -name overrides the whole scheme; that is what lets one workspace hold several
// sandboxes and lets a sandbox be reached from any directory.
func DeriveName(agent, hostPath string) (string, error) {
	return composeName(agent, hostPath, "")
}

// QualifiedName is DeriveName with a short digest of the workspace path appended. It is the
// name used when the readable one is already taken by a *different* directory — see
// ChooseName, which is where that decision is made.
func QualifiedName(agent, hostPath string) (string, error) {
	return composeName(agent, hostPath, pathDigest(hostPath))
}

// EphemeralName is a unique name for a sandbox that has no lasting identity.
//
// A `-rm` run must not take the name of the workspace's persistent sandbox, nor of another
// run happening at the same time, so the suffix is random rather than derived. It is longer
// than a path digest so the two cannot be mistaken for one another.
func EphemeralName(agent, hostPath string) (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating sandbox name: %w", err)
	}
	return composeName(agent, hostPath, hex.EncodeToString(buf))
}

// composeName joins the parts, keeping the result inside containerd's length limit.
//
// The workspace half is what gets truncated: the agent half is short, chosen by the user in
// this invocation, and the part that says what the sandbox is. A truncated name could
// collide with another long path that shares a prefix, so truncation always brings a digest
// with it — a name too long to be readable in full is still a name that has to be unique.
func composeName(agent, hostPath, digest string) (string, error) {
	if err := validateSegment(agent); err != nil {
		return "", fmt.Errorf("agent name %q cannot start a sandbox name: %w", agent, err)
	}

	segment := workspaceSegment(hostPath)
	if segment == "" {
		return "", fmt.Errorf("workspace %q has no name to derive a sandbox name from; pass -name", hostPath)
	}

	budget := maxNameLength - len(agent) - 1
	if digest != "" {
		budget -= len(digest) + 1
	}
	if budget < 1 {
		return "", fmt.Errorf("agent name %q leaves no room for a workspace name; pass -name", agent)
	}
	if len(segment) > budget {
		if digest == "" {
			// Truncating without a digest would let two long paths that share a
			// prefix share a name, so switch to the qualified form instead.
			return composeName(agent, hostPath, pathDigest(hostPath))
		}
		segment = trimSeparators(segment[:budget])
		if segment == "" {
			segment, digest = digest, ""
		}
	}

	name := agent + "-" + segment
	if digest != "" {
		name += "-" + digest
	}
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return name, nil
}

// workspaceSegment turns a workspace path into the readable half of a sandbox name.
//
// Two cases the rule leaves open are decided here. A workspace at a filesystem root has no
// basename worth using, so it is called "root": there is only one of them per machine, so
// the name stays unique. A basename with nothing usable left after sanitising — "…", or a
// directory named entirely in non-ASCII script — falls back to a digest of the path, which
// is unreadable but correct, and still re-attaches.
func workspaceSegment(hostPath string) string {
	if hostPath == "" {
		return ""
	}
	if filepath.Dir(hostPath) == hostPath {
		return "root"
	}
	if segment := sanitiseSegment(filepath.Base(hostPath)); segment != "" {
		return segment
	}
	return pathDigest(hostPath)
}

// sanitiseSegment folds a directory name into containerd's identifier grammar.
//
// containerd permits ASCII alphanumerics joined by single '.', '_' or '-' separators, and
// nothing else — no spaces, no slashes, no repeated separators, none at either end. Those
// are exactly the characters sbx's naming rule preserves, so ordinary project directories
// pass through unchanged; anything else becomes a single '-', and runs of separators
// collapse so that "my  project" and "my--project" both give "my-project".
func sanitiseSegment(s string) string {
	var b strings.Builder
	pendingSeparator := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			if pendingSeparator != 0 && b.Len() > 0 {
				b.WriteByte(pendingSeparator)
			}
			pendingSeparator = 0
			b.WriteByte(c)
		case c == '.' || c == '_' || c == '-':
			if pendingSeparator == 0 {
				pendingSeparator = c
			}
		default:
			// A byte containerd will not take, including every byte of a
			// multi-byte rune. One separator stands in for a run of them.
			if pendingSeparator == 0 {
				pendingSeparator = '-'
			}
		}
	}
	return b.String()
}

// trimSeparators removes separators from both ends, which containerd's grammar forbids.
func trimSeparators(s string) string { return strings.Trim(s, "._-") }

// pathDigest is a short, stable hash of a workspace path, used wherever a name needs to
// distinguish two directories that would otherwise produce the same one.
func pathDigest(hostPath string) string {
	sum := sha256.Sum256([]byte(hostPath))
	return hex.EncodeToString(sum[:])[:digestChars]
}

// validateSegment checks one half of a composed name.
func validateSegment(s string) error {
	if s == "" {
		return fmt.Errorf("it is empty")
	}
	if !containerdName.MatchString(s) {
		return fmt.Errorf("use letters and digits, separated by '.', '_' or '-'")
	}
	return nil
}

// Choice is the sandbox an invocation is about: which name it uses, whether that sandbox
// already exists, and — when the readable name was already taken by another directory —
// what it was taken by.
type Choice struct {
	Name   string
	Exists bool
	Info   Info
	// CollidedWith is the workspace holding the readable name, set only when this
	// invocation had to fall back to the qualified one.
	CollidedWith string
}

// ChooseName decides which sandbox name an agent and workspace map to, given what already
// exists. lookup reports the sandbox with a given name, if any.
//
// The readable name is not unique: two directories can share a basename, which the old
// path-digest scheme made impossible. Of the three ways out — reuse the sandbox, refuse, or
// pick another name — only the third is both safe and usable. Reuse would silently attach
// the second project to the first project's sandbox, which has the wrong workspace mounted
// and possibly its files; refusing would leave the user stuck at a name they never chose.
// So the second directory gets `<agent>-<dir>-<digest>`, deterministically, and is told why.
//
// The qualified name is checked first, so a sandbox that was once bumped keeps its name even
// after the sandbox that bumped it is removed.
func ChooseName(agent, hostPath string, lookup func(name string) (Info, bool)) (Choice, error) {
	preferred, err := DeriveName(agent, hostPath)
	if err != nil {
		return Choice{}, err
	}
	qualified, err := QualifiedName(agent, hostPath)
	if err != nil {
		return Choice{}, err
	}

	if info, ok := lookup(qualified); ok {
		return Choice{Name: qualified, Exists: true, Info: info}, nil
	}
	info, ok := lookup(preferred)
	switch {
	case !ok:
		return Choice{Name: preferred}, nil
	case info.Workspace() == "" || info.Workspace() == hostPath:
		return Choice{Name: preferred, Exists: true, Info: info}, nil
	default:
		return Choice{Name: qualified, CollidedWith: info.Workspace()}, nil
	}
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

// decodePorts reads the publish specifications back. As with every other label, a sandbox
// whose record is absent or unreadable is not broken — it predates the record — and it comes
// up with nothing published rather than failing to come up.
func decodePorts(labels map[string]string) []string {
	var specs []string
	if raw := labels[LabelPorts]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &specs)
	}
	return specs
}

// encodePolicyLabel serialises a sandbox's policy record, refusing to write one containerd
// would reject. See maxLabelBytes for why this is an error rather than a truncation.
func encodePolicyLabel(p *policy.SandboxPolicy) (string, error) {
	if p.IsZero() {
		return "", nil
	}
	raw, err := encodeLabel(p)
	if err != nil {
		return "", err
	}
	if len(raw)+len(LabelPolicy) > maxLabelBytes {
		return "", fmt.Errorf("this sandbox's policy is too large to record on it (%d bytes, limit %d).\n"+
			"Rules a sandbox cannot remember would be lost the next time it starts, so boks refuses\n"+
			"rather than create it. Put them in the policy store instead, where there is no limit:\n"+
			"  boks policy allow -sandbox <name> <destination>", len(raw), maxLabelBytes-len(LabelPolicy))
	}
	return raw, nil
}

// decodePolicy reads the policy record back. A sandbox whose label is absent or unreadable
// is not broken — it predates the record, or was made by something else — and it gets the
// same treatment it got before the record existed: the default policy, plus whatever the
// store says about it.
func decodePolicy(labels map[string]string) *policy.SandboxPolicy {
	raw := labels[LabelPolicy]
	if raw == "" {
		return nil
	}
	var p policy.SandboxPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil
	}
	return &p
}
