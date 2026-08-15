package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeExecutable writes a file the search will accept as runnable.
func fakeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRuntimeDirsHonoursTheOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(runtimeDirEnv, dir)
	dirs := RuntimeDirs()
	if len(dirs) == 0 || dirs[0] != dir {
		t.Fatalf("RuntimeDirs() = %v, want %s first", dirs, dir)
	}
}

// A directory that does not exist must not be returned, or every path built from it would be
// a stat that cannot succeed and the fallthrough to PATH would look like a search that ran.
func TestRuntimeDirsDropsWhatIsNotThere(t *testing.T) {
	t.Setenv(runtimeDirEnv, filepath.Join(t.TempDir(), "absent"))
	for _, dir := range RuntimeDirs() {
		if strings.HasSuffix(dir, "absent") {
			t.Errorf("RuntimeDirs() returned %s, which does not exist", dir)
		}
	}
}

// The bundle wins over PATH, and that ordering is the entire point of bundling: the containerd
// Boks ships is pinned against the shim Boks ships.
func TestFindContainerdPrefersTheBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake executables here are shell scripts")
	}
	bundle := t.TempDir()
	onPath := t.TempDir()
	want := fakeExecutable(t, bundle, "containerd")
	fakeExecutable(t, onPath, "containerd")

	t.Setenv(runtimeDirEnv, bundle)
	t.Setenv("PATH", onPath)
	t.Setenv(binaryEnv, "")

	got, err := FindContainerd()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("FindContainerd() = %s, want the bundled %s", got, want)
	}
}

func TestFindContainerdHonoursThePin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake executables here are shell scripts")
	}
	bundle := t.TempDir()
	fakeExecutable(t, bundle, "containerd")
	pinned := fakeExecutable(t, t.TempDir(), "my-containerd")

	t.Setenv(runtimeDirEnv, bundle)
	t.Setenv(binaryEnv, pinned)

	got, err := FindContainerd()
	if err != nil {
		t.Fatal(err)
	}
	if got != pinned {
		t.Errorf("FindContainerd() = %s, want the pinned %s", got, pinned)
	}
}

// The message a fresh host gets has to name a version and a place to get one, because "not
// found" is true and useless: no distribution packages a containerd new enough.
func TestFindContainerdExplainsItself(t *testing.T) {
	t.Setenv(runtimeDirEnv, filepath.Join(t.TempDir(), "absent"))
	t.Setenv(binaryEnv, "")
	t.Setenv("PATH", t.TempDir())

	_, err := FindContainerd()
	if err == nil {
		t.Fatal("FindContainerd() found a containerd on an empty PATH")
	}
	described := describeMissingContainerd(err)
	for _, want := range []string{minimumContainerd, "containerd/releases", binaryEnv} {
		if !strings.Contains(described.Error(), want) {
			t.Errorf("the message never mentions %q:\n%s", want, described)
		}
	}
}

func TestFindShimSearchesTheBundleThenPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake executables here are shell scripts")
	}
	const handler = "io.containerd.nerdbox.v1"
	const binary = "containerd-shim-nerdbox-v1"

	onPath := t.TempDir()
	wantFromPath := fakeExecutable(t, onPath, binary)
	t.Setenv(runtimeDirEnv, filepath.Join(t.TempDir(), "absent"))
	if got := FindShim(handler, onPath); got != wantFromPath {
		t.Errorf("FindShim from PATH = %q, want %q", got, wantFromPath)
	}

	bundle := t.TempDir()
	wantFromBundle := fakeExecutable(t, bundle, binary)
	t.Setenv(runtimeDirEnv, bundle)
	if got := FindShim(handler, onPath); got != wantFromBundle {
		t.Errorf("FindShim = %q, want the bundled %q", got, wantFromBundle)
	}

	// No shim anywhere is a question for `boks doctor`'s vm runtime line, not an error here.
	t.Setenv(runtimeDirEnv, filepath.Join(t.TempDir(), "absent"))
	if got := FindShim(handler, t.TempDir()); got != "" {
		t.Errorf("FindShim with no shim installed = %q, want \"\"", got)
	}
}

// containerd needs mkfs.erofs as well as the shim, so the bundle is prepended rather than
// substituted: replacing PATH would break every host that has erofs-utils installed normally,
// which today is all of them.
func TestDaemonPathPrependsAndKeepsTheRest(t *testing.T) {
	bundle := t.TempDir()
	t.Setenv(runtimeDirEnv, bundle)

	inherited := strings.Join([]string{"/usr/bin", "/bin"}, string(filepath.ListSeparator))
	got := filepath.SplitList(daemonPath(inherited))

	if len(got) == 0 || got[0] != bundle {
		t.Fatalf("daemonPath() = %v, want %s first", got, bundle)
	}
	for _, want := range []string{"/usr/bin", "/bin"} {
		if !containsString(got, want) {
			t.Errorf("daemonPath() dropped %s: %v", want, got)
		}
	}
}

// A directory appearing twice would make containerd stat it twice for every lookup, and would
// grow without bound across nested invocations.
func TestDaemonPathDoesNotRepeatItself(t *testing.T) {
	bundle := t.TempDir()
	t.Setenv(runtimeDirEnv, bundle)

	got := filepath.SplitList(daemonPath(daemonPath("/usr/bin")))
	seen := map[string]int{}
	for _, dir := range got {
		seen[dir]++
		if seen[dir] > 1 {
			t.Fatalf("daemonPath() repeats %s: %v", dir, got)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
