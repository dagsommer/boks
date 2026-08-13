package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/network"
)

// describeRun is one `boks run`'s worth of network description, against whatever state
// directory the test has set up. It returns what the user would have seen on stderr.
func describeRun(t *testing.T, sandbox string, quiet bool, allow, inject []string) string {
	t.Helper()
	flags := &policyFlags{allow: allow, inject: inject}
	resolution, err := flags.resolution(sandbox, nil)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	spec := enforce.Spec{Sandbox: sandbox, Resolution: &resolution, Inject: inject, Intercept: true}
	var out bytes.Buffer
	if err := describeNetwork(flags, spec, network.ModeNAT, quiet, &out); err != nil {
		t.Fatalf("describeNetwork: %v", err)
	}
	return out.String()
}

func lineCount(s string) int { return len(strings.Split(strings.TrimRight(s, "\n"), "\n")) }

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

	second := describeRun(t, "boks-test", false, []string{"example.com:443"}, nil)
	if n := lineCount(second); n > 3 {
		t.Errorf("an unchanged policy printed %d lines:\n%s", n, second)
	}
	if !strings.Contains(second, "unchanged since this sandbox last ran") {
		t.Errorf("the summary does not say why it is short:\n%s", second)
	}
	// The detail is one command away, and the summary has to say which command.
	for _, want := range []string{"policy ls --sandbox boks-test", "policy log --sandbox boks-test"} {
		if !strings.Contains(second, want) {
			t.Errorf("the summary does not point at %q:\n%s", want, second)
		}
	}
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
	if !strings.Contains(repeat, "TLS decrypted for 1 host") {
		t.Errorf("the summary stopped mentioning interception entirely:\n%s", repeat)
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

// TestQuietSuppressesTheSummary: --quiet is for the tenth run, and it may suppress only
// things a user can ask for again at any time.
func TestQuietSuppressesTheSummary(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())

	if out := describeRun(t, "boks-test", true, []string{"example.com:443"}, nil); out != "" {
		t.Errorf("--quiet printed a network summary:\n%s", out)
	}
	// And it is not a way to skip the first encounter permanently: the policy still counts
	// as shown only once it has been shown, so an unquiet run afterwards still explains
	// itself.
	if out := describeRun(t, "boks-test", false, []string{"example.com:443"}, nil); !strings.Contains(out, "terminated on the host") {
		t.Errorf("a --quiet run swallowed the first explanation for good:\n%s", out)
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
	if err := describeNetwork(flags, spec, network.ModeNone, false, &second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "NETWORK: none") {
		t.Errorf("-net none did not announce itself:\n%s", first.String())
	}
	if got := second.String(); !strings.Contains(got, "network: none") || lineCount(got) != 1 {
		t.Errorf("the second -net none run should be one line, got:\n%s", got)
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
