package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SourcePath is where a clone-mode sandbox sees the host repository.
//
// It is deliberately *not* the workspace's host path. In clone mode the host path inside the
// guest holds the guest's own clone, so the original has to appear somewhere else, and the
// somewhere else is the same location Docker Sandboxes uses. A user who has read that
// product's documentation already knows where to look, and an agent that has been told about
// `/run/sandbox/source` finds it here too.
const SourcePath = "/run/sandbox/source"

// Source returns the read-only share of this workspace at SourcePath.
//
// Read-only is the whole point: this is the one thing in a clone-mode sandbox that is still
// the user's actual disk, so the guest gets a view it cannot write through.
func (w Workspace) Source() Workspace {
	return Workspace{HostPath: w.HostPath, GuestPath: SourcePath, Mode: ModeReadOnly}
}

// Repo is what clone mode needs to know about a host directory: that it is a git repository
// Boks can clone, and which of the things a clone silently leaves behind are present.
type Repo struct {
	// Root is the repository's working tree, which is also the workspace path.
	Root string
	// Submodules reports a `.gitmodules` file. A clone does not populate submodules.
	Submodules bool
	// LFS reports that the repository appears to use Git LFS. A clone copies the
	// pointer files, not the objects they point at.
	LFS bool
}

// InspectRepo checks that a host directory can be the source of a clone-mode sandbox, and
// reports what a clone of it will be missing.
//
// Every refusal here is deliberate, and the reasoning is the same in each case: `--clone` is
// asked for because the user does not want guest writes on their disk, so the one answer
// Boks must never give is to quietly do something else. In particular there is no fallback
// to direct mode — that would turn a request for containment into a live share on the
// strength of a warning nobody reads.
func InspectRepo(hostPath string) (Repo, error) {
	dotGit := filepath.Join(hostPath, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		if !os.IsNotExist(err) {
			return Repo{}, fmt.Errorf("inspecting %s: %w", dotGit, err)
		}
		return Repo{}, notARepository(hostPath)
	}

	if !info.IsDir() {
		// A `.git` file rather than a directory means the real git directory is
		// somewhere else: a linked worktree, or a submodule of a parent repository.
		// That directory is outside the workspace and is not shared, so a clone would
		// fail — and sharing it too would expose more of the host than direct mode
		// does, which is the wrong direction for a containment feature.
		return Repo{}, fmt.Errorf(
			"%s is a linked git worktree or a submodule: its git directory is %s,\n"+
				"which is outside the workspace and is not shared with the sandbox.\n"+
				"Boks refuses rather than share more of your disk than direct mode would.\n"+
				"Run --clone against the main checkout, or run without --clone.",
			hostPath, gitDirTarget(dotGit))
	}

	repo := Repo{Root: hostPath}
	if _, err := os.Stat(filepath.Join(hostPath, ".gitmodules")); err == nil {
		repo.Submodules = true
	}
	repo.LFS = usesLFS(hostPath)
	return repo, nil
}

// notARepository explains the absence, using whatever the directory does contain to say
// which mistake was made.
func notARepository(hostPath string) error {
	if isBareRepo(hostPath) {
		return fmt.Errorf(
			"%s is a bare git repository, which has no working tree for an agent to work in.\n"+
				"Clone it on the host and run Boks against the clone, or run without --clone.",
			hostPath)
	}
	if root := enclosingRepo(hostPath); root != "" {
		// Cloning the enclosing repository would share a directory the user did not
		// name — strictly more of the host than direct mode exposes.
		return fmt.Errorf(
			"%s is not a git repository; it is a subdirectory of the one at %s.\n"+
				"--clone shares and clones the repository root, so run it from %s.\n"+
				"Boks will not widen the share to a directory you did not name.",
			hostPath, root, root)
	}
	return fmt.Errorf(
		"%s is not a git repository, and --clone has nothing to clone.\n"+
			"Run 'git init' there first, or run without --clone — but note that direct mode\n"+
			"lets the guest write straight to these files. See docs/security-model.md.",
		hostPath)
}

// isBareRepo reports whether a directory looks like a bare repository. The three entries are
// what `git init --bare` always produces and what git's own discovery keys on.
func isBareRepo(hostPath string) bool {
	for _, entry := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(hostPath, entry)); err != nil {
			return false
		}
	}
	return true
}

// enclosingRepo returns the nearest ancestor that is a repository root, or "".
func enclosingRepo(hostPath string) string {
	for dir := filepath.Dir(hostPath); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}

// gitDirTarget reads the "gitdir:" pointer out of a `.git` file, so the refusal can name the
// directory rather than describe it. An unreadable file still produces a usable message.
func gitDirTarget(dotGit string) string {
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "elsewhere on this machine"
	}
	target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "gitdir:"))
	if target == "" {
		return "elsewhere on this machine"
	}
	return target
}

// usesLFS reports whether a repository appears to use Git LFS.
//
// Two signals, both cheap and both host-side so that no git binary is needed: a `.git/lfs`
// directory, which only exists once LFS has actually fetched something, and a `filter=lfs`
// attribute in the top-level `.gitattributes`. Attributes in subdirectories are not read —
// this is a warning, not a gate, and a false negative costs a message rather than a
// guarantee.
func usesLFS(hostPath string) bool {
	if info, err := os.Stat(filepath.Join(hostPath, ".git", "lfs")); err == nil && info.IsDir() {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(hostPath, ".gitattributes"))
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), "filter=lfs")
}

// Notes returns the things a clone of this repository will not contain, in the words the
// user needs to hear them. Empty for a repository with none of them.
//
// These are warnings rather than refusals because both shapes are common and both are
// recoverable from inside the sandbox, given network access to wherever the content lives.
// A refusal would make --clone unusable for a large class of real projects in order to
// prevent a surprise a sentence can prevent instead.
func (r Repo) Notes() []string {
	var notes []string
	if r.Submodules {
		notes = append(notes,
			"this repository has submodules, and a clone does not populate them; run\n"+
				"      'git submodule update --init' inside the sandbox, which needs network access\n"+
				"      to the submodule URLs")
	}
	if r.LFS {
		notes = append(notes,
			"this repository appears to use Git LFS, and a clone carries the pointer files\n"+
				"      rather than their contents; run 'git lfs pull' inside the sandbox, which needs\n"+
				"      network access to the LFS endpoint")
	}
	return notes
}
