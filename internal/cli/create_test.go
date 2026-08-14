package cli

import (
	"strings"
	"testing"
)

// `boks create` writes one thing to stdout: the name of the sandbox it made.
//
// The name is the whole point of the command's output — `NAME=$(boks create shell .)` is how
// a script gets a handle on the sandbox it just built — so anything else on that stream
// breaks the caller silently, by handing it a name with a paragraph in front of it.
// Everything create says to a person goes to stderr instead: the isolation warning asserted
// here, the clone notes, and the policy explainer describeNetwork prints.
//
// The reach of this test is the part of create that runs before containerd is contacted,
// because the rest needs a daemon and an image. That is where the isolation warning is, and
// it is enough to state the contract in an executable form; the explainer itself is covered
// by TestDescribeNetworkTellsTheUserWhatWillHappen, which asserts against the writer create
// passes it.
func TestCreateWritesNothingButTheNameToStdout(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	// An address nothing can be listening on, so this can never reach a containerd that
	// happens to be running on the machine under test and start pulling an image.
	stdout, stderr, err := runCLI(t, "", "create",
		"--containerd-address", "/nonexistent/boks-create-test.sock",
		"--runtime", "io.containerd.runc.v2", "--i-know-this-is-not-isolated",
		"shell", t.TempDir())

	if err == nil {
		t.Fatal("create succeeded against an address nothing can answer")
	}
	if stdout != "" {
		t.Errorf("create wrote to stdout before it had a name to write: %q", stdout)
	}
	if !strings.Contains(stderr, "WARNING") {
		t.Errorf("the isolation warning is not on stderr:\n%s", stderr)
	}
}
