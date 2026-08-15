package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// stagedRuntimeDir puts a runtime directory where daemon.RuntimeDirs will find it, and
// returns it. BOKS_RUNTIME_DIR is the documented override and is what makes this testable
// without building a binary and re-execing it.
func stagedRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BOKS_RUNTIME_DIR", dir)
	// A PATH that deliberately excludes it — the whole point is that a packaged install
	// puts these files somewhere no shell searches.
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty-but-real"))
	return dir
}

// The checks ask what containerd and the shim can find, not what the user's shell can. A
// packaged install puts the runtime in /usr/libexec/boks, which is on neither, and `boks
// daemon` bridges the gap by prepending it to containerd's PATH.
func TestShimGetenvUsesContainerdsPath(t *testing.T) {
	dir := stagedRuntimeDir(t)

	if got := shimGetenv("PATH"); !slices.Contains(filepath.SplitList(got), dir) {
		t.Errorf("the runtime directory is missing from the PATH the checks reason about.\n"+
			"  want %s in\n  got  %s", dir, got)
	}
	if slices.Contains(filepath.SplitList(os.Getenv("PATH")), dir) {
		t.Fatal("the shell PATH already contains the runtime directory, so this test would " +
			"pass without the substitution doing anything")
	}
	// Everything other than PATH must pass through untouched: `boks daemon` does not
	// rewrite LIBKRUN_PATH, so neither may this.
	t.Setenv("LIBKRUN_PATH", "/some/where")
	if got := shimGetenv("LIBKRUN_PATH"); got != "/some/where" {
		t.Errorf("LIBKRUN_PATH = %q, want it passed through unchanged", got)
	}
}

func TestLookPathFindsAPackagedBinary(t *testing.T) {
	dir := stagedRuntimeDir(t)

	name := "containerd-shim-nerdbox-v1"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	staged := filepath.Join(dir, name)
	if err := os.WriteFile(staged, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The control: the standard library, reading the shell's PATH, cannot see it. Without
	// this the test would pass on any implementation.
	if _, err := exec.LookPath(name); err == nil {
		t.Fatal("exec.LookPath found it on the shell PATH, so this test proves nothing")
	}

	got, err := lookPath(name)
	if err != nil {
		t.Fatalf("lookPath(%q) = %v; a correctly installed package would report the shim "+
			"missing", name, err)
	}
	if !strings.HasPrefix(got, dir) {
		t.Errorf("lookPath returned %q, want something under %q", got, dir)
	}
}

// A name with a directory in it is not a PATH lookup, in either implementation.
func TestLookPathPassesThroughExplicitPaths(t *testing.T) {
	stagedRuntimeDir(t)
	if _, err := lookPath(filepath.Join("definitely", "not", "here")); err == nil {
		t.Error("an explicit relative path that does not exist was resolved anyway")
	}
}

func TestLookPathReportsMissingBinaries(t *testing.T) {
	stagedRuntimeDir(t)
	if got, err := lookPath("a-binary-no-host-has"); err == nil {
		t.Errorf("lookPath resolved a nonexistent binary to %q", got)
	}
}
