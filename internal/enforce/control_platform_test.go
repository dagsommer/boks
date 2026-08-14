package enforce

import (
	"runtime"
	"strings"
	"testing"
)

// TestTheControlSocketIsBoundOnlyWhereItCanBeSecured pins the platform decision itself.
//
// The socket's protection is two things — a 0700 directory the kernel enforces, and a peer
// credential check where the platform can report one — and control.go's argument for binding
// it at all rests on both. Windows has neither: it ignores the permission bits Go passes, and
// its AF_UNIX carries no peer credentials for peerUID to read. Before `boks run` was allowed
// to attempt a sandbox on Windows that was inert, because no supervisor ever started there;
// it stopped being inert with that change, so the socket is not bound there.
//
// Guarded on runtime.GOOS rather than skipped, so that the assertion runs on both sides: on
// Unix the socket must still be bound (a regression here would take `boks ports` away from
// every platform), and on Windows it must still be refused.
func TestTheControlSocketIsBoundOnlyWhereItCanBeSecured(t *testing.T) {
	err := controlSocketSecurable()
	switch runtime.GOOS {
	case "windows":
		if err == nil {
			t.Error("the control socket would be bound on Windows, where neither the mode " +
				"it is created with nor the identity of its peer can be enforced")
		}
	default:
		if err != nil {
			t.Errorf("the control socket is refused on %s: %v", runtime.GOOS, err)
		}
	}
}

// TestTheControlRefusalNamesBothMissingProtections reads the text a Windows user gets from
// `boks ports` on a running sandbox.
//
// It is checked here because the Windows build is compiled in CI and executed nowhere, so this
// is the only place the sentence is ever looked at. Two things have to be in it: why the
// socket is absent — both halves, since either alone would look like an oversight — and what
// still works, because a refusal with no way forward reads as a bug.
func TestTheControlRefusalNamesBothMissingProtections(t *testing.T) {
	err := controlSocketRefusal()
	if err == nil {
		t.Fatal("controlSocketRefusal() returned nil")
	}
	for _, want := range []string{
		"not bound on Windows",
		"permission bits Windows ignores",
		"no peer credentials",
		"GetNamedPipeClientProcessId",
		"boks run --publish",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}
