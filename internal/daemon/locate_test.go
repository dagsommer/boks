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

// A package install puts boks in /usr/bin and the runtime in /usr/libexec/boks. /usr/bin
// holds the distribution's own containerd on any real machine, so if the executable's own
// directory is searched first the system binary wins and vendoring has bought nothing.
//
// Measured from an installed .deb on 2026-08-15, before the fix: `boks daemon start` reported
// containerd v2.2.6 out of /usr/bin — below the 2.3 floor, the version that fails at task
// start with `unsupported protocol: Yunix` — while the 2.3.3 the package had just installed
// sat unused in /usr/libexec/boks.
func TestBundleIsSearchedBeforeTheExecutablesOwnDirectory(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "usr", "bin")
	libexec := filepath.Join(root, "usr", "libexec", "boks")
	for _, d := range []string{bin, libexec} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The layout a package produces: a containerd in both places.
	for _, d := range []string{bin, libexec} {
		if err := os.WriteFile(filepath.Join(d, "containerd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dirs := runtimeDirsFrom(filepath.Join(bin, "boks"))
	if len(dirs) < 2 {
		t.Fatalf("expected both directories, got %v", dirs)
	}
	if dirs[0] != libexec {
		t.Errorf("searched %q first; the bundle %q must come first or a package install "+
			"runs the distribution's containerd instead of its own", dirs[0], libexec)
	}
}

// The packaged layout, driven through the production entry points rather than the helper.
//
// The point is the wiring: TestBundleIsSearchedBeforeTheExecutablesOwnDirectory asserts the
// order runtimeDirsFrom returns, but nothing asserted that RuntimeDirs consults it at all.
// Deleting that call left every test in this package passing.
func TestPackagedLayoutResolvesThroughTheProductionPath(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "usr", "bin")
	libexec := filepath.Join(root, "usr", "libexec", "boks")
	for _, d := range []string{bin, libexec} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A containerd in both places, as a package install produces: the distribution's in
	// /usr/bin and the vendored one beside the rest of the runtime.
	//
	// Named the way the platform names it. This test does not run the file — it asserts which
	// PATH entry wins — but FindContainerd looks for containerd.exe on Windows, so a fixture
	// called plainly "containerd" made the lookup miss and the test fail there for a reason
	// that had nothing to do with ordering.
	binary := ContainerdBinaryName()
	for _, d := range []string{bin, libexec} {
		if err := os.WriteFile(filepath.Join(d, binary), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(runtimeDirEnv, "")
	t.Setenv(binaryEnv, "")
	osExecutable = func() (string, error) { return filepath.Join(bin, "boks"), nil }
	t.Cleanup(func() { osExecutable = os.Executable })

	dirs := RuntimeDirs()
	if len(dirs) == 0 || dirs[0] != libexec {
		t.Errorf("RuntimeDirs() = %v; the bundle %q must come first", dirs, libexec)
	}

	got, err := FindContainerd()
	if err != nil {
		t.Fatalf("FindContainerd(): %v", err)
	}
	if want := filepath.Join(libexec, binary); got != want {
		t.Errorf("FindContainerd() = %q, want %q — a packaged install would run the "+
			"distribution's containerd instead of the one it shipped", got, want)
	}

	// And the PATH handed to containerd must lead with the bundle, since that is what the
	// shim inherits when it looks for libkrun and the guest.
	if first := filepath.SplitList(ContainerdPath("/usr/bin"))[0]; first != libexec {
		t.Errorf("ContainerdPath leads with %q, want %q", first, libexec)
	}
}

// Homebrew's e2fsprogs is keg-only, so `brew install e2fsprogs` — the thing every document
// tells a macOS user to do — puts mkfs.ext4 in the keg's sbin and links it nowhere. Off Linux
// containerd formats a writable layer per sandbox with that binary, so a Mac that did the
// documented thing still could not start one. The daemon's PATH is where that is fixed.
//
// The prefixes and the platform are arguments rather than constants because neither is
// reachable from a Linux test runner: /opt/homebrew does not exist here, and a test that only
// called kegDirs would pass just as happily against a function that returned nothing.
func TestKegDirsFindsHomebrewsKegOnlyE2fsprogs(t *testing.T) {
	prefix := t.TempDir()
	sbin := filepath.Join(prefix, "e2fsprogs", "sbin")
	fakeExecutable(t, sbin, "mkfs.ext4")

	got := kegDirsFor("darwin", []string{prefix})
	if len(got) != 1 || got[0] != sbin {
		t.Errorf("kegDirsFor(darwin) = %v, want [%q]; containerd on macOS would never find "+
			"mkfs.ext4 and no sandbox could start", got, sbin)
	}
}

// Only macOS, and only where the keg is. Prepending a directory that does not exist, or one
// that means nothing on the platform, would be noise in a PATH the shim also reads.
func TestKegDirsIsMacOSOnlyAndOnlyWhatExists(t *testing.T) {
	prefix := t.TempDir()
	fakeExecutable(t, filepath.Join(prefix, "e2fsprogs", "sbin"), "mkfs.ext4")

	for _, goos := range []string{"linux", "windows"} {
		if got := kegDirsFor(goos, []string{prefix}); len(got) != 0 {
			t.Errorf("kegDirsFor(%s) = %v, want none: Homebrew's keg-only rule is not what "+
				"stops a binary being found there", goos, got)
		}
	}
	if got := kegDirsFor("darwin", []string{filepath.Join(prefix, "absent")}); len(got) != 0 {
		t.Errorf("kegDirsFor(darwin) = %v for a prefix that does not exist, want none", got)
	}
}

// The keg goes at the END. A user who installed a mkfs.ext4 of their own and put it on PATH
// deliberately must keep it, and Boks' own bundle must still win over both.
func TestKegDirsComeAfterTheInheritedPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kegDirs is macOS-only, and daemonPath cannot be asked for another platform")
	}
	inherited := t.TempDir()
	path := filepath.SplitList(daemonPath(inherited))
	for i, dir := range path {
		if strings.Contains(dir, "e2fsprogs") && i < len(path)-1 && path[i+1] == inherited {
			t.Errorf("keg directory %q precedes the inherited PATH entry %q", dir, inherited)
		}
	}
}
