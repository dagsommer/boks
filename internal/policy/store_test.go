package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "policy", "policy.json"))
}

func TestStoreRoundTrip(t *testing.T) {
	s := tempStore(t)
	s.Preset = PresetLocked
	if _, err := s.Add(GlobalScope(), RuleSpec{Action: Allow, Spec: "github.com:443", Note: "git"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.Add(SandboxScope("web"), RuleSpec{Action: Deny, Spec: "evil.test"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.AddProfile("ci", Profile{Preset: PresetLocked, Description: "builds"}); err != nil {
		t.Fatalf("add profile: %v", err)
	}
	if _, err := s.Add(ProfileScope("ci"), RuleSpec{Action: Allow, Spec: "proxy.golang.org:443"}); err != nil {
		t.Fatalf("add profile rule: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := LoadStore(s.Path())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Version != StoreVersion || got.Preset != PresetLocked {
		t.Errorf("version/preset lost: %+v", got)
	}
	if len(got.Global) != 1 || got.Global[0].Action != Allow || got.Global[0].Note != "git" {
		t.Errorf("global rules lost: %+v", got.Global)
	}
	if rules, ok := got.Rules(SandboxScope("web")); !ok || len(rules) != 1 || rules[0].Action != Deny {
		t.Errorf("sandbox rules lost: %+v", rules)
	}
	if p := got.Profiles["ci"]; p.Preset != PresetLocked || len(p.Rules) != 1 {
		t.Errorf("profile lost: %+v", p)
	}
	if !got.Exists() {
		t.Error("a saved store should report that it exists")
	}
}

// TestStoreIsWrittenAsText: the store is meant to be readable and hand-editable, so the
// action must be a word rather than the integer the type happens to be.
func TestStoreIsWrittenAsText(t *testing.T) {
	s := tempStore(t)
	if _, err := s.Add(GlobalScope(), RuleSpec{Action: Deny, Spec: "metadata.test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version": 1`, `"action": "deny"`, `"spec": "metadata.test"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("stored file does not contain %s:\n%s", want, data)
		}
	}
}

// TestStorePermissions: the store records which destinations this machine permits, and a
// policy other users can rewrite is not a policy.
func TestStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	s := tempStore(t)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("policy store is %v; it must not be readable or writable by other users", perm)
	}
	dir, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("policy directory is %v; it must be owner-only", perm)
	}
}

// TestMissingStoreIsNotAnError: a machine where nobody has written a rule is the
// uninitialised state, not a broken one — and it must still be deny-by-default.
func TestMissingStoreIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nothing", "policy.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatalf("a missing store must load as empty: %v", err)
	}
	if s.Exists() || s.Count() != 0 {
		t.Errorf("a missing store should be empty: %+v", s)
	}
	res, err := (Request{Store: s}).Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Default != Deny {
		t.Errorf("a machine with no stored policy must still deny by default, got %s", res.Default)
	}
	p, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Evaluate(target(t, "unlisted.test", 443)); v.Allowed {
		t.Errorf("no stored policy allowed %s: %s", "unlisted.test", v.Reason)
	}
}

// TestCorruptStoreFailsClosed is the property that matters most about this file: every way
// of being unreadable must produce an error that stops the caller, never a policy that
// quietly permits more than the one on disk.
func TestCorruptStoreFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantIn  string
	}{
		{"truncated json", `{"version": 1, "global": [`, "not valid"},
		{"not json at all", "deny evil.test\n", "not valid"},
		{"no version", `{"global": []}`, "no version"},
		{"future version", `{"version": 99}`, "version 99"},
		{"unknown field", `{"version": 1, "globl": []}`, "not valid"},
		{"unknown preset", `{"version": 1, "preset": "paranoid"}`, "unknown network policy"},
		{"unknown action", `{"version": 1, "global": [{"action": "maybe", "spec": "a.test"}]}`, "unknown action"},
		{"unusable rule", `{"version": 1, "global": [{"action": "deny", "spec": "*.*.evil.test"}]}`, "unusable rule"},
		{"unusable sandbox rule", `{"version": 1, "sandboxes": {"web": [{"action": "deny", "spec": "a:b:c"}]}}`, "unusable rule"},
		{"unusable profile rule", `{"version": 1, "profiles": {"ci": {"rules": [{"action": "allow", "spec": ""}]}}}`, "unusable rule"},
		{"nameless sandbox scope", `{"version": 1, "sandboxes": {"": []}}`, "no name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := LoadStore(path)
			if err == nil {
				t.Fatalf("a corrupt store was accepted and resolved to %+v", s)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should mention %q", err, tc.wantIn)
			}
			if s != nil {
				t.Error("a corrupt store must not also return a usable store")
			}
		})
	}
}

// TestUnreadableStoreFailsClosed covers the other way a store can be unavailable: it is
// there, and this user cannot read it. Falling back to the defaults would be the wrong
// direction to fail in.
func TestUnreadableStoreFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(path); err == nil {
		t.Fatal("an unreadable store was accepted")
	}
}

func TestAddIsIdempotentAndCaseFolding(t *testing.T) {
	s := tempStore(t)
	added, err := s.Add(GlobalScope(), RuleSpec{Action: Allow, Spec: "GitHub.com:443"})
	if err != nil || !added {
		t.Fatalf("first add: %v %v", added, err)
	}
	added, err = s.Add(GlobalScope(), RuleSpec{Action: Allow, Spec: "github.com:443"})
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Error("the same destination was stored twice under a different spelling")
	}
	// The same destination with the other disposition is a different rule, and both are
	// kept: the deny is what will win, and the allow being visible is how a user sees why.
	if added, err = s.Add(GlobalScope(), RuleSpec{Action: Deny, Spec: "github.com:443"}); err != nil || !added {
		t.Fatalf("deny of an allowed destination: %v %v", added, err)
	}
	if len(s.Global) != 2 {
		t.Errorf("expected an allow and a deny, got %+v", s.Global)
	}
}

func TestAddRejectsAnUnusableRule(t *testing.T) {
	s := tempStore(t)
	if _, err := s.Add(GlobalScope(), RuleSpec{Action: Allow, Spec: "*.*.example.com"}); err == nil {
		t.Fatal("an unusable rule was stored")
	}
	if _, err := s.Add(ProfileScope("nope"), RuleSpec{Action: Allow, Spec: "a.test"}); err == nil {
		t.Fatal("a rule was stored in a profile that does not exist")
	}
}

func TestRemove(t *testing.T) {
	s := tempStore(t)
	mustAdd(t, s, GlobalScope(), RuleSpec{Action: Allow, Spec: "a.test:443"})
	mustAdd(t, s, GlobalScope(), RuleSpec{Action: Deny, Spec: "a.test:443"})
	mustAdd(t, s, GlobalScope(), RuleSpec{Action: Allow, Spec: "b.test:443"})

	allow := Allow
	removed, err := s.Remove(GlobalScope(), &allow, "a.test:443")
	if err != nil || len(removed) != 1 || removed[0].Action != Allow {
		t.Fatalf("removing one disposition: %v %+v", err, removed)
	}
	if len(s.Global) != 2 {
		t.Fatalf("expected two rules left, got %+v", s.Global)
	}
	if removed, err = s.Remove(GlobalScope(), nil, "a.test:443"); err != nil || len(removed) != 1 {
		t.Fatalf("removing without an action: %v %+v", err, removed)
	}
	if _, err := s.Remove(GlobalScope(), nil, "a.test:443"); err == nil {
		t.Error("removing a rule that is not there should say so")
	}
	if _, err := s.Remove(SandboxScope("ghost"), nil, "a.test:443"); err == nil {
		t.Error("removing from a scope that does not exist should say so")
	}
}

// TestRemovingTheLastSandboxRuleDropsTheScope keeps the file honest: a sandbox with no rules
// should not linger in it as an empty object that looks like configuration.
func TestRemovingTheLastSandboxRuleDropsTheScope(t *testing.T) {
	s := tempStore(t)
	mustAdd(t, s, SandboxScope("web"), RuleSpec{Action: Deny, Spec: "a.test"})
	if _, err := s.Remove(SandboxScope("web"), nil, "a.test"); err != nil {
		t.Fatal(err)
	}
	if len(s.SandboxNames()) != 0 {
		t.Errorf("empty sandbox scope survived: %v", s.SandboxNames())
	}
}

func TestProfiles(t *testing.T) {
	s := tempStore(t)
	if err := s.AddProfile("ci", Profile{Preset: PresetLocked}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProfile("ci", Profile{}); err == nil {
		t.Error("a profile was silently overwritten")
	}
	if err := s.AddProfile("bad", Profile{Preset: "nonesuch"}); err == nil {
		t.Error("a profile with an unknown preset was accepted")
	}
	if err := s.AddProfile("", Profile{}); err == nil {
		t.Error("a nameless profile was accepted")
	}
	if err := s.RemoveProfile("ci"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveProfile("ci"); err == nil {
		t.Error("removing a profile twice should say so")
	}
}

func TestResetScopes(t *testing.T) {
	s := tempStore(t)
	s.Preset = PresetOpen
	mustAdd(t, s, GlobalScope(), RuleSpec{Action: Deny, Spec: "a.test"})
	mustAdd(t, s, SandboxScope("web"), RuleSpec{Action: Deny, Spec: "b.test"})
	if err := s.AddProfile("ci", Profile{}); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, ProfileScope("ci"), RuleSpec{Action: Allow, Spec: "c.test"})

	if n := s.Reset(SandboxScope("web"), false); n != 1 {
		t.Errorf("resetting one sandbox destroyed %d rules, want 1", n)
	}
	if s.Count() != 2 {
		t.Errorf("resetting one scope touched another: %d rules left", s.Count())
	}
	if n := s.Reset(GlobalScope(), true); n != 2 {
		t.Errorf("full reset destroyed %d rules, want 2", n)
	}
	if s.Count() != 0 || len(s.ProfileNames()) != 0 || s.Preset != DefaultPreset {
		t.Errorf("a full reset must restore the defaults: %+v", s)
	}
}

func TestParseScope(t *testing.T) {
	if _, err := ParseScope("web", "ci"); err == nil {
		t.Error("-sandbox and -profile together should be refused")
	}
	if s, _ := ParseScope("", ""); s.Kind != ScopeGlobal || s.String() != "global" {
		t.Errorf("default scope is %v", s)
	}
	if s, _ := ParseScope("web", ""); s.String() != "sandbox web" {
		t.Errorf("sandbox scope is %v", s)
	}
	if s, _ := ParseScope("", "ci"); s.String() != "profile ci" {
		t.Errorf("profile scope is %v", s)
	}
}

func mustAdd(t *testing.T, s *Store, scope ScopeRef, r RuleSpec) {
	t.Helper()
	if _, err := s.Add(scope, r); err != nil {
		t.Fatalf("add %v to %s: %v", r, scope, err)
	}
}

func target(t *testing.T, host string, port int) Target {
	t.Helper()
	tg, err := NewTarget(host, port)
	if err != nil {
		t.Fatalf("target %s:%d: %v", host, port, err)
	}
	return tg
}
