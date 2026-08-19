package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/network"
)

// describeRun is one `boks run`'s worth of network description, against whatever state
// directory the test has set up. It returns what the user would have seen on stderr.
func describeRun(t *testing.T, sandbox string, verbose bool, allow, inject []string) string {
	t.Helper()
	flags := (&policyFlags{allow: allow, inject: inject}).forAgent(mustAgent(t, "claude"))
	resolution, err := flags.resolution(sandbox, nil)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	spec := enforce.Spec{Sandbox: sandbox, Resolution: &resolution, Inject: inject, Intercept: true}
	var out bytes.Buffer
	if err := describeNetwork(flags, spec, network.ModeNAT, verbose, &out); err != nil {
		t.Fatalf("describeNetwork: %v", err)
	}
	return out.String()
}

func lineCount(s string) int { return len(strings.Split(strings.TrimRight(s, "\n"), "\n")) }

func mustAgent(t *testing.T, name string) agent.Agent {
	t.Helper()
	a, err := agent.Builtin().Resolve(name)
	if err != nil {
		t.Fatalf("agent %q: %v", name, err)
	}
	return a
}

// TestTheSecondRunIsQuiet is the whole of item 3: the first encounter is loud, and a run
// that has nothing new to say says almost nothing. Fifty lines before every command is how
// you train someone to grep past a security notice.
func TestTheSecondRunIsQuiet(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	first := describeRun(t, "boks-test", false, []string{"example.com:443"}, nil)
	if !strings.Contains(first, "example.com:443") || !strings.Contains(first, "terminated on the host") {
		t.Fatalf("the first run did not explain itself:\n%s", first)
	}
	if lineCount(first) < 20 {
		t.Errorf("the first run should be the educational one, got %d lines:\n%s", lineCount(first), first)
	}

	// The second run says NOTHING by default. This is stronger than it used to be: the
	// restatement was printed on every run until the flag inverted, and three identical
	// lines above every agent session is how someone learns to skip the network output —
	// which is precisely where a new interception host would have been.
	second := describeRun(t, "boks-test", false, []string{"example.com:443"}, nil)
	if strings.TrimSpace(second) != "" {
		t.Errorf("an unchanged policy printed something without -v:\n%s", second)
	}

	// With -v it is still short, and still points at the commands that show the detail.
	verbose := describeRun(t, "boks-test", true, []string{"example.com:443"}, nil)
	if n := lineCount(verbose); n > 3 {
		t.Errorf("an unchanged policy printed %d lines under -v:\n%s", n, verbose)
	}
	if !strings.Contains(verbose, "unchanged since this sandbox last ran") {
		t.Errorf("the summary does not say why it is short:\n%s", verbose)
	}
	// The detail is one command away, and the summary has to say which command —
	// including the agent, or it would name a command that prints a *different* policy
	// from the one this sandbox is running under.
	for _, want := range []string{"policy ls --sandbox boks-test --agent claude", "policy log --sandbox boks-test"} {
		if !strings.Contains(verbose, want) {
			t.Errorf("the summary does not point at %q:\n%s", want, verbose)
		}
	}
	second = verbose
	if strings.Contains(second, "terminated on the host") {
		t.Errorf("the enforcement note was repeated:\n%s", second)
	}

	// Another sandbox has been told nothing, so it gets the whole thing. The memory is
	// per sandbox because the text describes a sandbox's containment.
	other := describeRun(t, "boks-other", false, []string{"example.com:443"}, nil)
	if !strings.Contains(other, "terminated on the host") {
		t.Errorf("a sandbox that had never been told got the short form:\n%s", other)
	}
}

// TestAChangedPolicyIsShownAgain: the case a user must not miss is the policy having moved
// under them. Quietening the repeats must not quieten that.
func TestAChangedPolicyIsShownAgain(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	describeRun(t, "boks-test", false, []string{"example.com:443"}, nil)
	changed := describeRun(t, "boks-test", false, []string{"example.com:443", "second.test:443"}, nil)
	if !strings.Contains(changed, "second.test:443") {
		t.Errorf("a changed policy was not shown:\n%s", changed)
	}
	// The mechanism has not changed, though, so the paragraph about it is not repeated.
	if strings.Contains(changed, "terminated on the host") {
		t.Errorf("the enforcement note was repeated for a policy change:\n%s", changed)
	}
}

// TestANewInterceptionHostIsAlwaysAnnounced is the exception that makes the rest safe. A
// host whose TLS boks is about to terminate and that this sandbox has never been told about
// is announced whatever else is true — including under --quiet, and including when other
// hosts were announced before.
func TestANewInterceptionHostIsAlwaysAnnounced(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	first := describeRun(t, "boks-test", false, nil, []string{"anthropic@api.anthropic.com=x-api-key"})
	if !strings.Contains(first, "TLS INTERCEPTION") || !strings.Contains(first, "api.anthropic.com") {
		t.Fatalf("the first interception was not announced:\n%s", first)
	}

	// Same host, second run: it has been said, and saying it again every time is what
	// teaches people to skip it.
	repeat := describeRun(t, "boks-test", false, nil, []string{"anthropic@api.anthropic.com=x-api-key"})
	if strings.Contains(repeat, "TLS INTERCEPTION") {
		t.Errorf("an already-announced interception host was announced again:\n%s", repeat)
	}
	// The standing summary is chatter and now waits for -v, but it must not have LOST the
	// fact: a user who asks what is going on still has to be told their traffic is being
	// decrypted, or -v would describe a sandbox that sounds safer than it is.
	repeatVerbose := describeRun(t, "boks-test", true, nil, []string{"anthropic@api.anthropic.com=x-api-key"})
	if !strings.Contains(repeatVerbose, "TLS decrypted for 1 host") {
		t.Errorf("the summary stopped mentioning interception entirely:\n%s", repeatVerbose)
	}

	// A second host, added later, under --quiet. Both conditions are the ones that would
	// have hidden it if the rule were "say it once" or "say nothing when asked to be
	// quiet".
	added := describeRun(t, "boks-test", true, nil, []string{
		"anthropic@api.anthropic.com=x-api-key",
		"gh@api.github.com=bearer",
	})
	if !strings.Contains(added, "api.github.com") || !strings.Contains(added, "DECRYPT") {
		t.Errorf("a new interception host was not announced under --quiet:\n%s", added)
	}
	if !strings.Contains(added, "NEW: interception now covers api.github.com") {
		t.Errorf("the announcement does not say which host is the new one:\n%s", added)
	}
}

// TestTheFirstExplanationSurvivesAQuietRun: the default is quiet, and quiet may suppress only
// things a user can ask for again at any time. The first encounter with a policy is not one of
// those, so it is still printed — and if it were ever suppressed, it must not be recorded as
// having been shown, or the explanation would be consumed by a run that never made it.
func TestTheFirstExplanationSurvivesAQuietRun(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	// A first run with no -v still explains itself: a changed policy is news.
	first := describeRun(t, "boks-test", false, []string{"example.com:443"}, nil)
	if !strings.Contains(first, "terminated on the host") {
		t.Errorf("the first run did not explain itself without -v:\n%s", first)
	}
	// The second says nothing, which is the point of the inversion.
	if out := describeRun(t, "boks-test", false, []string{"example.com:443"}, nil); strings.TrimSpace(out) != "" {
		t.Errorf("an unchanged policy printed on a later run:\n%s", out)
	}
}

// TestNoNetworkIsStatedOnceThenSummarised: -net none is fixed for a sandbox's whole life, so
// it is worth stating in full once and abbreviating after.
func TestNoNetworkIsStatedOnceThenSummarised(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	flags := &policyFlags{mode: "none"}
	spec := enforce.Spec{Sandbox: "boks-test"}
	var first, second bytes.Buffer
	if err := describeNetwork(flags, spec, network.ModeNone, false, &first); err != nil {
		t.Fatal(err)
	}
	// The second run asks for detail; without -v it says nothing at all, which is asserted
	// below. The full notice on the FIRST run is news and prints either way — a sandbox
	// that silently has no network is the one thing worth stating unprompted.
	if err := describeNetwork(flags, spec, network.ModeNone, true, &second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "NETWORK: none") {
		t.Errorf("-net none did not announce itself without -v:\n%s", first.String())
	}
	if got := second.String(); !strings.Contains(got, "network: none") || lineCount(got) != 1 {
		t.Errorf("the second -net none run under -v should be one line, got:\n%s", got)
	}

	var quiet bytes.Buffer
	if err := describeNetwork(flags, spec, network.ModeNone, false, &quiet); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(quiet.String()) != "" {
		t.Errorf("a later -net none run printed without -v:\n%s", quiet.String())
	}
}

// TestNoticeMemoryFailsOpen: the record is a display convenience. If it cannot be read or
// written, the user gets the long form again — never silence.
func TestNoticeMemoryFailsOpen(t *testing.T) {
	dir := t.TempDir()
	if got := loadNotices(dir, "never-written"); got.Policy != "" || len(got.Intercept) != 0 {
		t.Errorf("a missing record read as %+v, want empty", got)
	}
	// A name that could escape the state directory is refused rather than followed.
	if p := noticePath(dir, "../../etc/passwd"); !strings.HasPrefix(p, dir) {
		t.Errorf("noticePath escaped the state directory: %q", p)
	}
	if p := noticePath(dir, ""); p != "" {
		t.Errorf("an empty sandbox name produced a path: %q", p)
	}
}
