//go:build !windows

package enforce

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// A supervisor's liveness is a held file lock, not a PID.
//
// A PID recorded in a file is a claim about the past: between a crash and the next command
// the number can belong to somebody else's process, and signalling a stranger is a worse
// failure than leaving a socket behind. An flock is held by the kernel and released when
// the holder dies however it dies, so "can I take this lock" answers "is that supervisor
// gone" with no window and no ambiguity.

// acquire takes the supervisor lock for the life of the process. The returned function
// releases it; the kernel releases it anyway if the process dies.
func acquire(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("the lock %s is held: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// locked reports whether a supervisor is holding the lock.
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

// terminate asks a supervisor to shut down. SIGTERM only: the supervisor's teardown removes
// the link socket and closes the stack, and SIGKILL would leave both behind.
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

// detach puts the supervisor in its own session, so that the Ctrl-C for the command that
// spawned it, and the closing of the terminal it was started from, do not reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
