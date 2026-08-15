package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// How Boks was installed, and therefore what to tell someone to type.
//
// "A new version exists" is only half a useful message. The other half is the one command that
// gets it, and that command differs per install method — `brew upgrade` on a machine where
// Boks came from a tarball does nothing, and telling someone to download a tarball when their
// package manager owns the binary invites two Boks installations that shadow each other.
//
// Detection is by path and by marker file only. Nothing here runs a subprocess: this is called
// on the `boks run` hot path, and forking `dpkg -S` to decorate a notice would cost more than
// the notice is worth. A wrong answer here is a suboptimal sentence, never a broken install,
// so cheap and usually-right beats thorough.

// Method is how a boks binary got onto the machine.
type Method int

const (
	// MethodUnknown means detection did not recognise the location. The advice is the
	// release page, which is correct for any install method.
	MethodUnknown Method = iota
	MethodHomebrew
	MethodWinget
	MethodDeb
	MethodRPM
)

// wingetPackage is the winget package identifier.
//
// This must match the PackageIdentifier in packaging/winget/. It is duplicated because the
// manifests are YAML submitted to microsoft/winget-pkgs and cannot be imported from Go; the
// pairing is asserted by a test that reads the manifest.
const wingetPackage = "dagsommer.boks"

// releasesPage is what to point at when nothing else is known.
const releasesPage = "https://github.com/dagsommer/boks/releases/latest"

// Detect works out how the running binary was installed.
//
// exePath is the path to the running executable — os.Executable() at the call site, passed in
// so this is testable. An empty path reports MethodUnknown rather than guessing from the
// platform, because "probably Homebrew, it is a Mac" is exactly the guess that produces a
// command the user cannot run.
func Detect(exePath string) Method {
	if exePath == "" {
		return MethodUnknown
	}
	// Symlinks are the normal case for a package-managed binary — Homebrew links
	// /opt/homebrew/bin/boks to a path inside the Cellar — and it is the target that
	// identifies the manager.
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	slashed := filepath.ToSlash(exePath)

	if runtime.GOOS == "windows" {
		// winget unpacks portable packages under a Packages directory in the user's
		// local app data, and shims them from Links. Matching on the path segment rather
		// than on an absolute prefix keeps this working for a machine-scope install,
		// which lives under ProgramFiles instead.
		lower := strings.ToLower(slashed)
		if strings.Contains(lower, "/winget/packages/") || strings.Contains(lower, "/winget/links/") {
			return MethodWinget
		}
		return MethodUnknown
	}

	// Homebrew: /opt/homebrew/Cellar/boks/... on Apple silicon, /usr/local/Cellar/... on
	// Intel, and an arbitrary prefix for a user-built install — the Cellar segment is the
	// invariant, and it is what a resolved symlink lands in.
	if strings.Contains(slashed, "/Cellar/boks/") || strings.Contains(slashed, "/homebrew/") {
		return MethodHomebrew
	}
	if runtime.GOOS == "linux" {
		return detectLinuxPackage(slashed)
	}
	return MethodUnknown
}

// detectLinuxPackage distinguishes a .deb from a .rpm install.
//
// Both put the binary in the same place, so the binary's path cannot tell them apart and the
// package databases are consulted instead — by the existence of a file dpkg or rpm maintains,
// not by running their tools. dpkg records a file list per package, whose presence is a direct
// statement that this package is installed; rpm has no equivalent per-package path, so the
// fallback is the database directory, which says only "this is an rpm system".
func detectLinuxPackage(slashed string) Method {
	// Only claim a package install for a binary in a system location. A tarball unpacked
	// into ~/.local/bin on a Debian machine is not owned by dpkg, and telling its owner to
	// run `apt upgrade` would be wrong in the way that wastes the most of their time.
	if !strings.HasPrefix(slashed, "/usr/bin/") && !strings.HasPrefix(slashed, "/usr/local/bin/") {
		return MethodUnknown
	}
	if _, err := os.Stat("/var/lib/dpkg/info/boks.list"); err == nil {
		return MethodDeb
	}
	if _, err := os.Stat("/var/lib/rpm"); err == nil {
		return MethodRPM
	}
	return MethodUnknown
}

// Upgrade returns the line to print telling someone how to get the new version.
func (m Method) Upgrade() string {
	switch m {
	case MethodHomebrew:
		return "brew upgrade boks"
	case MethodWinget:
		return "winget upgrade " + wingetPackage
	case MethodDeb:
		return "sudo apt update && sudo apt install --only-upgrade boks"
	case MethodRPM:
		return "sudo dnf upgrade boks"
	default:
		return releasesPage
	}
}
