package workspace

import (
	"fmt"
	"path"
	"runtime"
	"strings"
)

// pathStyle is the grammar a host path is written in.
//
// It exists so that the Windows mapping is decided by the *host*, once, rather than by
// inspecting the string. A Unix directory may legitimately be called `C:\Users` — a colon and
// a backslash are both ordinary filename characters on Linux, and Parse already has a test
// saying so — so a mapping that sniffed the argument would mangle a real directory on a Unix
// host. The style is therefore an input, which also lets the Windows rules be tested on Linux.
type pathStyle int

const (
	stylePOSIX pathStyle = iota
	styleWindows
)

// hostStyle reports the grammar of the machine Boks is running on. This is the only place in
// Boks that asks what the host OS is for path purposes; everything downstream takes a
// pathStyle, so the knowledge does not spread.
func hostStyle() pathStyle {
	if runtime.GOOS == "windows" {
		return styleWindows
	}
	return stylePOSIX
}

// guestPath maps an absolute, resolved host path to where that directory appears in the guest.
//
// On a POSIX host this is the identity, and that is the whole point of Boks: the host path
// *is* a Linux path, so the workspace is mounted at it verbatim and every absolute path keeps
// working on both sides.
//
// On Windows it cannot be. `C:\Users\dag\src\foo` is not a Linux path — there is no drive
// letter to preserve, and both the colon and the backslash are legal characters in a Linux
// filename, so mounting it verbatim would produce one directory with a very strange name
// rather than a path tree. Something has to translate, and Boks translates the way the Docker
// family has since docker-machine: lowercase the drive letter, drop the colon, backslashes
// become forward slashes, prefix a slash. `C:\Users\dag\src\foo` -> `/c/Users/dag/src/foo`.
//
// That spelling is not a preference. It is what Docker Sandboxes does — a guest's
// /proc/mounts on a Windows 11 machine shows `bind-f366aeed8fafcfc3
// /c/Users/E194604/source/repos/DigitalPostNy virtiofs rw,relatime` — and what Git Bash,
// MSYS2, minikube, boot2docker and Docker Compose's COMPOSE_CONVERT_WINDOWS_PATHS have done
// for over a decade. Boks exists for parity with that product; a different spelling would buy
// nothing but surprise. docs/windows.md section 4 has the evidence and the rejected
// alternatives.
//
// Nothing in this function has ever been executed on Windows.
func guestPath(host string, style pathStyle) (string, error) {
	if style != styleWindows {
		return host, nil
	}
	return windowsGuestPath(host)
}

// windowsGuestPath implements the mapping described on guestPath.
//
// It takes the path the caller has already made absolute and symlink-resolved. Resolving
// first is what makes the mapping total: `..`, `.`, drive-relative spellings and junctions are
// all gone before the grammar is inspected, so the only thing left to do is transliterate.
func windowsGuestPath(host string) (string, error) {
	// A NUL truncates every C string this destination passes through — the OCI spec, the
	// shim, the virtiofs daemon — so a path containing one would be silently shortened to a
	// *different* path and shared instead. Windows filenames cannot contain NUL, so this
	// only fires on something synthetic, which is exactly when silence would be worst.
	if strings.ContainsRune(host, 0) {
		return "", fmt.Errorf("workspace path %q contains a NUL byte", host)
	}

	// Accept the forward-slash spelling throughout. Go, PowerShell, git and most CLI tools
	// take `C:/Users/dag`, so someone who typed it must reach the same share as someone who
	// typed `C:\Users\dag` — two spellings of one directory producing two guest paths would
	// be two mounts of the same host tree at different places inside one sandbox.
	p := strings.ReplaceAll(host, `\`, "/")

	// `\\?\C:\...` is the extended-length form. It arrives from Windows itself rather than
	// from a user — filepath.EvalSymlinks hands it back for a path over MAX_PATH and for
	// some reparse points — and names the same file as the path without it.
	if rest, ok := strings.CutPrefix(p, "//?/"); ok {
		// `\\?\UNC\server\share` is a UNC path wearing the prefix. Put it back into UNC
		// shape so it earns the UNC refusal below rather than a confusing complaint
		// about a missing drive letter.
		if after, isUNC := cutPrefixFold(rest, "UNC/"); isUNC {
			p = "//" + after
		} else {
			p = rest
		}
	}

	// UNC (`\\server\share\...`) and the device namespace (`\\.\...`) are refused rather
	// than mapped, deliberately. There is no established guest spelling for a UNC path —
	// WSL does not automount them either — so any mapping Boks invented would be one no
	// other tool agrees with, and an SMB share reached through virtiofs inside a microVM is
	// a failure mode nobody wants to debug. Refusing is the honest answer; a wrong path
	// would be discovered as data appearing in the wrong place.
	if strings.HasPrefix(p, "//") {
		return "", fmt.Errorf(
			"workspace %q is a UNC or device path; Boks shares local drives only.\n"+
				"Map the share to a drive letter, or copy the project onto one", host)
	}

	if len(p) < 2 || p[1] != ':' || !isDriveLetter(p[0]) {
		return "", fmt.Errorf("workspace %q has no drive letter; Boks maps paths of the form C:\\dir into the guest", host)
	}
	// Lowercase, always. `C:` and `c:` are one drive on a case-insensitive filesystem and
	// must be one directory in a case-sensitive guest, and the letter is lowercase in every
	// tool that implements this convention — /mnt/C does not exist under WSL either.
	drive := strings.ToLower(p[:1])
	rest := p[2:]

	if !strings.HasPrefix(rest, "/") {
		// `C:foo` is relative to the current directory *of that drive*, which is
		// per-process state with no guest equivalent. Callers resolve with filepath.Abs
		// before mapping, so reaching here means the path was never resolved. Guessing a
		// root would share a directory the user did not name.
		return "", fmt.Errorf("workspace %q is relative to drive %s:; resolve it to an absolute path first", host, drive)
	}

	// path.Clean, not filepath.Clean: the result is a *guest* path, and filepath follows the
	// host's separator, which on a Windows host would turn it straight back into
	// backslashes. Clean also collapses the repeated and trailing separators that a
	// hand-typed path carries, so `C:\Users\x\` and `C:\Users\x` are one share.
	guest := path.Clean("/" + drive + rest)

	// The result is rooted at the drive by construction: the input is rooted, and Clean
	// resolves every ".." in a rooted path rather than letting one escape. This asserts it
	// anyway, because the failure it guards against is a mount pointing at a host directory
	// the user never named, which is a vulnerability rather than a bug.
	root := "/" + drive
	if guest != root && !strings.HasPrefix(guest, root+"/") {
		return "", fmt.Errorf("workspace %q maps to %q, outside drive root %q; refusing", host, guest, root)
	}
	return guest, nil
}

// isDriveLetter reports whether c can name a Windows drive. Drives are ASCII letters only, so
// this is deliberately not unicode.IsLetter: a non-ASCII first character means the path is not
// a drive path at all, and should be refused rather than lowercased into one.
func isDriveLetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// cutPrefixFold is strings.CutPrefix with an ASCII case-insensitive comparison, for the one
// place that needs it: Windows accepts `\\?\UNC\` in any case.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}
