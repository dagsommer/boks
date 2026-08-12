package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/sandbox"
)

// TestRunRejectsAMalformedRule: a rule that cannot work must cost a message, not a sandbox.
// The check runs before anything is created, pulled or started.
func TestRunRejectsAMalformedRule(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	err := runCommand(context.Background(), Env{
		Args:   []string{dir, "-allow", "*.*.example.com"},
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errOut,
	})
	if err == nil {
		t.Fatal("an invalid -allow rule was accepted")
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("error %q should name the offending rule", err)
	}
}

// TestDescribeNetworkTellsTheUserWhatWillHappen covers what `boks run` owes a user at the
// moment they ask for a network: which destinations are permitted, that the policy is
// enforced by the stack rather than by an environment variable, and — the one that used to
// be missing entirely — which hosts boks is about to decrypt.
func TestDescribeNetworkTellsTheUserWhatWillHappen(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	flags := &policyFlags{
		allow:  stringList{"example.com:443"},
		inject: stringList{"anthropic@api.anthropic.com=x-api-key"},
	}
	spec := enforce.Spec{
		Sandbox:   "boks-test",
		Allow:     flags.allow,
		Inject:    flags.inject,
		Intercept: true,
	}
	var errOut bytes.Buffer
	if err := describeNetwork(flags, spec, network.ModeNAT, &errOut); err != nil {
		t.Fatalf("describeNetwork: %v", err)
	}
	got := errOut.String()
	for _, want := range []string{
		"example.com",                   // the policy it resolved to
		"TLS INTERCEPTION",              // the notice that had no call site before
		"api.anthropic.com",             // and the host it applies to
		"terminated on the host",        // where enforcement actually happens
		"Measured against a real guest", // and the evidence for it
		"Linux is not covered",          // and the limit of that evidence
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the description does not mention %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{
		"NOT enforced",         // the obsolete unenforced-policy warning
		"Not yet demonstrated", // superseded once a real guest was refused
	} {
		if strings.Contains(got, stale) {
			t.Errorf("obsolete wording %q is still printed:\n%s", stale, got)
		}
	}
}

// TestDescribeNetworkForNoNetwork: -net none has no policy to describe and must not imply
// one, but it must say what it is, because it is the strongest containment on offer.
func TestDescribeNetworkForNoNetwork(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	flags := &policyFlags{mode: "none", allow: stringList{"example.com"}}
	var errOut bytes.Buffer
	if err := describeNetwork(flags, enforce.Spec{Sandbox: "boks-test"}, network.ModeNone, &errOut); err != nil {
		t.Fatalf("describeNetwork: %v", err)
	}
	got := errOut.String()
	if !strings.Contains(got, "NETWORK: none") {
		t.Errorf("-net none did not announce itself:\n%s", got)
	}
	if !strings.Contains(got, "not applied") {
		t.Errorf("-net none did not say the policy flags do nothing:\n%s", got)
	}
	if strings.Contains(got, "HTTP_PROXY") {
		t.Errorf("-net none advertised a proxy that does not exist:\n%s", got)
	}
}

// TestNetworkForHonoursHowTheSandboxWasWired: the mode is fixed at creation, because it
// lives in annotations the runtime reads at boot. A -net flag that appeared to be obeyed
// while the container stayed connected would be the worst possible outcome.
func TestNetworkForHonoursHowTheSandboxWasWired(t *testing.T) {
	wired := invocation{
		name:   "shell-proj",
		exists: true,
		info: sandbox.Info{Annotations: map[string]string{
			"io.containerd.nerdbox.network.0":     "socket=/x,mode=unixgram,mac=aa:bb:cc:dd:ee:ff",
			"io.containerd.nerdbox.ctr.network.0": "vmmac=aa:bb:cc:dd:ee:ff,addr=192.168.127.2/24",
		}},
	}
	var errOut bytes.Buffer
	mode, ok := networkFor(wired, network.ModeNone, true, &errOut)
	if !ok || mode != network.ModeNAT {
		t.Errorf("networkFor = %v, %v; an existing sandbox keeps the network it was created with", mode, ok)
	}
	if !strings.Contains(errOut.String(), "fixed when a sandbox is created") {
		t.Errorf("the user was not told their -net flag was ignored:\n%s", errOut.String())
	}

	// A sandbox with no network annotations at all is not "no network": it is on the
	// runtime's default transport, which reaches host loopback. That has to be said.
	errOut.Reset()
	legacy := invocation{name: "old", exists: true, info: sandbox.Info{}}
	if _, ok := networkFor(legacy, network.ModeNAT, false, &errOut); ok {
		t.Error("a sandbox with no network annotations was reported as wired")
	}
	if !strings.Contains(errOut.String(), "TSI") {
		t.Errorf("the warning does not name the transport such a sandbox actually uses:\n%s", errOut.String())
	}
}

func TestPolicyFlagsSpecified(t *testing.T) {
	var f policyFlags
	if f.specified() {
		t.Error("empty flags reported as specified")
	}
	f.allow = stringList{"example.com"}
	if !f.specified() {
		t.Error("-allow was not detected")
	}
}
