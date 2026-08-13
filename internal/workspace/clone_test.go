package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoAt makes a directory that looks like a repository root to InspectRepo, which reads the
// filesystem rather than running git. That is the whole point of it: Boks requires no git on
// the host, so the checks have to be answerable without one.
func repoAt(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir
}

// The source share is the one thing in a clone-mode sandbox that is still the user's disk,
// so it must be read-only, and it must not be at the workspace's host path — that path holds
// the clone.
func TestSourceShareIsReadOnlyAndRelocated(t *testing.T) {
	ws := Workspace{HostPath: "/home/alice/src/foo", GuestPath: "/home/alice/src/foo", Mode: ModeReadWrite}

	src := ws.Source()
	if src.HostPath != ws.HostPath {
		t.Errorf("HostPath = %q, want the workspace's %q", src.HostPath, ws.HostPath)
	}
	if src.GuestPath != SourcePath {
		t.Errorf("GuestPath = %q, want %q", src.GuestPath, SourcePath)
	}
	if !src.ReadOnly() {
		t.Errorf("Mode = %q, want the source share to be read-only", src.Mode)
	}
	if !slices.Contains(src.MountOptions(), "ro") {
		t.Errorf("MountOptions = %v, want it to contain \"ro\"", src.MountOptions())
	}
	if slices.Contains(src.MountOptions(), "rw") {
		t.Errorf("MountOptions = %v, want no \"rw\"", src.MountOptions())
	}
}

// A read-write workspace turned into a source share must not carry its old mode with it.
func TestSourceShareIgnoresTheWorkspaceMode(t *testing.T) {
	for _, mode := range []Mode{ModeReadWrite, ModeReadOnly} {
		if src := (Workspace{HostPath: "/x", Mode: mode}).Source(); !src.ReadOnly() {
			t.Errorf("Source() of a %q workspace is %q, want read-only", mode, src.Mode)
		}
	}
}

func TestInspectRepoAcceptsARepositoryRoot(t *testing.T) {
	dir := repoAt(t, t.TempDir())

	repo, err := InspectRepo(dir)
	if err != nil {
		t.Fatalf("InspectRepo: %v", err)
	}
	if repo.Root != dir {
		t.Errorf("Root = %q, want %q", repo.Root, dir)
	}
	if repo.Submodules || repo.LFS {
		t.Errorf("repo = %+v, want no submodule or LFS finding", repo)
	}
	if notes := repo.Notes(); len(notes) != 0 {
		t.Errorf("Notes() = %v, want none", notes)
	}
}

// The refusals are the load-bearing part of clone mode: --clone is asked for to keep guest
// writes off the user's disk, so anything Boks cannot clone has to be an error rather than a
// quiet fallback. Each case also has to say which mistake was made.
func TestInspectRepoRefusals(t *testing.T) {
	tests := []struct {
		name    string
		build   func(t *testing.T, base string) string
		wantAll []string
	}{
		{
			name: "not a repository",
			build: func(t *testing.T, base string) string {
				return base
			},
			wantAll: []string{"not a git repository", "git init"},
		},
		{
			name: "a subdirectory of a repository",
			build: func(t *testing.T, base string) string {
				repoAt(t, base)
				sub := filepath.Join(base, "pkg", "inner")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				return sub
			},
			// It must name the root and say why it will not simply use it.
			wantAll: []string{"subdirectory of the one at", "will not widen the share"},
		},
		{
			name: "a linked worktree or submodule",
			build: func(t *testing.T, base string) string {
				if err := os.WriteFile(filepath.Join(base, ".git"),
					[]byte("gitdir: /elsewhere/.git/worktrees/w\n"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return base
			},
			wantAll: []string{"linked git worktree", "/elsewhere/.git/worktrees/w", "outside the workspace"},
		},
		{
			name: "a bare repository",
			build: func(t *testing.T, base string) string {
				for _, entry := range []string{"objects", "refs"} {
					if err := os.MkdirAll(filepath.Join(base, entry), 0o755); err != nil {
						t.Fatalf("MkdirAll: %v", err)
					}
				}
				if err := os.WriteFile(filepath.Join(base, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return base
			},
			wantAll: []string{"bare git repository", "no working tree"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.build(t, t.TempDir())
			_, err := InspectRepo(path)
			if err == nil {
				t.Fatalf("InspectRepo(%q) = nil error, want a refusal", path)
			}
			for _, want := range tc.wantAll {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// Submodules and LFS are warned about rather than refused: both are common, both are
// recoverable from inside the sandbox, and a refusal would cost more than the surprise does.
// A warning that never fires is the failure worth testing for.
func TestInspectRepoWarnsAboutMissingContent(t *testing.T) {
	tests := []struct {
		name     string
		build    func(t *testing.T, dir string)
		wantWord string
	}{
		{
			name: "submodules",
			build: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, ".gitmodules"), "[submodule \"x\"]\n")
			},
			wantWord: "submodules",
		},
		{
			name: "LFS by attribute",
			build: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, ".gitattributes"), "*.bin filter=lfs diff=lfs -text\n")
			},
			wantWord: "Git LFS",
		},
		{
			name: "LFS by the objects it already fetched",
			build: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, ".git", "lfs"), 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
			},
			wantWord: "Git LFS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := repoAt(t, t.TempDir())
			tc.build(t, dir)

			repo, err := InspectRepo(dir)
			if err != nil {
				t.Fatalf("InspectRepo: %v", err)
			}
			notes := repo.Notes()
			if len(notes) != 1 {
				t.Fatalf("Notes() = %v, want exactly one", notes)
			}
			if !strings.Contains(notes[0], tc.wantWord) {
				t.Errorf("note %q does not mention %q", notes[0], tc.wantWord)
			}
		})
	}
}

// A `.gitattributes` that mentions no LFS filter must not produce a warning, or the warning
// stops meaning anything.
func TestInspectRepoDoesNotInventAnLFSWarning(t *testing.T) {
	dir := repoAt(t, t.TempDir())
	write(t, filepath.Join(dir, ".gitattributes"), "*.txt text eol=lf\n")

	repo, err := InspectRepo(dir)
	if err != nil {
		t.Fatalf("InspectRepo: %v", err)
	}
	if repo.LFS {
		t.Error("LFS = true for a .gitattributes with no lfs filter")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
