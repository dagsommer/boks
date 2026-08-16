package doctor

import (
	"os"

	"github.com/dagsommer/boks/internal/daemon"
)

// Whose PATH the checks reason about.
//
// Every check here asks a question about something *containerd or the shim* will do — find
// the shim binary, dlopen libkrun, exec mkfs.erofs, read a guest image. None of them asks
// what the user's shell can find, and the two answers differ whenever Boks was installed as a
// package: the .deb and .rpm put containerd, the shim and libkrun.so in /usr/libexec/boks/,
// which is deliberately not on anyone's PATH — a containerd in /usr/bin would collide with
// the distribution's own. `boks daemon` prepends those directories to the PATH it starts
// containerd with, so the shim finds them; a check reading the invoking shell's PATH does not.
//
// The symptom that found this, on 2026-08-15, was a correctly installed .deb on which `boks
// doctor` reported `vm runtime fail`, `hypervisor library warn` and `guest image fail` while
// `runtime skew ok` — on the same run — named the version of the very shim the other three
// said was missing. Skew already went through daemon.FindShim; the others did not.
//
// The remedy text on one of those checks has said "Note that containerd's PATH is the
// daemon's, not your shell's" since long before this. The code said it and did not do it.

// shimGetenv is os.Getenv with PATH replaced by the PATH containerd is actually started with.
//
// Only PATH is substituted. LIBKRUN_PATH and the rest are read from the environment
// unchanged, because `boks daemon` passes those through untouched and the shim will see
// exactly what this process sees.
func shimGetenv(key string) string {
	if key == "PATH" {
		return daemon.ContainerdPath(os.Getenv("PATH"))
	}
	return os.Getenv(key)
}

// lookPath is daemon.LookPath, re-exported under the name the checks here use.
//
// It delegates rather than reimplementing. An earlier version of this file carried its own
// copy of the search, and internal/daemon carried another in HasEROFS that consulted the
// shell's PATH instead — so `boks doctor` reported mkfs.erofs present while `boks daemon
// start`, in the same install, wrote a config saying it was absent. Two implementations of
// one question is how they disagreed; there is now one.
func lookPath(binary string) (string, error) { return daemon.LookPath(binary) }
