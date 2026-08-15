package proclock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The contract every caller depends on: a held lock is visible to anyone who asks, a second
// taker is refused with ErrHeld and not with something else, and releasing frees it.
//
// The ErrHeld half is the part worth pinning. `boks net serve` once reported every failure of
// Acquire as "this sandbox already has a network supervisor", including failures that meant
// nothing of the sort, and the first line of the log then sent readers hunting for a process
// that had never existed.
func TestLockIsHeldExclusively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	if Locked(path) {
		t.Fatal("a lock file that does not exist reads as held")
	}

	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Locked(path) {
		t.Error("a lock this process holds does not read as held")
	}

	second, err := Acquire(path)
	if err == nil {
		second()
		t.Fatal("a held lock was taken twice")
	}
	if !errors.Is(err, ErrHeld) {
		t.Errorf("taking a held lock returned %v, which does not wrap ErrHeld", err)
	}

	release()
	if Locked(path) {
		t.Error("a released lock still reads as held")
	}
}

// A path that cannot be opened at all is not a held lock, and must not be reported as one.
func TestAcquireDistinguishesUnopenableFromHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", "x.lock")
	_, err := Acquire(path)
	if err == nil {
		t.Fatal("Acquire succeeded on a path with no directory")
	}
	if errors.Is(err, ErrHeld) {
		t.Errorf("a missing directory was reported as a held lock: %v", err)
	}
}

// Callers want "make sure it is gone", so ending a process that has already ended is a
// success rather than an error to handle.
func TestTerminateToleratesAProcessThatIsGone(t *testing.T) {
	cmd := shortLivedProcess(t)
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	if err := Terminate(pid); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Terminate on an exited process returned %v", err)
	}
}
