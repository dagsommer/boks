//go:build !windows

package proclock

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// The Unix primitives. An flock is held by the kernel and released when the holder dies
// however it dies, so "can I take this lock" answers "is that process gone" with no window
// and no ambiguity. See proclock.go for why that is the liveness test rather than a PID.

// acquire takes the lock for the life of the process. The returned function releases it; the
// kernel releases it anyway if the process dies.
func acquire(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// Not a held lock: the file could not be opened at all. Saying so here is
		// what keeps the caller from reporting it as a live holder.
		return nil, fmt.Errorf("opening the lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		// Only this branch means somebody else is holding it, and only this branch
		// may claim so.
		return nil, fmt.Errorf("%w: %s (%v)", ErrHeld, path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// locked reports whether some live process is holding the lock.
func locked(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false // no lock file at all: nothing is running
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true // somebody else holds it
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// terminate asks a process to shut down. SIGTERM only, never SIGKILL: everything Boks runs
// this way has teardown that matters — the network supervisor removes its link socket and
// closes the stack, containerd unmounts and closes its database — and SIGKILL would leave all
// of it behind.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}

// detach puts the process in its own session, so that the Ctrl-C for the command that
// spawned it, and the closing of the terminal it was started from, do not reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// noConsole does nothing here. A Unix process has no console to be given, and a child whose
// standard streams point at a file is silent whatever terminal its parent had.
func noConsole(cmd *exec.Cmd) {}
