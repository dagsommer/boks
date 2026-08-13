package enforce

import (
	"errors"
	"os/exec"
)

// There is no supervisor on Windows because there is nothing yet for it to supervise.
//
// A supervisor exists to hold one sandbox's link socket and run the netstack behind it. On
// Windows there is no such link, because Boks has no VMM that speaks the Windows Hypervisor
// Platform and therefore nothing that emits a guest's frames onto a host socket (see
// internal/network/vmm_windows.go and docs/windows.md). The design is sound there — the
// reference product runs the same shape on Windows — it simply has no VM under it. These
// stubs therefore refuse rather than pretending.
//
// This is deliberately *not* an unimplemented file-locking primitive. The Windows equivalent
// of the flock in lock_unix.go is known and would be `LockFileEx` with
// `LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY`, which is what Go's own toolchain uses
// in cmd/go/internal/lockedfile — with one semantic difference worth writing down before
// anybody implements it: Windows releases a byte-range lock when the holder dies, but the
// documentation states the release is not prompt ("the time it takes for the operating system
// to unlock these locks depends upon available system resources"). The Unix version relies on
// release being immediate to answer "is that supervisor gone" with no window. On Windows that
// answer would need a short retry before it could be trusted.
//
// Writing that primitive now would produce a correct lock with no caller, and would suggest
// the supervisor is one file away from working. It is not; it is a whole VMM away.

var errUnsupported = errors.New("enforce: sandbox networking is not available on Windows; " +
	"there is no host-terminated link for a supervisor to own (see docs/windows.md)")

func acquire(string) (func(), error) { return nil, errUnsupported }

func locked(string) bool { return false }

func terminate(int) error { return errUnsupported }

func detach(*exec.Cmd) {}
