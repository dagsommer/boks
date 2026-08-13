package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/ports"
)

// TestPortsColumnRendersEveryBinding: the PORTS column of `boks ls` was empty for the whole
// life of this project because nothing published anything. Now that something does, it has to
// show every binding rather than the first — a sandbox with two published ports and a column
// showing one is worse than the empty column was.
func TestPortsColumnRendersEveryBinding(t *testing.T) {
	if got := portsColumn(nil); got != "" {
		t.Errorf("portsColumn(nil) = %q, want empty so `ls` renders its dash", got)
	}
	got := portsColumn([]ports.Published{
		{HostIP: "127.0.0.1", HostPort: 8080, SandboxPort: 3000, Protocol: "tcp"},
		{HostIP: "::1", HostPort: 8080, SandboxPort: 3000, Protocol: "tcp"},
	})
	want := "127.0.0.1:8080->3000/tcp, [::1]:8080->3000/tcp"
	if got != want {
		t.Errorf("portsColumn = %q, want %q", got, want)
	}
}

// TestPortsTableSeparatesTheDiagnostic. The table goes to stdout so a script can parse it;
// the reason a port is not reaching the guest goes to stderr, because it is prose and because
// it would otherwise turn a parseable listing into a surprise.
func TestPortsTableSeparatesTheDiagnostic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writePortsTable(&stdout, &stderr, []ports.Published{{
		HostIP: "127.0.0.1", HostPort: 8080, SandboxPort: 3000, Protocol: "tcp",
		LastError: "nothing answered on port 3000 inside the sandbox",
	}})

	if !strings.Contains(stdout.String(), "127.0.0.1:8080") || !strings.Contains(stdout.String(), "3000") {
		t.Errorf("the table does not show the binding: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "nothing answered") {
		t.Errorf("the diagnostic is on stdout, where it breaks parsing: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "nothing answered") {
		t.Errorf("the diagnostic never reached the user: %q", stderr.String())
	}
}

// TestPortsTableSaysNothingIsPublished rather than printing an empty table, which reads like a
// failure of the command.
func TestPortsTableSaysNothingIsPublished(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writePortsTable(&stdout, &stderr, nil)
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for an empty listing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no ports") {
		t.Errorf("stderr = %q, want it to say the sandbox publishes nothing", stderr.String())
	}
}

// TestPublishFlagsAreValidatedBeforeAnythingHappens. A `boks run -p` with a typo must cost a
// message, not a sandbox and an image pull — and a UDP specification must be met with the
// reason rather than with a parse error, since the grammar does accept it.
func TestPublishFlagsAreValidatedBeforeAnythingHappens(t *testing.T) {
	good := &policyFlags{publish: []string{"3000", "8080:3000", "127.0.0.1:8080:3000/tcp4"}}
	if err := good.checkPublish(); err != nil {
		t.Errorf("a valid set of specifications was rejected: %v", err)
	}

	bad := &policyFlags{publish: []string{"8080:not-a-port"}}
	if err := bad.checkPublish(); err == nil {
		t.Error("a malformed specification was accepted")
	}

	udp := &policyFlags{publish: []string{"3000/udp"}}
	err := udp.checkPublish()
	if err == nil {
		t.Fatal("a UDP specification was accepted")
	}
	if !strings.Contains(err.Error(), "drops UDP at the link") {
		t.Errorf("the refusal does not give the reason: %v", err)
	}
}

// TestPublishIsIgnoredWhenReattaching pins sbx's documented behaviour, and the note that makes
// it visible: a flag that looks obeyed and is not is the worst outcome here, because the user
// would go looking for their port on the host and find nothing.
func TestPublishIsIgnoredWhenReattaching(t *testing.T) {
	flags := &policyFlags{publish: []string{"9999:3000"}}

	var stderr bytes.Buffer
	fresh := publishFor(flags, invocation{name: "new", exists: false}, &stderr)
	if len(fresh) != 1 || fresh[0] != "9999:3000" {
		t.Errorf("a new sandbox got %v, want the flags", fresh)
	}
	if stderr.Len() != 0 {
		t.Errorf("a new sandbox was warned about nothing: %q", stderr.String())
	}

	stderr.Reset()
	existing := invocation{name: "web", exists: true}
	existing.info.Ports = []string{"8080:3000"}
	got := publishFor(flags, existing, &stderr)
	if len(got) != 1 || got[0] != "8080:3000" {
		t.Errorf("an existing sandbox got %v, want the ports it was created with", got)
	}
	if !strings.Contains(stderr.String(), "ignored when re-attaching") {
		t.Errorf("the user was not told the flag was ignored: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "boks ports web --publish 9999:3000") {
		t.Errorf("the note does not say how to do what was asked: %q", stderr.String())
	}
}
