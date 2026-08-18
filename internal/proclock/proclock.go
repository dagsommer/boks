// Package proclock is the handful of process primitives Boks needs to own a background
// process it did not stay attached to: take a lock that dies with its holder, ask whether
// somebody else holds it, end a process, and start one that outlives the command which
// spawned it.
//
// It exists because two different things in Boks now own such a process, and they must own
// it the same way. The per-sandbox network supervisor (internal/enforce) was the first; the
// containerd supervisor (internal/daemon) is the second. The rules below were worked out for
// the first and are not obvious enough to rediscover for the second — the Windows note on
// Locked in particular is a caveat, not an implementation detail — so they live in one place
// with one set of documentation rather than being transcribed.
//
// Liveness is a held lock, never a recorded PID. A PID in a file is a claim about the past:
// between a crash and the next command the number can belong to somebody else's process, and
// signalling a stranger is a worse failure than leaving a socket behind. A lock is held by
// the kernel and released when the holder dies however it dies, so "can I take this" answers
// "is that process gone".
//
// Terminate is the one place a PID is used, and callers must establish for themselves that
// the PID is still the process they mean. There are two ways to do that, and both are used
// in this repository: hold the lock check immediately before (internal/enforce), or signal a
// child whose parent is provably alive and has not reaped it, which reserves the number on
// both platforms (internal/daemon).
package proclock

import (
	"errors"
	"os/exec"
)

// ErrHeld means the lock is held by another live process.
//
// It exists because "I could not take the lock" and "somebody else has it" are not the same
// statement, and reporting them with one sentence produced an error that was simply false.
// `boks net serve` used to answer every failure of Acquire with `sandbox %q already has a
// network supervisor`, so a Windows host — where Acquire refused outright, because there was
// no host-terminated link for a supervisor to own — was told a fresh sandbox already had one
// running. The true cause was wrapped inside as a detail, and the first line of the log sent
// the reader hunting for a process that had never existed.
//
// Only the platform primitives below may attribute a failure to this sentinel, and only for a
// lock another holder is provably holding. Everything else — a directory that cannot be
// written, a platform that has no supervisor to run, a failure that is yet to exist — keeps
// its own error and is reported as itself.
var ErrHeld = errors.New("another process holds the lock")

// Acquire takes the lock at path for the life of this process. The returned function
// releases it; on Unix the kernel releases it anyway if the process dies, and on Windows it
// does so eventually — see Locked for why "eventually" is worth knowing about.
//
// A failure to take a lock somebody else holds, and only that, wraps ErrHeld.
func Acquire(path string) (func(), error) { return acquire(path) }

// Locked reports whether some live process holds the lock at path.
func Locked(path string) bool { return locked(path) }

// Terminate ends a process by PID. Ending one that is already gone is not an error: callers
// want "make sure it is gone".
func Terminate(pid int) error { return terminate(pid) }

// Detach configures cmd so that the process it starts outlives the command that spawned it:
// no Ctrl-C meant for us, and no death with the terminal we were started from.
//
// It must be called before Start.
func Detach(cmd *exec.Cmd) { detach(cmd) }

// NoConsole configures cmd so that the process it starts shows no console window. It is a
// no-op everywhere except Windows, where a console program with nothing to inherit is given
// a console of its own — and that console is a window on the user's screen.
//
// It is not Detach. Detach is for a process that must survive this one; this is for a child
// whose output is already going somewhere else and whose window would be noise. The two are
// separate because they are not the same statement, and on Windows they resolve to different
// creation flags for a reason worth knowing — see the implementation.
//
// It must be called before Start.
func NoConsole(cmd *exec.Cmd) { noConsole(cmd) }
