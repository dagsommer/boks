package enforce

import (
	"os/exec"

	"github.com/dagsommer/boks/internal/proclock"
)

// The supervisor's process primitives. They live in internal/proclock, because the containerd
// supervisor in internal/daemon owns a background process the same way and the rules — a held
// lock rather than a recorded PID, SIGTERM rather than SIGKILL, and the Windows caveat about
// when a dead holder's lock is actually released — are not obvious enough to transcribe twice.
//
// They are wrapped here rather than called through the package name so that the rest of this
// package, and its tests, read as they did when the implementations were in this directory.

// errLockHeld means the supervisor lock is held by another live process — that is, the
// sandbox really does have a network supervisor.
//
// The distinction it draws matters more here than anywhere else, and proclock.ErrHeld carries
// the history: only a lock somebody is provably holding may be reported as one, and every
// other failure keeps its own error. `boks net serve` used to answer every failure of acquire
// with `sandbox %q already has a network supervisor`, which told a Windows host that a fresh
// sandbox already had one running.
var errLockHeld = proclock.ErrHeld

func acquire(path string) (func(), error) { return proclock.Acquire(path) }

func locked(path string) bool { return proclock.Locked(path) }

func terminate(pid int) error { return proclock.Terminate(pid) }

func detach(cmd *exec.Cmd) { proclock.Detach(cmd) }
