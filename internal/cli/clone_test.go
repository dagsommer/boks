package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

// hostRepo makes a directory InspectRepo will accept, and returns it as a workspace.
func hostRepo(t *testing.T) workspace.Workspace {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	ws, err := workspace.Parse(dir)
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}
	return ws
}

// applyClone drives the decision and hands back what the user would have seen.
func applyClone(t *testing.T, inv invocation) (sandbox.Config, string, error) {
	t.Helper()
	var stderr bytes.Buffer
	var cfg sandbox.Config
	err := applyCloneMode(&sandboxFlags{clone: true}, inv, &cfg,
		Env{Stdout: io.Discard, Stderr: &stderr})
	return cfg, stderr.String(), err
}

func TestCloneModeAcceptsARepository(t *testing.T) {
	cfg, out, err := applyClone(t, invocation{name: "s", workspaces: []workspace.Workspace{hostRepo(t)}})
	if err != nil {
		t.Fatalf("applyCloneMode: %v", err)
	}
	if !cfg.Clone {
		t.Error("Config.Clone = false, want clone mode selected")
	}
	// A user who has just chosen a mode whose commits live inside the VM needs to be
	// told how they come out, at the moment they choose it.
	if !strings.Contains(out, "boks bundle s") {
		t.Errorf("stderr = %q, want it to point at 'boks bundle'", out)
	}
}

// The one unacceptable answer to a workspace that is not a repository is to quietly do
// something else — direct mode above all, which would grant the guest write access to the
// user's files after they asked for the opposite.
func TestCloneModeRefusesANonRepository(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.Parse(dir)
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}

	cfg, _, err := applyClone(t, invocation{name: "s", workspaces: []workspace.Workspace{ws}})
	if err == nil {
		t.Fatal("applyCloneMode accepted a directory that is not a git repository")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %q, want it to say the directory is not a repository", err)
	}
	if cfg.Clone {
		t.Error("Config.Clone = true after a refusal")
	}
}

// One writable share anywhere undoes the property, so a second read-write workspace is
// refused rather than silently downgraded.
func TestCloneModeRefusesASecondWritableWorkspace(t *testing.T) {
	extra, err := workspace.Parse(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}

	_, _, err = applyClone(t, invocation{
		name:       "s",
		workspaces: []workspace.Workspace{hostRepo(t), extra},
	})
	if err == nil {
		t.Fatal("applyCloneMode accepted a read-write second workspace")
	}
	if !strings.Contains(err.Error(), ":ro") {
		t.Errorf("error = %q, want it to name the way out", err)
	}
}

func TestCloneModeAllowsAReadOnlySecondWorkspace(t *testing.T) {
	extra, err := workspace.Parse(t.TempDir() + ":ro")
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}

	cfg, _, err := applyClone(t, invocation{
		name:       "s",
		workspaces: []workspace.Workspace{hostRepo(t), extra},
	})
	if err != nil {
		t.Fatalf("applyCloneMode: %v", err)
	}
	if !cfg.Clone {
		t.Error("Config.Clone = false, want clone mode with a read-only extra workspace")
	}
}

// The mode lives in the OCI mounts, which are written when a container is created and never
// revisited, so --clone on a re-attach cannot be applied. It must say so — loudly for the
// case that matters, which is a sandbox that is about to write to the user's disk.
func TestCloneOnAnExistingDirectSandboxIsARefusedNoOp(t *testing.T) {
	inv := invocation{
		name:   "web",
		exists: true,
		info: sandbox.Info{
			Name:       "web",
			Filesystem: sandbox.Filesystem{Version: 1, Mode: sandbox.FilesystemDirect},
			Workspaces: []sandbox.WorkspaceRef{{HostPath: "/home/alice/src/foo"}},
		},
	}

	cfg, out, err := applyClone(t, inv)
	if err != nil {
		t.Fatalf("applyCloneMode: %v", err)
	}
	if cfg.Clone {
		t.Error("Config.Clone = true for a sandbox that already exists")
	}
	for _, want := range []string{"--clone is ignored", "DIRECT", "/home/alice/src/foo", "boks rm web"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning %q does not mention %q", out, want)
		}
	}
}

// Re-attaching to a sandbox that is already in clone mode is a no-op too, but not a warning:
// nothing is about to happen that the user did not ask for.
func TestCloneOnAnExistingCloneSandboxSaysItIsANoOp(t *testing.T) {
	inv := invocation{
		name:   "web",
		exists: true,
		info: sandbox.Info{
			Name:       "web",
			Filesystem: sandbox.Filesystem{Version: 1, Mode: sandbox.FilesystemClone},
		},
	}

	cfg, out, err := applyClone(t, inv)
	if err != nil {
		t.Fatalf("applyCloneMode: %v", err)
	}
	if cfg.Clone {
		t.Error("Config.Clone = true for a sandbox that already exists")
	}
	if !strings.Contains(out, "already in clone mode") {
		t.Errorf("note = %q, want it to say the sandbox is already in clone mode", out)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("note = %q, want no warning for a sandbox that is already what was asked for", out)
	}
}

// Without the flag, nothing about a run changes — including no message.
func TestWithoutTheFlagNothingHappens(t *testing.T) {
	var stderr bytes.Buffer
	var cfg sandbox.Config
	err := applyCloneMode(&sandboxFlags{}, invocation{name: "s"}, &cfg,
		Env{Stdout: io.Discard, Stderr: &stderr})
	if err != nil {
		t.Fatalf("applyCloneMode: %v", err)
	}
	if cfg.Clone {
		t.Error("Config.Clone = true without --clone")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// The warnings for a repository whose content a clone will not carry have to reach the user
// before the sandbox is created, and must not stop it.
func TestCloneWarnsAboutSubmodulesAndLFS(t *testing.T) {
	ws := hostRepo(t)
	for _, f := range []struct{ name, body string }{
		{".gitmodules", "[submodule \"x\"]\n"},
		{".gitattributes", "*.bin filter=lfs -text\n"},
	} {
		if err := os.WriteFile(filepath.Join(ws.HostPath, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	cfg, out, err := applyClone(t, invocation{name: "s", workspaces: []workspace.Workspace{ws}})
	if err != nil {
		t.Fatalf("applyCloneMode: %v", err)
	}
	if !cfg.Clone {
		t.Error("Config.Clone = false; submodules and LFS are warnings, not refusals")
	}
	for _, want := range []string{"submodules", "Git LFS"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to mention %q", out, want)
		}
	}
}

// `run` and `create` build the same sandbox, so both have to accept the flag; a --clone that
// only worked on one of them would be a trap.
func TestBothRunAndCreateAcceptClone(t *testing.T) {
	env := Env{Stdout: io.Discard, Stderr: io.Discard}
	commands := map[string]*cobra.Command{
		"run":    newRunCommand(env, &devFlags{}),
		"create": newCreateCommand(env, &devFlags{}),
	}
	for name, cmd := range commands {
		if cmd.Flags().Lookup("clone") == nil {
			t.Errorf("%s has no --clone flag", name)
		}
	}
}
