package cli

import (
	"bytes"
	"errors"
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

	_, _, err := runCLI(t, "", "run", dir, "--allow", "*.*.example.com")
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
		allow:  []string{"example.com:443"},
		inject: []string{"anthropic@api.anthropic.com=x-api-key"},
	}
	resolution, err := flags.resolution("boks-test", nil)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	spec := enforce.Spec{
		Sandbox:    "boks-test",
		Resolution: &resolution,
		Inject:     flags.inject,
		Intercept:  true,
	}
	var errOut bytes.Buffer
	if err := describeNetwork(flags, spec, network.ModeNAT, false, &errOut); err != nil {
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

	flags := &policyFlags{mode: "none", allow: []string{"example.com"}}
	var errOut bytes.Buffer
	if err := describeNetwork(flags, enforce.Spec{Sandbox: "boks-test"}, network.ModeNone, false, &errOut); err != nil {
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

// TestOrphanedStackWarningStatesTheMeasuredOutcome: this text used to say that whether a
// running guest re-attaches to a fresh link socket was "unverified", which a reader takes as
// "probably fine". It was measured on 2026-08-12 and it does not re-attach, so the sandbox
// has no network until it is restarted. Hedging here costs the user a debugging session.
func TestOrphanedStackWarningStatesTheMeasuredOutcome(t *testing.T) {
	got := orphanedStackWarning("web")
	for _, want := range []string{
		"does NOT re-attach", // the measured outcome, not a hope
		"2026-08-12",         // when it was measured
		"no network until it is restarted",
		"boks stop web && boks start web",  // the remedy, spelled out
		"kills whatever is running inside", // and its cost, so the user can decide
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unverified") {
		t.Errorf("the warning still calls a measured outcome unverified:\n%s", got)
	}
}

// TestUnexercisedNetworkWarningClaimsNothing covers the note printed before a sandbox is
// created on a platform where no frame has ever crossed the link — today, Windows.
//
// The condition is constructed rather than depended on: the warning takes the reason as an
// argument for exactly this, so the Windows text can be rendered on a machine that is not
// Windows. What it must do is refuse to promise anything, and name the failure that looks like
// success — a guest left on the runtime's own transport, where its 127.0.0.1 is the host's.
func TestUnexercisedNetworkWarningClaimsNothing(t *testing.T) {
	got := unexercisedNetworkWarning(errors.New("no frame has ever crossed this device on Windows"))
	for _, want := range []string{
		"WARNING",
		"attempt, not a claim",
		"no frame has ever crossed this device on Windows",
		"stack.log",
		"not contained",
		"boks policy log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"is supported", "works on Windows"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the warning claims %q, which nothing has shown:\n%s", unwanted, got)
		}
	}
}

func TestPolicyFlagsSpecified(t *testing.T) {
	var f policyFlags
	if f.specified() {
		t.Error("empty flags reported as specified")
	}
	f.allow = []string{"example.com"}
	if !f.specified() {
		t.Error("-allow was not detected")
	}
}
