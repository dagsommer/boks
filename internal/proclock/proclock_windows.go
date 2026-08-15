package proclock

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// The Windows primitives.
//
// This file used to refuse — all four functions returned "sandbox networking is not available
// on Windows" — and the refusal was honest at the time: its only caller was the per-sandbox
// network supervisor, which exists to hold one sandbox's link socket and run the netstack
// behind it, and Windows had no VMM that could put a guest's frames on such a socket. It has
// one now (see internal/network/vmm_windows.go), so stub primitives would make `boks run` fail
// on Windows for a reason that is no longer the real one — the worst kind of error message.
//
// **Nothing in this file has ever been executed.** It compiles for windows/amd64 and
// windows/arm64 and follows the APIs as Microsoft documents them; that is the whole of the
// claim.

// acquire takes the lock for the life of the process, as an exclusive byte-range lock on the
// lock file.
//
// LockFileEx with LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY is the Windows equivalent
// of the flock in lock_unix.go, and is what Go's own toolchain uses in
// cmd/go/internal/lockedfile. One byte at offset zero is locked rather than the whole file:
// the range is a token, nothing reads or writes the file's contents, and a one-byte range
// cannot be affected by the file's length.
//
// The returned function releases the lock explicitly. That is not decoration on this platform:
// see locked for why the implicit release at process death is the part that cannot be relied
// on to be prompt.
func acquire(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	overlapped := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("the lock %s is held: %w", path, err)
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
		_ = f.Close()
	}, nil
}

// locked reports whether some live process is holding the lock.
//
// # The one place Windows is not the same as Unix
//
// The Unix answer is exact. An flock is released by the kernel the instant the holder dies,
// so "can I take this lock" answers "is that process gone" with no window at all, and every
// command that asks gets a fact.
//
// Windows also releases a byte-range lock when the holder dies, but its own documentation
// declines to say when: "the time it takes for the operating system to unlock these locks
// depends upon available system resources", with an explicit recommendation to unlock
// explicitly instead. acquire does unlock explicitly, so a process that exits normally or is
// terminated through its own teardown leaves nothing behind. What remains is a **crashed**
// holder, whose lock may still look held for some unspecified interval after its process is
// gone.
//
// The consequence is worth stating rather than discovering, and it differs by caller. For the
// network supervisor, during that interval Lookup reports a network that is running when it is
// not, so `boks run` reuses a dead stack instead of replacing it, and the sandbox has no
// network with no warning — the very case orphanedStackWarning exists to catch. For the
// containerd supervisor, `boks daemon status` would report a daemon that is gone; that one is
// caught anyway, because status also asks containerd for its version and a dead daemon answers
// nothing. The next command after the interval sees the truth either way.
//
// This is not simulated to be better than it is: there is no retry here, because a retry would
// have to sleep on the common path (a live holder, which is exactly what a held lock usually
// means) to soften a rare one, and `boks net ls` walks every sandbox.
func locked(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false // no lock file at all: nothing is running
	}
	defer f.Close()
	overlapped := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err != nil {
		return true // somebody else holds it
	}
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
	return false
}

// terminate ends a process.
//
// Windows has no SIGTERM to send, and the graceful alternatives do not reach a detached
// process: GenerateConsoleCtrlEvent needs a shared console, and detach deliberately gives it
// none so that closing the terminal cannot take the sandbox's network — or the daemon — with
// it. So this is TerminateProcess, and the target's own teardown does not run.
//
// For the network supervisor nothing is lost that matters, and it is worth being precise about
// why rather than assuming it. The host listeners a published port binds are closed by the
// kernel with the process. The decision log is written a line at a time with no buffering, so
// no decision is dropped. The link socket and the state directory are removed by Stop, which
// does it after this returns for exactly this reason.
//
// For containerd the statement is weaker and is made in internal/daemon rather than here: a
// daemon killed this way does not close its database or unmount anything, and Boks says so
// instead of claiming a clean stop it did not perform.
//
// os.FindProcess is deliberately not used: on Windows it opens the process with
// PROCESS_QUERY_INFORMATION|SYNCHRONIZE and no PROCESS_TERMINATE, so the Kill that follows
// fails with access denied. The handle is opened here with the right to do the job.
func terminate(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// A pid nothing owns is reported as an invalid parameter rather than as a
		// missing object. The caller wants "make sure it is gone", so that is not an
		// error.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminating process %d: %w", pid, err)
	}
	return nil
}

// detach starts the process without a console and in a process group of its own, so that
// neither the Ctrl-C for the command that spawned it nor the closing of the terminal it was
// started from reaches it.
//
// DETACHED_PROCESS is what corresponds to setsid here: a process with no console receives no
// console control event at all, which covers both Ctrl-C and the close of the window.
// CREATE_NEW_PROCESS_GROUP is kept alongside it because the two answer different halves on
// Windows and the combination is what every long-lived child in the Go ecosystem uses. Neither
// affects the handles the child is given — the network supervisor's stdin pipe carries its
// spec, and its stderr is the sandbox's stack.log — because handle inheritance is independent
// of the console.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}
