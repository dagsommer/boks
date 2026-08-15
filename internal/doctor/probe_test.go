package doctor

import (
	"os/exec"
	"testing"
)

// buildProbe compiles a real Go binary that links containerd, for the checks that read a
// binary's build information rather than run it.
//
// It has to be a compiled binary rather than this test binary: the Go toolchain omits the
// `dep` lines from a test binary's build information, so buildinfo finds no containerd in one.
// cmd/boks is used because it is already in this module and already links containerd, so
// nothing has to be resolved.
func buildProbe(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build a probe binary with")
	}
	build := exec.Command("go", "build", "-o", path, "github.com/dagsommer/boks/cmd/boks")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build a probe binary: %v\n%s", err, out)
	}
}
