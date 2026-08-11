package sandbox_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/runtimecfg"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

// These tests drive a real containerd. They are skipped unless BOKS_INTEGRATION=1, since
// they need a running daemon, a pullable image, and usually elevated privileges.
//
// By default they exercise the isolating runtime, so a pass means a command really ran
// behind a VM boundary. BOKS_TEST_RUNTIME and BOKS_TEST_SNAPSHOTTER can point them at
// another runtime to exercise the orchestration path on hosts without a hypervisor; such a
// run proves the containerd plumbing only, not isolation.
//
//	BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v
func testConfig(t *testing.T) sandbox.Config {
	t.Helper()
	if os.Getenv("BOKS_INTEGRATION") != "1" {
		t.Skip("set BOKS_INTEGRATION=1 to run integration tests against a real containerd")
	}

	runtimeID := envOr("BOKS_TEST_RUNTIME", runtimecfg.Runtime)
	if runtimeID != runtimecfg.Runtime {
		t.Logf("WARNING: runtime %q is not the isolating runtime; this run does not test the VM boundary", runtimeID)
	}

	return sandbox.Config{
		Image:       envOr("BOKS_TEST_IMAGE", "docker.io/library/alpine:latest"),
		Runtime:     runtimeID,
		Snapshotter: envOr("BOKS_TEST_SNAPSHOTTER", runtimecfg.Snapshotter),
		Address:     runtimecfg.DefaultAddress(),
		CPUs:        2,
		MemoryMiB:   1024,
	}
}

// testName gives each test its own sandbox name, so a failure in one cannot be inherited by
// another through a re-attached sandbox.
func testName(t *testing.T) string {
	t.Helper()
	return "boks-test-" + strings.ToLower(strings.NewReplacer("/", "-", " ", "-", "_", "-").Replace(t.Name()))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// run executes a command in a fresh sandbox and returns its exit code and stdout.
//
// These tests are about what a single command observes, so they use an ephemeral sandbox:
// the command is the container process and nothing survives it. The persistent lifecycle is
// covered in lifecycle_integration_test.go.
func run(t *testing.T, ws workspace.Workspace, command ...string) (int, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	cfg := testConfig(t)
	cfg.Name = testName(t)
	cfg.Ephemeral = true
	cfg.Command = command
	cfg.Stdout = &stdout
	cfg.Stderr = &stderr
	if ws.HostPath != "" {
		cfg.Workspaces = []workspace.Workspace{ws}
	}

	code, err := sandbox.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("sandbox.Run: %v\nstderr: %s", err, stderr.String())
	}
	return code, stdout.String()
}

func tempWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.Parse(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}
	return ws
}

func TestIntegrationExitCodeZero(t *testing.T) {
	code, out := run(t, tempWorkspace(t), "sh", "-c", "echo hello")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("stdout = %q, want %q", out, "hello")
	}
}

// A non-zero guest exit must reach the caller unchanged, or scripts wrapping boks break.
func TestIntegrationExitCodePropagates(t *testing.T) {
	code, _ := run(t, tempWorkspace(t), "sh", "-c", "exit 42")
	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
}

// The exact-path property: the workspace must appear inside the guest at its host path.
func TestIntegrationWorkspaceExactPath(t *testing.T) {
	ws := tempWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws.HostPath, "marker.txt"), []byte("from-host"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	code, out := run(t, ws, "sh", "-c", "pwd && cat marker.txt")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) < 2 {
		t.Fatalf("stdout = %q, want a path and the file contents", out)
	}
	if lines[0] != ws.HostPath {
		t.Errorf("guest pwd = %q, want the host path %q", lines[0], ws.HostPath)
	}
	if lines[1] != "from-host" {
		t.Errorf("marker contents = %q, want %q", lines[1], "from-host")
	}
}

// Writes must land on the host, which is what makes a shared workspace useful.
func TestIntegrationWorkspaceWriteBack(t *testing.T) {
	ws := tempWorkspace(t)

	if code, _ := run(t, ws, "sh", "-c", "echo from-guest > out.txt"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	got, err := os.ReadFile(filepath.Join(ws.HostPath, "out.txt"))
	if err != nil {
		t.Fatalf("guest write did not reach the host: %v", err)
	}
	if strings.TrimSpace(string(got)) != "from-guest" {
		t.Errorf("host file = %q, want %q", got, "from-guest")
	}
}

// Sharing a workspace must not expose anything above it. The parent should exist as an
// empty mount point containing only the workspace itself.
func TestIntegrationParentDirectoriesNotExposed(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "secret.txt"), []byte("do-not-share"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	inner := filepath.Join(base, "project")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	ws, err := workspace.Parse(inner)
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}

	_, out := run(t, ws, "sh", "-c", "ls "+filepath.Dir(ws.GuestPath))
	if strings.Contains(out, "secret.txt") {
		t.Errorf("parent directory contents leaked into the guest: %q", out)
	}
	if !strings.Contains(out, "project") {
		t.Errorf("parent listing = %q, want it to contain the mount point", out)
	}
}

// The host container runtime socket must never be reachable from inside a sandbox.
func TestIntegrationNoHostDockerSocket(t *testing.T) {
	_, out := run(t, tempWorkspace(t), "sh", "-c",
		"test -S /var/run/docker.sock && echo PRESENT || echo absent")
	if strings.Contains(out, "PRESENT") {
		t.Error("the host Docker socket is visible inside the sandbox")
	}
}

// A read-only workspace must reject writes.
func TestIntegrationReadOnlyWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.Parse(dir + ":ro")
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}

	_, out := run(t, ws, "sh", "-c", "touch nope 2>&1 || echo REFUSED")
	if !strings.Contains(out, "REFUSED") && !strings.Contains(out, "Read-only") {
		t.Errorf("write to a read-only workspace was not refused: %q", out)
	}
}
