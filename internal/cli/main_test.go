package cli

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/dagsommer/boks/internal/daemon"
)

// TestMain keeps this package's tests from starting a real daemon.
//
// ensureDaemon exists so that `boks run` on an unprepared machine starts the daemon it needs
// instead of failing about a missing socket. Under test that is a hazard rather than a
// convenience: daemon.Start re-execs os.Executable(), which in a test is the TEST BINARY, as a
// DETACHED `daemon serve`. So every test invoking run, create, exec or start left a stray
// process behind that was a copy of cli.test — and waited for containerd to answer before
// giving up.
//
// On Linux that is invisible: unlink of a running executable succeeds and the orphan is
// somebody else's problem. On Windows it broke the build in a way that named nothing about it.
// Every package reported ok and `go test` still exited 1, on this line alone:
//
//	go: unlinkat C:\...\Temp\go-build785155958\b622\cli.test.exe: Access is denied.
//
// A running .exe cannot be deleted on Windows — os.Remove asks for POSIX semantics but also
// for FILE_DISPOSITION_FORCE_IMAGE_SECTION_CHECK, which refuses while the file is mapped as an
// executable image — so the go tool could not clean its own build cache. The same tests also
// took 133s against 37s before, which was the waiting.
//
// The three variables are already there for this, with a comment in autostart.go saying so;
// only the autostart tests were using them. Setting them here covers the package, and a test
// that wants different answers still overrides them locally as those tests do.
//
// daemonServing answers yes, which is what puts ensureDaemon on the path it took before it
// existed: the command proceeds and, if it really needs containerd, fails when it dials — the
// error the tests asserting that behaviour are written against. daemonStart panics rather than
// returning an error, because a test reaching it is a defect in the test rather than a
// condition to handle, and an error could be swallowed by a test that expects one.
func TestMain(m *testing.M) {
	daemonServing = func(context.Context, string) bool { return true }
	daemonStart = func(context.Context, string, io.Writer) (daemon.State, error) {
		panic("a test reached daemon.Start, which re-execs the test binary as a detached " +
			"process: override daemonStart/daemonServing in the test, as autostart_test.go does")
	}
	os.Exit(m.Run())
}
