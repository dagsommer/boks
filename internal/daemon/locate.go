package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dagsommer/boks/internal/runtimecfg"
)

// Where Boks looks for the runtime pieces it runs and hands to containerd.
//
// There are two questions here and they have the same answer, which is why they are in one
// file. The first is which containerd binary `boks daemon` starts. The second is what PATH
// that containerd is given — and the second is not a detail, because containerd resolves the
// runtime shim through *its own* PATH, and the shim then locates libkrun and the guest kernel
// by scanning that same PATH (see internal/doctor/libkrun.go, which transcribes the scan).
// docs/install.md lists "start containerd with the shim on its PATH" as one of the three
// things Homebrew cannot do for the user, and docs/verification.md's macOS notes list it as
// one of the four things that cost time on the first run. A daemon Boks starts itself is the
// one place that can simply be fixed.
//
// The layout below was written before anything produced it. It is produced now:
// .github/workflows/linux-runtime.yml builds the shim, libkrun.so and a containerd into
// `boks-runtime-linux-<arch>`, the .deb and .rpm install those into /usr/libexec/boks, and the
// Windows zip lays them out flat beside boks.exe. On a source build nothing is found here and
// everything falls through to PATH, which is exactly the behaviour a source build wants.

// runtimeDirEnv names a directory to search ahead of everything else. It is the escape hatch
// for a tester who has built the pieces and put them somewhere of their own.
const runtimeDirEnv = "BOKS_RUNTIME_DIR"

// binaryEnv pins the containerd executable outright, skipping the search.
const binaryEnv = "BOKS_CONTAINERD_BINARY"

// RuntimeDirs returns the directories Boks searches for bundled runtime pieces, nearest
// first, keeping only those that exist.
//
// The two derived locations are relative to the boks executable rather than absolute, so that
// a tarball unpacked anywhere works and a Homebrew keg's opt prefix needs no configuration.
// The bundle directory comes FIRST, and the order is load-bearing rather than incidental —
// see runtimeDirsFrom for the measured failure that reversing it produced:
//
//	<exe dir>/../libexec/boks     the FHS location for a program's private executables,
//	                              which is where a .deb, an .rpm or a Homebrew formula
//	                              puts a containerd nobody should reach by typing it
//	<exe dir>                     a tarball or a build tree, everything side by side
func RuntimeDirs() []string {
	var candidates []string
	if dir := os.Getenv(runtimeDirEnv); dir != "" {
		candidates = append(candidates, dir)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, runtimeDirsFrom(exe)...)
	}
	return existingDirs(candidates)
}

// runtimeDirsFrom returns the bundle directories implied by a boks binary at exe, in the order
// they should be searched. Separate from RuntimeDirs so the ordering can be tested against a
// layout on disk rather than only against wherever the test binary happens to live.
func runtimeDirsFrom(exe string) []string {
	// Symlinks are resolved because a Homebrew install is a symlink farm in bin/ pointing
	// into the keg, and the bundle sits beside the real file.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)

	// The dedicated bundle directory BEFORE the directory boks itself sits in, and the
	// order is the whole point. A tarball install puts boks and the runtime in one
	// directory, so there the two are the same and this changes nothing. A package install
	// puts boks in /usr/bin — which on any real machine also holds the distribution's own
	// containerd — and the runtime in /usr/libexec/boks.
	//
	// Searching the executable's own directory first therefore preferred the system
	// containerd over the vendored one, which is the exact failure vendoring exists to
	// prevent. Measured on 2026-08-15 from an installed .deb: `boks daemon start` reported
	// "containerd v2.2.6" out of /usr/bin — below the 2.3 floor, the version that fails at
	// task start — while the 2.3.3 the package had just installed sat unused in
	// /usr/libexec/boks.
	return []string{filepath.Join(dir, "..", "libexec", "boks"), dir}
}

// existingDirs cleans, absolutises and de-duplicates candidates, keeping only directories that
// exist and preserving order.
func existingDirs(candidates []string) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, dir := range candidates {
		abs, err := filepath.Abs(filepath.Clean(dir))
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			dirs = append(dirs, abs)
		}
	}
	return dirs
}

// ErrNoContainerd means no containerd executable could be found anywhere Boks looks.
var ErrNoContainerd = errors.New("no containerd executable found")

// FindContainerd returns the containerd Boks will run.
//
// A bundled binary wins over one on PATH. That ordering is the entire point of bundling: the
// containerd Boks ships is pinned against the shim Boks ships, and version skew between those
// two produces failures that name neither — see internal/daemon/compat.go for the measured
// one. A user who wants their own can say so with BOKS_CONTAINERD_BINARY.
func FindContainerd() (string, error) {
	if pinned := os.Getenv(binaryEnv); pinned != "" {
		path, err := exec.LookPath(pinned)
		if err != nil {
			return "", fmt.Errorf("%s is set to %q, which is not an executable: %w", binaryEnv, pinned, err)
		}
		return path, nil
	}
	name := "containerd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, dir := range RuntimeDirs() {
		candidate := filepath.Join(dir, name)
		if executable(candidate) {
			return candidate, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: not bundled with boks and not on PATH", ErrNoContainerd)
	}
	return path, nil
}

// FindShim returns the runtime shim binary containerd would resolve the handler to, searching
// the bundle first and then the PATH the daemon will be given.
//
// It returns "" with no error when there is none: a host can legitimately have containerd and
// no shim yet, and that is `boks doctor`'s `vm runtime` line to report, not this function's.
func FindShim(handler string, path string) string {
	binary := runtimecfg.ShimBinary(handler)
	if binary == "" {
		return ""
	}
	for _, dir := range RuntimeDirs() {
		candidate := filepath.Join(dir, binary)
		if executable(candidate) {
			return candidate
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, binary)
		if executable(candidate) {
			return candidate
		}
	}
	return ""
}

// executable reports whether path is a regular file Boks could execute.
//
// The mode is not consulted on Windows, where it means nothing: a .exe is executable by being
// a .exe, and Go reports 0666 for it.
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

// ContainerdPath returns the PATH containerd is started with: the bundle directories first,
// then whatever this process inherited.
//
// Exported because it is not only what `boks daemon start` uses, it is the answer to "where
// will the shim look?" — the shim inherits containerd's environment and scans PATH for
// libkrun and for the guest images. Anything reasoning about what the shim can find has to
// reason about this list rather than about the PATH of whichever shell asked, and
// internal/doctor does.
func ContainerdPath(inherited string) string { return daemonPath(inherited) }

// daemonPath returns the PATH containerd is started with: the bundle directories first, then
// whatever this process inherited.
//
// Prepending rather than replacing is deliberate. containerd shells out to more than the shim
// — mkfs.erofs, most importantly — and a PATH containing only Boks' bundle would break a host
// that has erofs-utils installed normally, which is every host today.
func daemonPath(inherited string) string {
	dirs := RuntimeDirs()
	if len(dirs) == 0 {
		return inherited
	}
	seen := map[string]bool{}
	var out []string
	for _, dir := range append(dirs, filepath.SplitList(inherited)...) {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}
