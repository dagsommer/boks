package cli

import (
	"strings"
	"testing"
)

// `boks create` has to register the agent a kind: sandbox kit defines, as `run` does. A kit
// that names a built-in agent is the refusal that proves it got that far, before the daemon.
func TestCreateRegistersTheKitsAgent(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	dir := writeKit(t, strings.Replace(netKit, "name: netkit", "name: shell", 1))
	_, stderr, err := runCLI(t, "", "create", "--containerd-address", "/nonexistent/boks-kit.sock",
		"--runtime", "io.containerd.runc.v2", "--i-know-this-is-not-isolated", "--kit", dir, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "already a built-in") {
		t.Errorf("err = %v\n%s", err, stderr)
	}
}
