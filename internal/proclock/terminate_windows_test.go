//go:build windows

package proclock

import (
	"testing"

	"golang.org/x/sys/windows"
)

// The condition the flaky failure needed, made deterministic.
//
// A Windows process object outlives the process while any handle to it exists, so an exited
// process can still be opened — and TerminateProcess on it fails with ERROR_ACCESS_DENIED.
// That reads like a permissions problem and is not one: there is nothing left to terminate.
//
// TestTerminateToleratesAProcessThatIsGone covers the same intent but reaches this state only
// by luck, which is why it passed on 2026-08-29 and failed on the next run with "terminating
// process 5136: Access is denied". Here the handle is held open deliberately, so the object is
// guaranteed to be there and guaranteed to be dead.
func TestTerminateToleratesAnExitedProcessWhoseObjectSurvives(t *testing.T) {
	cmd := shortLivedProcess(t)
	pid := cmd.Process.Pid

	// Opened BEFORE the process is reaped, so the object cannot go away underneath the
	// test no matter how the runtime handles the child.
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Skipf("cannot open the child process: %v", err)
	}
	defer windows.CloseHandle(h)

	_ = cmd.Wait()

	// Belt and braces: the exit above is what the child was started to do, but the wait
	// makes "it has exited" a fact this test established rather than assumed.
	if event, err := windows.WaitForSingleObject(h, 5000); err != nil || event != windows.WAIT_OBJECT_0 {
		t.Fatalf("the child did not exit: event=%d err=%v", event, err)
	}

	if err := Terminate(pid); err != nil {
		t.Errorf("Terminate on an exited process whose object still exists returned %v", err)
	}
}
