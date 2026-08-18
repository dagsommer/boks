//go:build windows

// This test is here, in a windows-only file, because a runtime skip cannot save a build.
// It reads SysProcAttr.CreationFlags, a field that only exists on Windows, so with
// `if runtime.GOOS != "windows" { t.Skip(...) }` in a portable file the package failed to
// COMPILE on Linux and macOS — `go test ./internal/proclock/` could not run at all, skip or
// no skip. The constraint is the only thing that makes the reference legal.

package proclock

import (
	"os/exec"
	"testing"
)

// NoConsole and Detach must not become the same thing.
//
// They are one line apart and both answer "do not show me a window", which is exactly the
// shape of a future simplification that merges them — and merging them reintroduces the bug
// this function was written for. DETACHED_PROCESS leaves the process with no console at all,
// so every console program it starts is given a fresh one, window included: `boks daemon
// start` was invisible and the containerd underneath it opened a terminal window that stayed
// on screen for as long as the daemon ran. CREATE_NO_WINDOW gives a console and does not show
// it, and a console that exists is one its children inherit — which is what keeps the shim
// containerd starts per sandbox from opening a window of its own.
//
// The flags are read through the exported API rather than asserted as numbers, so this says
// "these two configure a process differently" rather than restating the constant.
func TestNoConsoleIsNotDetach(t *testing.T) {
	detached, windowless := &exec.Cmd{}, &exec.Cmd{}
	Detach(detached)
	NoConsole(windowless)

	if detached.SysProcAttr == nil || windowless.SysProcAttr == nil {
		t.Fatal("one of Detach and NoConsole configured nothing")
	}
	if detached.SysProcAttr.CreationFlags == windowless.SysProcAttr.CreationFlags {
		t.Errorf("Detach and NoConsole both set 0x%08x; NoConsole must not detach, or every "+
			"process started below it opens a console window of its own",
			detached.SysProcAttr.CreationFlags)
	}
}
