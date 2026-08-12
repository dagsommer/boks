package enforce

import (
	"errors"
	"os/exec"
)

// There is no supervisor on Windows because there is nothing for it to supervise.
//
// A supervisor exists to hold one sandbox's link socket and run the netstack behind it. On
// Windows that stack has no link to hold: Hyper-V exposes no socket-backed NIC, so the
// guest's traffic never reaches a host process (see internal/network/gateway_windows.go and
// docs/windows.md). These stubs therefore refuse rather than pretending.
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
// the supervisor is one file away from working. It is not; it is one hypervisor feature away.

var errUnsupported = errors.New("enforce: sandbox networking is not available on Windows; " +
	"there is no host-terminated link for a supervisor to own (see docs/windows.md)")

func acquire(string) (func(), error) { return nil, errUnsupported }

func locked(string) bool { return false }

func terminate(int) error { return errUnsupported }

func detach(*exec.Cmd) {}
