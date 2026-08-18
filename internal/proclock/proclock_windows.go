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
// **What has and has not run here.** acquire and locked have now executed on Windows: the
// native job in .github/workflows/windows.yml runs internal/cli's tests, whose holdDaemonLock
// takes a real lock, and `boks purge` correctly refused while it was held. terminate and detach
// have still never run — they compile for windows/amd64 and windows/arm64 and follow the APIs
// as Microsoft documents them, which is the whole of the claim for those two.
//
// openLockFile's share mode is reasoned from documented behaviour plus Go's own delete path,
// not from a run: what proves it is that same job going green on the --force case.

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
// openLockFile opens the lock file so that another process may still DELETE it.
//
// This is the whole reason it exists rather than os.OpenFile. Go opens files on Windows with
// FILE_SHARE_READ|FILE_SHARE_WRITE and no FILE_SHARE_DELETE, and a Windows file open without
// that flag cannot be unlinked by anybody: the attempt fails with a sharing violation. On Unix
// an flock does not stand in the way of unlink at all, so a lock file held by a live process is
// still deletable, and code written against that behaviour breaks here in a way that names the
// wrong thing.
//
// What it broke: `boks purge --force`. The purge refuses while the managed daemon is up and
// documents --force as the override, but the override could not remove the tree — RemoveAll
// reached daemon.lock and stopped with "The process cannot access the file because it is being
// used by another process". The refusal was overridable on Unix and not on Windows, and the
// error named a file rather than the daemon holding it.
//
// Deleting an open file is then permitted but not automatic: os.Remove asks for POSIX
// semantics (FILE_DISPOSITION_POSIX_SEMANTICS, Windows 10 1607 and later), which unlinks the
// name immediately instead of leaving a directory entry behind until the last handle closes.
// Without that the name would linger and RemoveAll would fail one level up with
// ERROR_DIR_NOT_EMPTY, so both halves are needed and only one of them is here.
//
// The share mode does not weaken the lock. Exclusion is LockFileEx's byte-range lock, not the
// share mode, and that is unchanged: a second acquire still fails with ERROR_LOCK_VIOLATION.
// What changes is only that the file may be removed while held, which is what the Unix side
// has always allowed.
func openLockFile(path string, create bool) (*os.File, error) {
	wide, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	disposition := uint32(windows.OPEN_EXISTING)
	if create {
		disposition = windows.OPEN_ALWAYS
	}
	h, err := windows.CreateFile(wide,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, disposition, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		// A PathError carrying the Errno, so that os.IsNotExist works on the result the
		// way it did for os.OpenFile — locked() reads a missing file as "nothing runs".
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func acquire(path string) (func(), error) {
	f, err := openLockFile(path, true)
	if err != nil {
		return nil, err
	}
	overlapped := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err != nil {
		f.Close()
		// Only a lock or sharing violation means somebody else holds it, and only
		// that case may claim so — the same rule the Unix implementation follows.
		// Without this the wrapper never carried ErrHeld on Windows, so every caller
		// that asks "is it held?" got false and reported the raw Win32 sentence
		// instead of its own. `boks daemon start` against a running daemon answered
		// "The process cannot access the file because another process has locked a
		// portion of the file" rather than naming the daemon and pointing at `boks
		// daemon status`.
		//
		// That is the second time this exact divergence has been shipped: ErrHeld
		// exists because `boks net serve` had it before, in the other direction. The
		// abstraction was added to fix it and this half never joined in.
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, fmt.Errorf("%w: %s (%v)", ErrHeld, path, err)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
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
	f, err := openLockFile(path, false)
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
