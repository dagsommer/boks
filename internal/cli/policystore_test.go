package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
)

// policyState gives a test its own state directory, so that nothing it writes can reach the
// developer's real policy and nothing the developer has written can reach the test.
func policyState(t *testing.T) {
	t.Helper()
	t.Setenv("BOKS_STATE_DIR", t.TempDir())
}

// mustPolicy runs a policy subcommand and fails the test if it does not succeed.
func mustPolicy(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := runCLI(t, "", append([]string{"policy"}, args...)...)
	if err != nil {
		t.Fatalf("boks policy %s: %v\n%s", strings.Join(args, " "), err, errOut)
	}
	return out
}

func TestPolicyAllowAndDenyAreDurable(t *testing.T) {
	policyState(t)

	out := mustPolicy(t, "allow", "github.com:443", "-note", "git over HTTPS")
	if !strings.Contains(out, "added: allow github.com:443 to global") {
		t.Errorf("adding a global rule said:\n%s", out)
	}
	out = mustPolicy(t, "allow", "-sandbox", "web", "api.example.com:443")
	if !strings.Contains(out, "added: allow api.example.com:443 to sandbox web") {
		t.Errorf("adding a sandbox rule said:\n%s", out)
	}

	// A second process — which is what a second command is — must see both.
	out = mustPolicy(t, "ls", "-stored")
	for _, want := range []string{"global (every sandbox):", "github.com:443", "git over HTTPS", "sandbox web:", "api.example.com:443"} {
		if !strings.Contains(out, want) {
			t.Errorf("policy ls -stored is missing %q:\n%s", want, out)
		}
	}

	// Adding the same rule again is not an error and does not duplicate it.
	if out := mustPolicy(t, "allow", "github.com:443"); !strings.Contains(out, "already stored") {
		t.Errorf("re-adding a rule said:\n%s", out)
	}

	out = mustPolicy(t, "rm", "github.com:443")
	if !strings.Contains(out, "removed: allow github.com:443 from global") {
		t.Errorf("removing said:\n%s", out)
	}
	if out := mustPolicy(t, "ls", "-stored"); strings.Contains(out, "github.com") {
		t.Errorf("a removed rule is still listed:\n%s", out)
	}
}

// TestPolicyLsShowsScopes: precedence has to be unambiguous in what `ls` shows, not only in
// the code, so every rule is displayed with the scope that would have to change to remove it.
func TestPolicyLsShowsScopes(t *testing.T) {
	policyState(t)
	mustPolicy(t, "init", "-preset", "locked")
	mustPolicy(t, "allow", "github.com:443")
	mustPolicy(t, "deny", "-sandbox", "web", "evil.test")

	out := mustPolicy(t, "ls", "-sandbox", "web", "-allow", "flagged.test:443")
	for _, want := range []string{
		"effective policy for sandbox web",
		"deny (always wins):",
		"evil.test",
		"sandbox web",
		"github.com:443",
		"global",
		"flagged.test:443",
		"flag -allow",
		"layers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("policy ls is missing %q:\n%s", want, out)
		}
	}
}

// TestPolicyAllowWarnsWhenADenyStillWins: a rule that has no effect must not look like one
// that worked, and the way to be sure is to ask the engine.
func TestPolicyAllowWarnsWhenADenyStillWins(t *testing.T) {
	policyState(t)
	mustPolicy(t, "deny", "blocked.test")

	_, errOut, err := runCLI(t, "", "policy", "allow", "-sandbox", "web", "blocked.test:443")
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if !strings.Contains(errOut, "still denied") || !strings.Contains(errOut, "global") {
		t.Errorf("adding a futile allow said nothing useful:\n%s", errOut)
	}
}

// TestPolicyCheckAgreesWithTheEngine is the point of `policy check`. Its value is entirely in
// being the same answer the sandbox's network stack would give, so the test does not
// re-derive verdicts — it asks the engine and compares, destination by destination.
func TestPolicyCheckAgreesWithTheEngine(t *testing.T) {
	policyState(t)
	mustPolicy(t, "init", "-preset", "locked")
	mustPolicy(t, "allow", "global.test:443")
	mustPolicy(t, "deny", "blocked.test")
	mustPolicy(t, "allow", "-sandbox", "web", "scoped.test:443")
	mustPolicy(t, "allow", "-sandbox", "web", "blocked.test:443") // covered by the global deny
	mustPolicy(t, "allow", "-sandbox", "web", "10.0.0.0/8:5432")

	store, err := policy.LoadStore(policy.DefaultStorePath())
	if err != nil {
		t.Fatalf("loading the store the CLI wrote: %v", err)
	}

	for _, sandbox := range []string{"", "web", "other"} {
		res, err := (policy.Request{Store: store, Sandbox: sandbox}).Resolve()
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		pol, err := res.Policy()
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		// The engine, not a reimplementation of it: this is the object the network stack
		// and the proxy both consult, complete with its decision log.
		engine := policy.NewEngine(pol, policy.NewLog(64)).WithSandbox(sandbox)

		for _, destination := range []string{
			"global.test:443", "blocked.test:443", "scoped.test:443", "scoped.test:80",
			"unlisted.test:443", "10.1.2.3:5432", "10.1.2.3:22", "[::1]:443",
		} {
			target, err := policy.ParseTarget(destination, 443)
			if err != nil {
				t.Fatalf("target %s: %v", destination, err)
			}
			want := engine.Check(policy.StageConnect, target)

			args := []string{"check", destination}
			if sandbox != "" {
				args = append(args, "-sandbox", sandbox)
			}
			out := mustPolicy(t, args...)

			gotAllowed := strings.HasPrefix(out, "ALLOW ")
			if gotAllowed != want.Allowed {
				t.Errorf("check %s (sandbox %q) said %q; the engine said allowed=%v",
					destination, sandbox, firstLine(out), want.Allowed)
			}
			if want.Rule != "" && !strings.Contains(out, "rule:   "+want.Rule) {
				t.Errorf("check %s (sandbox %q) does not name the deciding rule %q:\n%s",
					destination, sandbox, want.Rule, out)
			}
			if !strings.Contains(out, "reason: "+want.Reason) {
				t.Errorf("check %s (sandbox %q) does not give the engine's reason %q:\n%s",
					destination, sandbox, want.Reason, out)
			}
		}
	}
}

// TestPolicyCheckReportsTheFlowMode covers the third thing check has to say: how the flow
// would be carried, in the same vocabulary the decision log uses.
func TestPolicyCheckReportsTheFlowMode(t *testing.T) {
	policyState(t)
	mustPolicy(t, "allow", "example.test:80,443,22")

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"check", "example.test:443"}, "forward-bypass"},
		{[]string{"check", "example.test:80"}, "forward"},
		{[]string{"check", "example.test:22"}, "transparent"},
		{[]string{"check", "example.test:443", "-inject", "svc@example.test=bearer"}, "forward —"},
		{[]string{"check", "example.test:443", "-net", "none"}, "no network at all"},
	}
	for _, tc := range cases {
		out := mustPolicy(t, tc.args...)
		if !strings.Contains(out, tc.want) {
			t.Errorf("%v does not report %q:\n%s", tc.args, tc.want, out)
		}
	}
}

// TestPolicyCheckIsHermetic: the command promises to answer without contacting anything, and
// the one way to keep that promise honest is to notice if it starts needing a sandbox.
func TestPolicyCheckIsHermetic(t *testing.T) {
	policyState(t)
	out := mustPolicy(t, "check", "-sandbox", "a-sandbox-that-does-not-exist", "example.test:443")
	if !strings.HasPrefix(out, "DENY ") {
		t.Errorf("check against an unknown sandbox said:\n%s", out)
	}
}

// TestPolicyStoreFailsClosedAtTheCLI: every command that reads the store must stop at a store
// it cannot read, rather than carry on with the built-in defaults, which may be wider.
func TestPolicyStoreFailsClosedAtTheCLI(t *testing.T) {
	policyState(t)
	mustPolicy(t, "init")
	corruptStore(t)

	for _, args := range [][]string{
		{"ls"},
		{"check", "example.test:443"},
		{"allow", "example.test:443"},
		{"deny", "example.test:443"},
		{"rm", "example.test:443"},
		{"inspect"},
		{"reset", "-f"},
		{"profile", "ls"},
	} {
		_, _, err := runCLI(t, "", append([]string{"policy"}, args...)...)
		if err == nil {
			t.Errorf("boks policy %s succeeded against a corrupt store", strings.Join(args, " "))
			continue
		}
		if !strings.Contains(err.Error(), "policy store") {
			t.Errorf("boks policy %s failed with %q, which does not name the cause", strings.Join(args, " "), err)
		}
	}
}

// TestRunRefusesToStartWithAnUnreadablePolicy is the same property where it matters most: a
// store nobody can read must cost a sandbox, not produce one with a policy nobody chose.
func TestRunRefusesToStartWithAnUnreadablePolicy(t *testing.T) {
	policyState(t)
	mustPolicy(t, "init")
	corruptStore(t)

	var flags policyFlags
	if _, err := flags.resolve(); err == nil {
		t.Fatal("a run resolved a policy from a corrupt store")
	}
}

func corruptStore(t *testing.T) {
	t.Helper()
	path := policy.DefaultStorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version": 1, "global": [{"action": "deny", "spec"`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyInitAndReset(t *testing.T) {
	policyState(t)

	out := mustPolicy(t, "init", "-preset", "locked")
	if !strings.Contains(out, "locked preset") {
		t.Errorf("init said:\n%s", out)
	}
	if _, _, err := runCLI(t, "", "policy", "init"); err == nil {
		t.Error("init overwrote an existing store without -force")
	}
	mustPolicy(t, "allow", "example.test:443")

	// reset asks before it destroys, and anything but an explicit yes is a no.
	out, errOut, err := runCLI(t, "n\n", "policy", "reset")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !strings.Contains(errOut, "Continue? [y/N]") {
		t.Errorf("reset did not ask first:\n%s", errOut)
	}
	if !strings.Contains(out, "cancelled") {
		t.Errorf("a declined reset said:\n%s", out)
	}
	if out := mustPolicy(t, "ls", "-stored"); !strings.Contains(out, "example.test:443") {
		t.Errorf("a declined reset destroyed rules anyway:\n%s", out)
	}

	// An empty stdin is not consent either.
	if out, _, err := runCLI(t, "", "policy", "reset"); err != nil || !strings.Contains(out, "cancelled") {
		t.Errorf("reset with no answer: %v\n%s", err, out)
	}

	out = mustPolicy(t, "reset", "-f")
	if !strings.Contains(out, "1 rule(s) removed") {
		t.Errorf("forced reset said:\n%s", out)
	}
	if out := mustPolicy(t, "ls", "-stored"); strings.Contains(out, "example.test") {
		t.Errorf("reset left rules behind:\n%s", out)
	}
}

func TestPolicyProfiles(t *testing.T) {
	policyState(t)
	mustPolicy(t, "profile", "create", "ci", "-preset", "locked",
		"-allow", "proxy.golang.org:443", "-description", "dependency fetch only")

	if out := mustPolicy(t, "profile", "ls"); !strings.Contains(out, "ci") || !strings.Contains(out, "dependency fetch only") {
		t.Errorf("profile ls said:\n%s", out)
	}
	mustPolicy(t, "allow", "-profile", "ci", "sum.golang.org:443")
	out := mustPolicy(t, "profile", "show", "ci")
	for _, want := range []string{"proxy.golang.org:443", "sum.golang.org:443", "profile ci"} {
		if !strings.Contains(out, want) {
			t.Errorf("profile show is missing %q:\n%s", want, out)
		}
	}

	// A profile is selected by name, and what it resolves to is what a run would get.
	out = mustPolicy(t, "check", "-profile", "ci", "proxy.golang.org:443")
	if !strings.HasPrefix(out, "ALLOW") || !strings.Contains(out, "profile ci") {
		t.Errorf("checking through a profile said:\n%s", out)
	}
	// And it still cannot unsay a deny written elsewhere.
	mustPolicy(t, "deny", "proxy.golang.org")
	out = mustPolicy(t, "check", "-profile", "ci", "proxy.golang.org:443")
	if !strings.HasPrefix(out, "DENY") {
		t.Errorf("a profile allow defeated a global deny:\n%s", out)
	}

	mustPolicy(t, "profile", "rm", "ci")
	if _, _, err := runCLI(t, "", "policy", "check", "-profile", "ci", "example.test:443"); err == nil {
		t.Error("a run selected a profile that does not exist")
	}
}

func TestPolicyInspect(t *testing.T) {
	policyState(t)
	mustPolicy(t, "allow", "github.com:443", "-note", "git")
	mustPolicy(t, "deny", "-sandbox", "web", "github.com")

	out := mustPolicy(t, "inspect", "github.com:443", "-sandbox", "web")
	for _, want := range []string{"allow github.com:443 in global", "deny github.com in sandbox web", "is denied"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect is missing %q:\n%s", want, out)
		}
	}
	// A pattern names a set, and inspect says so rather than inventing a destination.
	if out := mustPolicy(t, "inspect", "*.example.com"); !strings.Contains(out, "set of destinations") {
		t.Errorf("inspecting a pattern said:\n%s", out)
	}
}

// TestPolicyRejectsAmbiguousScope: -sandbox and -profile name two different places, and
// guessing which one was meant is how a rule ends up somewhere nobody looks.
func TestPolicyRejectsAmbiguousScope(t *testing.T) {
	policyState(t)
	if _, _, err := runCLI(t, "", "policy", "allow", "-sandbox", "web", "-profile", "ci", "a.test"); err == nil {
		t.Fatal("-sandbox and -profile together were accepted")
	}
}

// TestRecordedPolicyIsWhatARestartServes is the CLI half of the restart fix: a command with
// no policy flags — which is what `boks start` and `boks exec` are — must resolve the policy
// the sandbox recorded, not the default preset.
func TestRecordedPolicyIsWhatARestartServes(t *testing.T) {
	policyState(t)
	mustPolicy(t, "allow", "-sandbox", "web", "stored.test:443")

	record := &policy.SandboxPolicy{
		V:      policy.SandboxPolicyVersion,
		Preset: policy.PresetLocked,
		Allow:  []string{"allowed.test:443"},
		Deny:   []string{"blocked.test"},
	}

	var noFlags policyFlags
	res, err := noFlags.resolution("web", record)
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	pol, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		destination string
		allowed     bool
	}{
		{"allowed.test:443", true},  // the sandbox's own -allow, remembered
		{"stored.test:443", true},   // a rule added to the store after it was created
		{"blocked.test:443", false}, // its own -deny, remembered
		{"github.com:443", false},   // in the default preset, and so the tell-tale of a fallback
	} {
		target, err := policy.ParseTarget(tc.destination, 443)
		if err != nil {
			t.Fatal(err)
		}
		if v := pol.Evaluate(target); v.Allowed != tc.allowed {
			t.Errorf("%s allowed=%v, want %v (%s)", tc.destination, v.Allowed, tc.allowed, v.Reason)
		}
	}
}

// TestPerRunFlagsOverrideARecordWithoutLoosening pins the merge rule: a flag replaces the
// posture and the allow list, and adds to the denies. A run cannot drop a prohibition the
// sandbox was created with by typing a different one.
func TestPerRunFlagsOverrideARecordWithoutLoosening(t *testing.T) {
	policyState(t)
	record := &policy.SandboxPolicy{
		V:      policy.SandboxPolicyVersion,
		Preset: policy.PresetLocked,
		Allow:  []string{"recorded.test:443"},
		Deny:   []string{"blocked.test"},
	}
	flags := policyFlags{
		allow: stringList{"oneoff.test:443"},
		deny:  stringList{"blocked.test", "alsoblocked.test"},
	}
	res, err := flags.resolution("web", record)
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	pol, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		destination string
		allowed     bool
	}{
		{"oneoff.test:443", true},       // the flag's allow applies
		{"recorded.test:443", false},    // and replaces the recorded one
		{"blocked.test:443", false},     // the recorded deny survives
		{"alsoblocked.test:443", false}, // and the flag's deny is added
	} {
		target, err := policy.ParseTarget(tc.destination, 443)
		if err != nil {
			t.Fatal(err)
		}
		if v := pol.Evaluate(target); v.Allowed != tc.allowed {
			t.Errorf("%s allowed=%v, want %v (%s)", tc.destination, v.Allowed, tc.allowed, v.Reason)
		}
	}

	// A deny that appears in both the record and the flags is one rule, not two: a policy
	// that lists the same prohibition twice cannot be checked against what was written.
	blocked := 0
	for _, r := range res.Rules {
		if r.Action == policy.Deny && r.Spec == "blocked.test" {
			blocked++
		}
	}
	if blocked != 1 {
		t.Errorf("the same deny appears %d times in the resolved policy", blocked)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
