package cli

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/sandbox"
)

// shortStateDir is t.TempDir() without the test's name in the path.
//
// t.TempDir() embeds the test function's name, and these names are long. On Windows the
// runner's temp root is long too, and the two together push a sandbox's link socket past the
// 104-byte sun_path limit Boks enforces — so a test about network *modes* failed on a path
// length, on Windows only, saying "use a shorter sandbox name". The limit is real and the
// check is right to be there; it is the fixture that was wrong.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestRunRejectsAMalformedRule: a rule that cannot work must cost a message, not a sandbox.
// The check runs before anything is created, pulled or started.
func TestRunRejectsAMalformedRule(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
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
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))

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
		"example.com",            // the policy it resolved to
		"TLS INTERCEPTION",       // the notice that had no call site before
		"api.anthropic.com",      // and the host it applies to
		"terminated on the host", // where enforcement actually happens
		"docs/security-model.md", // and where the limits of that are written down
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
	// And no lab notebook. This used to close with the date, the host and the hypervisor
	// that the claim was measured against, and a pointer to docs/verification.md. A user
	// can act on none of that: they need to know what boks does to their traffic, not which
	// machine proved it. The evidence record keeps the transcript; this text keeps the fact.
	if when := citationDate.FindString(got); when != "" {
		t.Errorf("the description cites a measurement date (%s), which belongs in "+
			"docs/verification.md and not in front of a user:\n%s", when, got)
	}
}

// citationDate matches a date printed at a user. Nothing describing a network says one
// except as a citation of when it was measured.
var citationDate = regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`)

// TestDescribeNetworkForNoNetwork: -net none has no policy to describe and must not imply
// one, but it must say what it is, because it is the strongest containment on offer.
func TestDescribeNetworkForNoNetwork(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))

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

	// The sandbox's own wiring wins, whatever this run asked for, and nothing is said
	// about it: the disagreement is refused earlier, by checkFixedAtCreation, which is
	// where every other flag a sandbox fixes at creation is refused too. See
	// TestReAttachRefusesANetworkModeItCannotApply for the refusal itself.
	mode, ok := networkFor(wired, network.ModeNone, &errOut)
	if !ok || mode != network.ModeNAT {
		t.Errorf("networkFor = %v, %v; an existing sandbox keeps the network it was created with", mode, ok)
	}
	if errOut.Len() != 0 {
		t.Errorf("a re-attach was told about a mode it never asked to change:\n%s", errOut.String())
	}

	// A sandbox with no network annotations at all is not "no network": it is on the
	// runtime's default transport, which reaches host loopback. That has to be said.
	errOut.Reset()
	legacy := invocation{name: "old", exists: true, info: sandbox.Info{}}
	if _, ok := networkFor(legacy, network.ModeNAT, &errOut); ok {
		t.Error("a sandbox with no network annotations was reported as wired")
	}
	if !strings.Contains(errOut.String(), "TSI") {
		t.Errorf("the warning does not name the transport such a sandbox actually uses:\n%s", errOut.String())
	}
}

// TestNetworkForHonoursAnExplicitFlagOnANewSandbox: the refusal is about a mode that is
// already fixed. A sandbox that does not exist yet has nothing to disagree with, so every
// mode is available and no flag is refused.
func TestNetworkForHonoursAnExplicitFlagOnANewSandbox(t *testing.T) {
	fresh := invocation{name: "shell-new", exists: false}
	for _, mode := range []network.Mode{network.ModeNone, network.ModeNAT} {
		var errOut bytes.Buffer
		got, ok := networkFor(fresh, mode, &errOut)
		if !ok || got != mode {
			t.Errorf("networkFor(new sandbox, -net %v) = %v, %v; want %v, true", mode, got, ok, mode)
		}
		if errOut.Len() != 0 {
			t.Errorf("a new sandbox was warned about its own -net flag:\n%s", errOut.String())
		}
	}
}

// TestUnexercisedWarningIsNotSaidWhenThereIsNoLinkToDial: on Windows, -net none printed
// "network: none — nothing leaves sandbox X." and then, on the very next line, a WARNING that
// boks was "attempting a sandbox network". There was no attempt to warn about.
//
// The warning's premise is a link the guest could dial that may be enforcing nothing. Under
// -net none the host end is a blackhole: the NIC exists only to switch the runtime's own
// transport off, and there is no stack, proxy or policy behind it. Every sentence the warning
// prints is false there, including the one telling the reader to check `boks policy log`.
//
// The platform answer is injected, so the Windows case is constructed on any host.
func TestUnexercisedWarningIsNotSaidWhenThereIsNoLinkToDial(t *testing.T) {
	windows := errors.New("this has never been seen carrying a frame on Windows")

	if warnUnexercisedNetwork(windows, network.ModeNone) {
		t.Error("-net none was warned about a network it does not have")
	}
	// The warning must survive for the mode it was written for; gating it on the mode
	// must not have turned it off everywhere.
	if !warnUnexercisedNetwork(windows, network.ModeNAT) {
		t.Error("-net nat on an unexercised platform was not warned about")
	}
	// And on a platform where the link has carried frames, neither mode warns.
	for _, mode := range []network.Mode{network.ModeNone, network.ModeNAT} {
		if warnUnexercisedNetwork(nil, mode) {
			t.Errorf("-net %v warned on a platform where the link is exercised", mode)
		}
	}

	// The gate reads spec.Plan.Mode, so the predicate being right is only half of it:
	// the mode has to survive the trip from the flag through planFor into the Plan the
	// call site actually holds. Walk that chain rather than trusting it.
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	for _, mode := range []network.Mode{network.ModeNone, network.ModeNAT} {
		plan, err := (&policyFlags{}).planFor("shell-proj", mode)
		if err != nil {
			t.Fatalf("planFor(%v): %v", mode, err)
		}
		if plan.Mode != mode {
			t.Fatalf("planFor(%v).Mode = %v; the field the warning gates on does not carry the mode",
				mode, plan.Mode)
		}
		if got, want := warnUnexercisedNetwork(windows, plan.Mode), mode != network.ModeNone; got != want {
			t.Errorf("warnUnexercisedNetwork(windows, planFor(%v).Mode) = %v; want %v", mode, got, want)
		}
	}
}

// TestOrphanedStackWarningStatesTheOutcome: this text used to say that whether a running
// guest re-attaches to a fresh link socket was "unverified", which a reader takes as
// "probably fine". It does not re-attach, so the sandbox has no network until it is
// restarted. Hedging here costs the user a debugging session.
//
// The date that outcome was measured on used to be pinned here too, and printed. It is in
// docs/verification.md, where a reader who wants the transcript can find it; in the warning
// it was a line of noise between the user and the command that fixes their sandbox.
func TestOrphanedStackWarningStatesTheOutcome(t *testing.T) {
	got := orphanedStackWarning("web")
	for _, want := range []string{
		"does NOT re-attach", // the outcome, stated, not a hope
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
	if when := citationDate.FindString(got); when != "" {
		t.Errorf("the warning cites a measurement date (%s); a user restarting a sandbox "+
			"cannot act on it:\n%s", when, got)
	}
}

// TestUnexercisedNetworkWarningClaimsNothing covers the note printed before a sandbox is
// created on a platform where no frame has ever crossed the link. Windows was that platform
// until 2026-08-14 and no platform is today, so this text is never printed on any host Boks
// currently runs on — which makes constructing the condition the only way to check it at all.
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
