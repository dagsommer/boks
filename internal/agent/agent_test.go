package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
)

// The built-in set is sbx's, so that a habit formed there works here.
func TestBuiltinNames(t *testing.T) {
	want := []string{"claude", "codex", "copilot", "cursor", "docker-agent", "droid", "gemini", "kiro", "opencode", "shell"}
	got := Builtin().Names()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
}

// Every agent Boks ships an image for runs without being told one, and every one of those
// images is a Boks image at the published tag.
func TestRunnableAgentsPointAtBoksImages(t *testing.T) {
	r := Builtin()
	shell, ok := r.Lookup("shell")
	if !ok {
		t.Fatal("the default agent is not registered")
	}
	// The shell agent is the base image itself, not an image of its own.
	if shell.Image != Image("base") {
		t.Errorf("shell image = %q, want the base image %q", shell.Image, Image("base"))
	}
	if err := RequireRunnable(shell); err != nil {
		t.Errorf("RequireRunnable(shell) = %v, want nil", err)
	}

	for _, a := range r.All() {
		if !a.Runnable() {
			continue
		}
		if !strings.HasPrefix(a.Image, ImageRepo+"/") {
			t.Errorf("agent %q runs %q, which is not a Boks image", a.Name, a.Image)
		}
		if !strings.HasSuffix(a.Image, ":"+ImageTag) {
			t.Errorf("agent %q runs %q, which is not at the published tag %q", a.Name, a.Image, ImageTag)
		}
		// An image Boks builds carries tini and the CA entrypoint, and a sandbox
		// bypasses the image's own ENTRYPOINT, so the definition has to name them.
		if len(a.Init) == 0 {
			t.Errorf("agent %q has an image but no init prefix", a.Name)
		}
	}
}

// An agent's allowlist becomes a default allow in every sandbox running that agent, so this
// is the test that guards what may go in one. Each entry has to parse, carry the reason it
// is there, name a port, and not be a wildcard over somebody else's tenants.
func TestAgentAllowlistsAreNarrowAndExplained(t *testing.T) {
	// Domains where a wildcard would allow content someone else controls. Those are the
	// wildcards worth refusing: `*.githubcopilot.com` is GitHub's own service domain and
	// is fine, `*.githubusercontent.com` would be every user's repository content.
	multiTenant := []string{
		"githubusercontent.com", "googleapis.com", "amazonaws.com", "blob.core.windows.net",
		"cloudfront.net", "s3.amazonaws.com", "pages.dev", "workers.dev",
	}
	// Telemetry endpoints seen while researching these lists. None of them belongs in a
	// default allowlist: a run with one of them blocked was observed to break nothing.
	telemetry := []string{
		"statsig", "sentry", "datadog", "segment", "ab.chatgpt.com", "collector.github.com",
		"exp-tas.com", "copilot-telemetry", "play.googleapis.com", "firebaselogging",
		"clearcut", "amplitude", "posthog", "mixpanel",
	}

	for _, a := range Builtin().All() {
		for _, d := range a.Allow {
			rule, err := policy.ParseRule(policy.Allow, d.Spec)
			if err != nil {
				t.Errorf("agent %q: %q does not parse: %v", a.Name, d.Spec, err)
				continue
			}
			if d.Why == "" {
				t.Errorf("agent %q: %q has no reason attached", a.Name, d.Spec)
			}
			if rule.Ports.Any() {
				t.Errorf("agent %q: %q allows every port; pin it to 443", a.Name, d.Spec)
			}
			if strings.HasPrefix(d.Spec, "*:") || d.Spec == "*" {
				t.Errorf("agent %q: %q is a catch-all", a.Name, d.Spec)
			}
			for _, host := range multiTenant {
				if strings.HasPrefix(d.Spec, "*.") && strings.Contains(d.Spec, host) {
					t.Errorf("agent %q: %q wildcards a multi-tenant domain", a.Name, d.Spec)
				}
			}
			for _, t9y := range telemetry {
				if strings.Contains(d.Spec, t9y) {
					t.Errorf("agent %q: %q looks like telemetry, which is not a default allow", a.Name, d.Spec)
				}
			}
		}
	}

	// The one entry confirmed by a real run rather than by reading has to stay.
	claude, _ := Builtin().Lookup("claude")
	found := false
	for _, d := range claude.Allow {
		if d.Spec == "api.anthropic.com:443" {
			found = true
		}
	}
	if !found {
		t.Error("claude no longer allows api.anthropic.com:443, which a real run needed")
	}
	if rules := claude.AllowRules(); len(rules) != len(claude.Allow) || rules[0].Action != policy.Allow {
		t.Errorf("AllowRules did not render the allowlist as allow rules: %+v", rules)
	}
}

// Every run began with three lines of tini warning that reads like a fault: tini is not PID 1
// inside a microVM — the guest's own init is — so without -s it registers as no kind of
// reaper and orphans go past it. The fix is to make the reaping work, not to hide the notice,
// so the flag has to stay in the prefix and in the images' ENTRYPOINT.
func TestTiniRegistersAsASubreaper(t *testing.T) {
	if !slices.Contains(initArgv, "-s") {
		t.Errorf("init prefix %v does not register tini as a subreaper", initArgv)
	}
	if initArgv[0] != "/usr/bin/tini" || initArgv[len(initArgv)-2] != "--" {
		t.Errorf("init prefix %v is no longer tini with a separator before the entrypoint", initArgv)
	}
}

// A definition that cannot produce a rule is caught when it is registered, not when someone
// tries to run the agent — where the only choices left are refusing to run it or dropping the
// rule, and dropping it would leave a policy with a hole nothing announced.
func TestAnUnparseableAllowlistEntryIsRejected(t *testing.T) {
	r := &Registry{}
	err := r.Add(Agent{Name: "bad", Allow: []Destination{{Spec: "*.*.example.com", Why: "nonsense"}}})
	if err == nil {
		t.Fatal("an unparseable allowlist entry was registered")
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("error %q does not name the offending destination", err)
	}
}

// Kiro is registered without an image. That has to read as "the name is right, the
// environment is missing", which is a different answer from "no such agent".
func TestAnAgentWithoutAnImageExplainsItself(t *testing.T) {
	kiro, ok := Builtin().Lookup("kiro")
	if !ok {
		t.Fatal("kiro is not registered")
	}
	if kiro.Runnable() {
		t.Fatal("kiro was reported as runnable; update this test if an image now exists")
	}
	err := RequireRunnable(kiro)
	if err == nil {
		t.Fatal("an agent with no image was reported as runnable")
	}
	if !strings.Contains(err.Error(), "-template") {
		t.Errorf("error = %q, want it to say how to supply an image", err)
	}
}

// -template puts the agent in an image Boks knows nothing about, where the paths in Init do
// not exist. Bare is what keeps the command runnable there.
func TestBareDropsTheInitPrefix(t *testing.T) {
	claude, _ := Builtin().Lookup("claude")
	// Read the command off the agent rather than repeating it. This test hardcoded
	// []string{"claude"} and went red the day the definition gained
	// --dangerously-skip-permissions: it was asserting the flag's absence, which was never
	// the point. What it is for is the init PREFIX, so only the prefix is spelled out here.
	want := append(slices.Clone(initArgv), claude.Command...)
	if got := claude.Argv(nil); !slices.Equal(got, want) {
		t.Errorf("Argv = %v, want %v — the init prefix in front of the command", got, want)
	}
	if got := claude.Bare().Argv(nil); !slices.Equal(got, claude.Command) {
		t.Errorf("Bare().Argv = %v, want just the command %v", got, claude.Command)
	}
	// The assertion above compares two things derived from the same agent, so it would hold
	// even if Argv returned the command with no prefix at all. This is what rules that out.
	if len(initArgv) == 0 {
		t.Fatal("initArgv is empty, so the test above compares nothing")
	}
	if got := claude.Argv(nil); len(got) != len(initArgv)+len(claude.Command) {
		t.Errorf("Argv = %v, want %d elements", got, len(initArgv)+len(claude.Command))
	}
}

func TestResolveUnknownAgentListsTheKnownOnes(t *testing.T) {
	_, err := Builtin().Resolve("cladue")
	if err == nil {
		t.Fatal("an unknown agent was accepted")
	}
	for _, want := range []string{"cladue", "claude", "shell"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// What `--` means depends on the agent: arguments to an agent that has its own command,
// and the command itself for a shell.
func TestArgv(t *testing.T) {
	tests := []struct {
		name  string
		agent Agent
		extra []string
		want  []string
	}{
		{"no extra", Agent{Command: []string{"claude"}}, nil, []string{"claude"}},
		{"appended", Agent{Command: []string{"claude"}, Args: ArgsAppend}, []string{"--continue"},
			[]string{"claude", "--continue"}},
		{"a shell's arguments are the command", Agent{Command: []string{"/bin/sh"}, Args: ArgsCommand},
			[]string{"uname", "-a"}, []string{"uname", "-a"}},
		{"no command of its own", Agent{Args: ArgsAppend}, []string{"ls"}, []string{"ls"}},
		{"image default", Agent{}, nil, nil},
		{"init comes first", Agent{Init: []string{"tini", "--"}, Command: []string{"claude"}}, nil,
			[]string{"tini", "--", "claude"}},
		{"init survives appended arguments",
			Agent{Init: []string{"tini", "--"}, Command: []string{"claude"}, Args: ArgsAppend},
			[]string{"--continue"}, []string{"tini", "--", "claude", "--continue"}},
		{"init survives a replaced command",
			Agent{Init: []string{"tini", "--"}, Command: []string{"/bin/bash"}, Args: ArgsCommand},
			[]string{"uname", "-a"}, []string{"tini", "--", "uname", "-a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.agent.Argv(tt.extra); !slices.Equal(got, tt.want) {
				t.Errorf("Argv(%v) = %v, want %v", tt.extra, got, tt.want)
			}
		})
	}
}

// Add is the seam a user-defined agent will arrive through, so it has to behave like one
// even before a loader exists: register, override, and reject a name that could not become
// a sandbox name.
func TestAddUserDefinedAgent(t *testing.T) {
	r := Builtin()
	custom := Agent{Name: "udi-copilot-yolo", Image: "example.test/copilot:latest", Command: []string{"copilot"}}
	if err := r.Add(custom); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := r.Lookup("udi-copilot-yolo")
	if !ok || got.Image != custom.Image {
		t.Fatalf("looked up %+v, want the added agent", got)
	}
	if got.Args != ArgsAppend {
		t.Errorf("args mode = %q, want the default %q", got.Args, ArgsAppend)
	}
	if !r.Known("udi-copilot-yolo") {
		t.Error("an added agent is not recognised as a first positional")
	}

	// Overriding a built-in is how a user replaces an environment Boks ships.
	before := len(r.All())
	if err := r.Add(Agent{Name: "shell", Image: "example.test/shell:latest"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(r.All()) != before {
		t.Errorf("overriding an agent changed the count from %d to %d", before, len(r.All()))
	}
	if shell, _ := r.Lookup("shell"); shell.Image != "example.test/shell:latest" {
		t.Errorf("shell image = %q, want the override", shell.Image)
	}

	for _, bad := range []string{"", "has space", "slash/name", "-leading", strings.Repeat("a", 40)} {
		if err := r.Add(Agent{Name: bad}); err == nil {
			t.Errorf("Add accepted the name %q", bad)
		}
	}
}
