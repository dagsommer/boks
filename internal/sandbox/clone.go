package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/containerd/containerd/v2/client"

	"github.com/dagsommer/boks/internal/workspace"
)

// Filesystem modes: how a sandbox's primary workspace reaches the guest.
const (
	// FilesystemDirect shares the host directory itself, read-write. Guest writes land
	// on the host's disk immediately. It is the default, and it is what every sandbox
	// created before clone mode existed is in.
	FilesystemDirect = "direct"
	// FilesystemClone shares the host repository read-only at workspace.SourcePath and
	// gives the guest a clone of it, in the guest's own filesystem, at the workspace's
	// host path. Nothing the guest writes reaches the host's disk.
	FilesystemClone = "clone"
)

// filesystemRecordVersion is the schema version of the label below. It is written from the
// first release of the feature rather than added later: the label is the only durable record
// of a security-relevant property, and a reader that cannot tell which shape it is holding
// has to guess about exactly the thing it must not guess about.
const filesystemRecordVersion = 1

// Filesystem is how a sandbox's workspace reaches the guest, as recorded on the container
// when it was created.
//
// It is fixed for the sandbox's lifetime, and for a reason stronger than convenience: the
// mode is expressed in OCI mounts, which containerd writes into the spec at creation and
// never revisits. A sandbox cannot change modes any more than it can change what it has
// mounted, so `--clone` on a re-attach is reported as ignored rather than applied.
type Filesystem struct {
	// Version is the record's schema version, so a later Boks can tell what it is
	// reading rather than inferring it from which fields happen to be set.
	Version int `json:"version"`
	// Mode is FilesystemDirect or FilesystemClone.
	Mode string `json:"mode"`
	// Source is where the host workspace appears in the guest, read-only. Empty in
	// direct mode, where the workspace appears at its own host path instead.
	Source string `json:"source,omitempty"`
	// Clone is the guest path holding the clone: the workspace's host path, which is
	// also the process's working directory. Empty in direct mode.
	Clone string `json:"clone,omitempty"`
}

// IsClone reports whether the guest works on a clone rather than on the host's files.
func (f Filesystem) IsClone() bool { return f.Mode == FilesystemClone }

// directFilesystem is what a sandbox with no record is in, which is also the truth about it.
func directFilesystem() Filesystem {
	return Filesystem{Version: filesystemRecordVersion, Mode: FilesystemDirect}
}

// cloneFilesystem is the record for a clone-mode sandbox over the given workspace.
func cloneFilesystem(ws workspace.Workspace) Filesystem {
	return Filesystem{
		Version: filesystemRecordVersion,
		Mode:    FilesystemClone,
		Source:  workspace.SourcePath,
		Clone:   ws.GuestPath,
	}
}

// encodeFilesystemLabel serialises the record. Only clone mode is written: direct is the
// absence of the label as well as its default, so a sandbox made before this existed and one
// made in direct mode today are the same thing rather than two things that read alike.
func encodeFilesystemLabel(f Filesystem) (string, error) {
	if !f.IsClone() {
		return "", nil
	}
	return encodeLabel(f)
}

// decodeFilesystem reads the record back. An absent or unreadable label is direct mode,
// which is both the historical behaviour and the safe reading: it claims no containment.
func decodeFilesystem(labels map[string]string) Filesystem {
	raw := labels[LabelFilesystem]
	if raw == "" {
		return directFilesystem()
	}
	var f Filesystem
	if err := json.Unmarshal([]byte(raw), &f); err != nil || !f.IsClone() {
		return directFilesystem()
	}
	return f
}

// cloneScript is the shell that produces the guest-local clone.
//
// It is idempotent — a sandbox that already holds its clone starts without touching git — so
// it can run on every start and does not need a "have I done this" flag anywhere. That
// matters because a sandbox outlives one command: `boks run`, `boks exec` and `boks start`
// all reach a running task by the same path, and only one of them is the first.
//
// Three details are load-bearing:
//
//   - `--no-hardlinks`. A local `git clone` hardlinks object files when it can, so the
//     clone's objects would be *the same inodes* as the host repository's — measured, same
//     inode number, link count 2. The guest is root and can chmod, so a hostile guest could
//     then overwrite the host repository's object store through a mode that promises it
//     cannot write to the host at all. Cross-filesystem links fail with EXDEV and git falls
//     back to copying, which is why this has never bitten; a security property must not rest
//     on the two paths happening to be on different filesystems.
//   - `safe.directory`. The source is owned by the host user, and the guest process is
//     usually a different uid, so git's dubious-ownership check refuses it. The exemption
//     names the one directory Boks put there, not `*`.
//   - the uncommitted-work note. A clone carries committed history; whatever is dirty or
//     staged in the host tree is not in it. That surprise is worth a line, and the check runs
//     here because this is where a git that can read the source already exists — Boks itself
//     requires no git on the host.
const cloneScript = `set -e
boks_dst=$0
boks_src=` + workspace.SourcePath + `
if [ -e "$boks_dst/.git" ]; then exit 0; fi
if ! command -v git >/dev/null 2>&1; then
	echo "boks: clone mode needs 'git' inside the guest, and this image has none." >&2
	exit 127
fi
if [ ! -d "$boks_src/.git" ]; then
	echo "boks: $boks_src does not hold a git repository; the host share is missing or empty." >&2
	exit 1
fi
mkdir -p "$boks_dst"
echo "boks: cloning the host repository into $boks_dst (nothing written here reaches the host)" >&2
git -c safe.directory="$boks_src" -c safe.directory="$boks_src/.git" \
	clone --no-hardlinks -- "$boks_src" "$boks_dst" >&2
boks_dirty=$(git --no-optional-locks -c safe.directory="$boks_src" -C "$boks_src" \
	status --porcelain 2>/dev/null | wc -l)
if [ "${boks_dirty:-0}" -gt 0 ]; then
	echo "boks: the host tree has $boks_dirty uncommitted change(s), and a clone carries committed" >&2
	echo "      history only, so they are not in this sandbox. Commit them, or copy them in with" >&2
	echo "      'boks cp <file> <sandbox>:$boks_dst/<file>'." >&2
fi
`

// cloneCommand is the argv that performs the clone on its own, for a sandbox whose container
// process is the keeper. The destination travels as $0 rather than being interpolated into
// the script, so a host path containing a quote is a path and not a shell injection.
func cloneCommand(dst string) []string {
	return []string{"/bin/sh", "-c", cloneScript, dst}
}

// cloneBootstrap wraps a command so that it runs after the clone exists.
//
// It exists for the ephemeral sandbox, where the user's command *is* the container process
// and there is no earlier moment to exec into. `exec "$@"` replaces the shell, so the
// command keeps the process, its signals and its exit status; the wrapper is gone by the
// time anything the user asked for runs.
func cloneBootstrap(dst string, argv []string) []string {
	return append([]string{"/bin/sh", "-c", cloneScript + "\nexec \"$@\"\n", dst}, argv...)
}

// ensureClone makes sure a clone-mode sandbox holds its clone, and reports what the guest
// said while making it.
//
// It runs against the task the caller has just made sure is running, and drives the guest
// through execProcess rather than through Exec: Exec starts the sandbox first, which is what
// called this, and the two would call each other forever.
//
// The guest's output is forwarded rather than swallowed because the first run is where the
// user learns two things they cannot learn anywhere else: that a clone was made, and what
// their dirty working tree did not bring with it. Later starts print nothing, because the
// script does nothing.
func ensureClone(ctx context.Context, container client.Container, task client.Task, out io.Writer) error {
	labels, err := container.Labels(ctx)
	if err != nil {
		return fmt.Errorf("reading sandbox %q: %w", container.ID(), err)
	}
	fs := decodeFilesystem(labels)
	if !fs.IsClone() {
		return nil
	}

	var stderr bytes.Buffer
	code, err := execProcess(ctx, container, task, ExecConfig{
		Name:    container.ID(),
		Command: cloneCommand(fs.Clone),
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	if err != nil {
		return fmt.Errorf("preparing the clone in sandbox %q: %w", container.ID(), err)
	}
	if code != 0 {
		return fmt.Errorf("preparing the clone in sandbox %q failed (exit %d):\n%s",
			container.ID(), code, strings.TrimSpace(stderr.String()))
	}
	if out != nil && stderr.Len() > 0 {
		fmt.Fprint(out, stderr.String())
	}
	return nil
}
