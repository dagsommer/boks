package sandbox_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

// These tests drive the persistent lifecycle against a real containerd. Like the rest of
// the integration suite they need BOKS_INTEGRATION=1, and they default to the isolating
// runtime; see integration_test.go.

// newSandbox creates a sandbox for one test and removes it afterwards, so a failure in the
// middle of a lifecycle cannot leave a container, task or snapshot behind.
func newSandbox(t *testing.T, ws workspace.Workspace, command ...string) sandbox.Config {
	t.Helper()

	cfg := testConfig(t)
	cfg.Name = testName(t)
	cfg.Command = command
	cfg.Workspaces = []workspace.Workspace{ws}
	cfg.Stdout = os.Stdout
	cfg.Stderr = os.Stderr

	t.Cleanup(func() {
		if err := sandbox.Remove(context.Background(), cfg.Address, cfg.Name, true); err != nil &&
			!strings.Contains(err.Error(), "no sandbox named") {
			t.Errorf("cleanup: %v", err)
		}
	})
	return cfg
}

// execIn runs a command in a sandbox and returns its exit code and stdout.
func execIn(t *testing.T, cfg sandbox.Config, command ...string) (int, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code, err := sandbox.Exec(context.Background(), sandbox.ExecConfig{
		Address: cfg.Address,
		Name:    cfg.Name,
		Command: command,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		return code, stdout.String(), err
	}
	if stderr.Len() > 0 {
		t.Logf("guest stderr: %s", stderr.String())
	}
	return code, stdout.String(), nil
}

func find(t *testing.T, address, name string) (sandbox.Info, bool) {
	t.Helper()
	infos, err := sandbox.List(context.Background(), address)
	if err != nil {
		t.Fatalf("sandbox.List: %v", err)
	}
	for _, info := range infos {
		if info.Name == name {
			return info, true
		}
	}
	return sandbox.Info{}, false
}

// The whole point of a persistent sandbox: create, use, stop, start, and the filesystem is
// still there. This is the create → ls → exec → stop → start → exec → rm path.
func TestIntegrationLifecycle(t *testing.T) {
	ws := tempWorkspace(t)
	cfg := newSandbox(t, ws)
	ctx := context.Background()

	if _, err := sandbox.Create(ctx, cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	info, ok := find(t, cfg.Address, cfg.Name)
	if !ok {
		t.Fatalf("sandbox %q is not listed after Create", cfg.Name)
	}
	if info.Status != sandbox.StatusStopped {
		t.Errorf("status after Create = %q, want %q", info.Status, sandbox.StatusStopped)
	}
	if info.Workspace() != ws.HostPath {
		t.Errorf("workspace = %q, want %q", info.Workspace(), ws.HostPath)
	}
	if info.Created.IsZero() {
		t.Error("creation time is zero")
	}

	if err := sandbox.Start(ctx, cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info, _ := find(t, cfg.Address, cfg.Name); info.Status != sandbox.StatusRunning {
		t.Fatalf("status after Start = %q, want %q", info.Status, sandbox.StatusRunning)
	}

	// Write outside the workspace, so what survives can only have come from the
	// sandbox's own snapshot.
	if code, _, err := execIn(t, cfg, "sh", "-c", "echo persisted > /root/state.txt"); err != nil || code != 0 {
		t.Fatalf("exec write: code=%d err=%v", code, err)
	}

	if err := sandbox.Stop(ctx, cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if info, _ := find(t, cfg.Address, cfg.Name); info.Status != sandbox.StatusStopped {
		t.Errorf("status after Stop = %q, want %q", info.Status, sandbox.StatusStopped)
	}

	if err := sandbox.Start(ctx, cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	code, out, err := execIn(t, cfg, "cat", "/root/state.txt")
	if err != nil || code != 0 {
		t.Fatalf("exec read: code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out) != "persisted" {
		t.Errorf("file written before stop = %q, want %q", out, "persisted")
	}

	if err := sandbox.Remove(ctx, cfg.Address, cfg.Name, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := find(t, cfg.Address, cfg.Name); ok {
		t.Error("sandbox is still listed after Remove")
	}
}

// Re-attach is what makes a sandbox worth keeping: a second run against the same workspace
// must land in the same sandbox, not a new one.
func TestIntegrationRunReattaches(t *testing.T) {
	ws := tempWorkspace(t)
	cfg := newSandbox(t, ws)

	var first bytes.Buffer
	firstCfg := cfg
	firstCfg.Command = []string{"sh", "-c", "echo from-first > /root/marker; echo done"}
	firstCfg.Stdout = &first
	if code, err := sandbox.Run(context.Background(), firstCfg); err != nil || code != 0 {
		t.Fatalf("first Run: code=%d err=%v", code, err)
	}

	var second bytes.Buffer
	secondCfg := cfg
	secondCfg.Command = []string{"cat", "/root/marker"}
	secondCfg.Stdout = &second
	if code, err := sandbox.Run(context.Background(), secondCfg); err != nil || code != 0 {
		t.Fatalf("second Run: code=%d err=%v", code, err)
	}
	if strings.TrimSpace(second.String()) != "from-first" {
		t.Errorf("second run read %q, want %q — it did not re-attach", second.String(), "from-first")
	}

	infos, err := sandbox.List(context.Background(), cfg.Address)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	matches := 0
	for _, info := range infos {
		if info.Name == cfg.Name {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("found %d sandboxes named %q, want exactly 1", matches, cfg.Name)
	}
}

// A persistent sandbox must outlive the command that created it, or "persistent" means
// nothing.
func TestIntegrationRunLeavesSandboxRunning(t *testing.T) {
	cfg := newSandbox(t, tempWorkspace(t))
	cfg.Command = []string{"true"}
	cfg.Stdout = &bytes.Buffer{}

	if code, err := sandbox.Run(context.Background(), cfg); err != nil || code != 0 {
		t.Fatalf("Run: code=%d err=%v", code, err)
	}
	info, ok := find(t, cfg.Address, cfg.Name)
	if !ok {
		t.Fatal("sandbox is gone after run")
	}
	if info.Status != sandbox.StatusRunning {
		t.Errorf("status = %q, want %q", info.Status, sandbox.StatusRunning)
	}
}

// The exec exit code is what scripts wrapping boks depend on.
func TestIntegrationExecExitCode(t *testing.T) {
	cfg := newSandbox(t, tempWorkspace(t))
	if _, err := sandbox.Create(context.Background(), cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sandbox.Start(context.Background(), cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}

	code, _, err := execIn(t, cfg, "sh", "-c", "exit 42")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
}

// An exec'd command runs in the sandbox's workspace, at its host path, like the sandbox's
// own process does.
func TestIntegrationExecUsesWorkspace(t *testing.T) {
	ws := tempWorkspace(t)
	cfg := newSandbox(t, ws)
	if _, err := sandbox.Create(context.Background(), cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sandbox.Start(context.Background(), cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}

	code, out, err := execIn(t, cfg, "pwd")
	if err != nil || code != 0 {
		t.Fatalf("Exec: code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out) != ws.HostPath {
		t.Errorf("exec pwd = %q, want the workspace host path %q", out, ws.HostPath)
	}
}

// Expected-failure paths. The message is the feature here: it has to say what to do next.
func TestIntegrationExecIntoStoppedSandbox(t *testing.T) {
	cfg := newSandbox(t, tempWorkspace(t))
	if _, err := sandbox.Create(context.Background(), cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, _, err := execIn(t, cfg, "true")
	if err == nil {
		t.Fatal("exec into a stopped sandbox succeeded")
	}
	if !strings.Contains(err.Error(), "boks start") {
		t.Errorf("error = %q, want it to suggest 'boks start'", err)
	}
}

func TestIntegrationOperationsOnMissingSandbox(t *testing.T) {
	cfg := testConfig(t)
	const missing = "boks-test-definitely-not-here"

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"inspect", func() error { _, err := sandbox.Inspect(context.Background(), cfg.Address, missing); return err }},
		{"start", func() error { return sandbox.Start(context.Background(), cfg.Address, missing) }},
		{"stop", func() error { return sandbox.Stop(context.Background(), cfg.Address, missing) }},
		{"rm", func() error { return sandbox.Remove(context.Background(), cfg.Address, missing, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s on a missing sandbox succeeded", tc.name)
			}
			if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "boks ls") {
				t.Errorf("error = %q, want it to name the sandbox and point at 'boks ls'", err)
			}
		})
	}
}

// Removing a running sandbox without force must refuse, and say how to proceed.
func TestIntegrationRemoveRunningRequiresForce(t *testing.T) {
	cfg := newSandbox(t, tempWorkspace(t))
	if _, err := sandbox.Create(context.Background(), cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sandbox.Start(context.Background(), cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err := sandbox.Remove(context.Background(), cfg.Address, cfg.Name, false)
	if err == nil {
		t.Fatal("Remove deleted a running sandbox without force")
	}
	if !strings.Contains(err.Error(), "rm -f") {
		t.Errorf("error = %q, want it to mention 'rm -f'", err)
	}

	if err := sandbox.Remove(context.Background(), cfg.Address, cfg.Name, true); err != nil {
		t.Fatalf("Remove with force: %v", err)
	}
}

// cp in both directions, including a directory tree, which is the part that exercises the
// tar packing rather than a single file write.
func TestIntegrationCopy(t *testing.T) {
	ws := tempWorkspace(t)
	cfg := newSandbox(t, ws)
	ctx := context.Background()

	if _, err := sandbox.Create(ctx, cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sandbox.Start(ctx, cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Start: %v", err)
	}

	host := t.TempDir()
	tree := filepath.Join(host, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "file.txt"), []byte("to-guest"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := sandbox.Copy(ctx, sandbox.CopyConfig{
		Address:   cfg.Address,
		Name:      cfg.Name,
		ToSandbox: true,
		HostPath:  tree,
		GuestPath: "/root/tree",
	}); err != nil {
		t.Fatalf("Copy to sandbox: %v", err)
	}

	code, out, err := execIn(t, cfg, "cat", "/root/tree/sub/file.txt")
	if err != nil || code != 0 {
		t.Fatalf("reading the copied file: code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out) != "to-guest" {
		t.Errorf("copied file = %q, want %q", out, "to-guest")
	}

	if code, _, err := execIn(t, cfg, "sh", "-c", "mkdir -p /root/out && echo to-host > /root/out/back.txt"); err != nil || code != 0 {
		t.Fatalf("preparing the guest file: code=%d err=%v", code, err)
	}
	back := filepath.Join(host, "back")
	if err := sandbox.Copy(ctx, sandbox.CopyConfig{
		Address:   cfg.Address,
		Name:      cfg.Name,
		HostPath:  back,
		GuestPath: "/root/out",
	}); err != nil {
		t.Fatalf("Copy from sandbox: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(back, "back.txt"))
	if err != nil {
		t.Fatalf("the copied directory did not reach the host: %v", err)
	}
	if strings.TrimSpace(string(got)) != "to-host" {
		t.Errorf("copied file = %q, want %q", got, "to-host")
	}
}

// A sandbox that is stopped cannot be copied into, and the error has to say so rather than
// failing somewhere inside tar.
func TestIntegrationCopyIntoStoppedSandbox(t *testing.T) {
	cfg := newSandbox(t, tempWorkspace(t))
	if _, err := sandbox.Create(context.Background(), cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	src := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := sandbox.Copy(context.Background(), sandbox.CopyConfig{
		Address:   cfg.Address,
		Name:      cfg.Name,
		ToSandbox: true,
		HostPath:  src,
		GuestPath: "/root/f.txt",
	})
	if err == nil {
		t.Fatal("copying into a stopped sandbox succeeded")
	}
	if !strings.Contains(err.Error(), "boks start") {
		t.Errorf("error = %q, want it to suggest 'boks start'", err)
	}
}
