package policy

import (
	"strings"
	"testing"
)

// storeWith builds a store in memory for the precedence tests. Nothing is written to disk:
// these tests are about the layering rule, not about the file.
func storeWith(t *testing.T, preset string, global, sandbox []RuleSpec) *Store {
	t.Helper()
	s := NewStore("")
	s.Preset = preset
	s.Global = global
	if len(sandbox) > 0 {
		s.Sandboxes = map[string][]RuleSpec{"web": sandbox}
	}
	return s
}

func allowRule(spec string) RuleSpec { return RuleSpec{Action: Allow, Spec: spec} }
func denyRule(spec string) RuleSpec  { return RuleSpec{Action: Deny, Spec: spec} }

// TestScopePrecedence is the matrix. Every combination of where an allow and a deny can be
// written, and what the result must be.
//
// One rule governs the whole table: **a deny in any scope beats an allow in any scope.**
// There is no specificity, no "closer scope wins", and no way for a narrower scope to unsay
// a wider one. That is what makes a per-sandbox rule safe to hand to a user — it can add
// access the machine's policy already tolerates, and it can subtract, but it can never widen
// past a prohibition someone wrote down.
func TestScopePrecedence(t *testing.T) {
	const host = "example.com"
	cases := []struct {
		name    string
		req     func(t *testing.T) Request
		allowed bool
		scope   string
	}{
		{
			name: "global allow reaches every sandbox",
			req: func(t *testing.T) Request {
				return Request{Store: storeWith(t, PresetLocked, []RuleSpec{allowRule(host + ":443")}, nil), Sandbox: "web"}
			},
			allowed: true, scope: "global",
		},
		{
			name: "sandbox allow applies to that sandbox",
			req: func(t *testing.T) Request {
				return Request{Store: storeWith(t, PresetLocked, nil, []RuleSpec{allowRule(host + ":443")}), Sandbox: "web"}
			},
			allowed: true, scope: "sandbox web",
		},
		{
			name: "sandbox allow does not reach another sandbox",
			req: func(t *testing.T) Request {
				return Request{Store: storeWith(t, PresetLocked, nil, []RuleSpec{allowRule(host + ":443")}), Sandbox: "other"}
			},
			allowed: false, scope: "default",
		},
		{
			name: "global deny beats a sandbox allow",
			req: func(t *testing.T) Request {
				return Request{
					Store:   storeWith(t, PresetLocked, []RuleSpec{denyRule(host)}, []RuleSpec{allowRule(host + ":443")}),
					Sandbox: "web",
				}
			},
			allowed: false, scope: "global",
		},
		{
			name: "sandbox deny beats a global allow",
			req: func(t *testing.T) Request {
				return Request{
					Store:   storeWith(t, PresetLocked, []RuleSpec{allowRule(host + ":443")}, []RuleSpec{denyRule(host)}),
					Sandbox: "web",
				}
			},
			allowed: false, scope: "sandbox web",
		},
		{
			name: "a sandbox catch-all allow still cannot defeat a global deny",
			req: func(t *testing.T) Request {
				return Request{
					Store:   storeWith(t, PresetLocked, []RuleSpec{denyRule(host)}, []RuleSpec{allowRule("*")}),
					Sandbox: "web",
				}
			},
			allowed: false, scope: "global",
		},
		{
			name: "a per-run allow cannot defeat a global deny",
			req: func(t *testing.T) Request {
				return Request{
					Store:   storeWith(t, PresetLocked, []RuleSpec{denyRule(host)}, nil),
					Sandbox: "web",
					Allow:   []string{host + ":443"},
				}
			},
			allowed: false, scope: "global",
		},
		{
			name: "a per-run allow cannot defeat a sandbox deny",
			req: func(t *testing.T) Request {
				return Request{
					Store:   storeWith(t, PresetLocked, nil, []RuleSpec{denyRule(host)}),
					Sandbox: "web",
					Allow:   []string{host + ":443"},
				}
			},
			allowed: false, scope: "sandbox web",
		},
		{
			name: "a per-run deny beats a stored allow",
			req: func(t *testing.T) Request {
				return Request{
					Store:   storeWith(t, PresetLocked, []RuleSpec{allowRule(host + ":443")}, nil),
					Sandbox: "web",
					Deny:    []string{host},
				}
			},
			allowed: false, scope: scopeFlagDeny,
		},
		{
			name: "a per-run allow works when nothing denies it",
			req: func(t *testing.T) Request {
				return Request{Store: storeWith(t, PresetLocked, nil, nil), Sandbox: "web", Allow: []string{host + ":443"}}
			},
			allowed: true, scope: scopeFlagAllow,
		},
		{
			name: "--policy open cannot resurrect a destination the store denies",
			req: func(t *testing.T) Request {
				return Request{
					Store:   storeWith(t, PresetLocked, []RuleSpec{denyRule(host)}, nil),
					Sandbox: "web",
					Preset:  PresetOpen,
				}
			},
			allowed: false, scope: "global",
		},
		{
			name: "--policy open does widen what nothing denies",
			req: func(t *testing.T) Request {
				return Request{Store: storeWith(t, PresetLocked, nil, nil), Sandbox: "web", Preset: PresetOpen}
			},
			allowed: true, scope: "default",
		},
		{
			name: "the stored preset is the base when nothing overrides it",
			req: func(t *testing.T) Request {
				return Request{Store: storeWith(t, PresetOpen, nil, nil), Sandbox: "web"}
			},
			allowed: true, scope: "default",
		},
		{
			name: "a profile's rules apply when it is selected",
			req: func(t *testing.T) Request {
				s := storeWith(t, PresetLocked, nil, nil)
				if err := s.AddProfile("ci", Profile{Preset: PresetLocked, Rules: []RuleSpec{allowRule(host + ":443")}}); err != nil {
					t.Fatal(err)
				}
				return Request{Store: s, Sandbox: "web", Profile: "ci"}
			},
			allowed: true, scope: "profile ci",
		},
		{
			name: "a profile cannot resurrect a globally denied destination",
			req: func(t *testing.T) Request {
				s := storeWith(t, PresetLocked, []RuleSpec{denyRule(host)}, nil)
				if err := s.AddProfile("ci", Profile{Preset: PresetOpen, Rules: []RuleSpec{allowRule(host + ":443")}}); err != nil {
					t.Fatal(err)
				}
				return Request{Store: s, Sandbox: "web", Profile: "ci"}
			},
			allowed: false, scope: "global",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.req(t).Resolve()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			p, err := res.Policy()
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			v := p.Evaluate(target(t, host, 443))
			if v.Allowed != tc.allowed {
				t.Fatalf("allowed=%v, want %v (%s)", v.Allowed, tc.allowed, v.Reason)
			}
			if v.Scope != tc.scope {
				t.Errorf("decided by scope %q, want %q (%s)", v.Scope, tc.scope, v.Reason)
			}
		})
	}
}

// TestDenyWinsWhicheverScopeItIsIn walks every ordered pair of scopes and asserts the
// invariant directly: with an allow in one and a deny in the other, the answer is deny —
// both ways round, so the result cannot depend on the order the layers happen to be
// assembled in.
func TestDenyWinsWhicheverScopeItIsIn(t *testing.T) {
	const host = "pair.test"
	type placer func(s *Store, req *Request, r RuleSpec)

	scopes := map[string]placer{
		"global": func(s *Store, _ *Request, r RuleSpec) { s.Global = append(s.Global, r) },
		"sandbox": func(s *Store, _ *Request, r RuleSpec) {
			if s.Sandboxes == nil {
				s.Sandboxes = map[string][]RuleSpec{}
			}
			s.Sandboxes["web"] = append(s.Sandboxes["web"], r)
		},
		"profile": func(s *Store, req *Request, r RuleSpec) {
			p := s.Profiles["ci"]
			p.Rules = append(p.Rules, r)
			s.Profiles["ci"] = p
			req.Profile = "ci"
		},
		"flag": func(_ *Store, req *Request, r RuleSpec) {
			if r.Action == Deny {
				req.Deny = append(req.Deny, r.Spec)
				return
			}
			req.Allow = append(req.Allow, r.Spec)
		},
	}

	for allowIn := range scopes {
		for denyIn := range scopes {
			if allowIn == denyIn {
				continue
			}
			t.Run("allow in "+allowIn+", deny in "+denyIn, func(t *testing.T) {
				s := NewStore("")
				s.Preset = PresetLocked
				s.Profiles = map[string]Profile{"ci": {Preset: PresetLocked}}
				req := Request{Store: s, Sandbox: "web"}
				scopes[allowIn](s, &req, allowRule(host+":443"))
				scopes[denyIn](s, &req, denyRule(host))

				res, err := req.Resolve()
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				p, err := res.Policy()
				if err != nil {
					t.Fatal(err)
				}
				if v := p.Evaluate(target(t, host, 443)); v.Allowed {
					t.Fatalf("an allow in %s defeated a deny in %s: %s", allowIn, denyIn, v.Reason)
				}
			})
		}
	}
}

// agentRequest is a run of the claude-shaped agent: one destination its definition says it
// cannot work without, on a machine whose stored policy is whatever the test sets up.
func agentRequest(s *Store, host string) Request {
	return Request{
		Store:      s,
		Sandbox:    "web",
		Agent:      "claude",
		AgentAllow: []RuleSpec{{Action: Allow, Spec: host + ":443", Note: "the agent's own API"}},
	}
}

// TestAgentAllowlistIsItsOwnLayer: an agent's own destinations are permitted, and the verdict
// names the agent rather than a preset — because "why can this sandbox reach that?" has to be
// answerable, and "an agent I ran needs it" is a different answer from "I allowed it".
func TestAgentAllowlistIsItsOwnLayer(t *testing.T) {
	const host = "api.agent.test"
	res, err := agentRequest(storeWith(t, PresetStandard, nil, nil), host).Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	p, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	v := p.Evaluate(target(t, host, 443))
	if !v.Allowed {
		t.Fatalf("the agent's own destination was denied: %s", v.Reason)
	}
	if v.Scope != "agent claude" {
		t.Errorf("scope = %q, want %q", v.Scope, "agent claude")
	}
	// It has to be visible where a user looks for it, labelled and with its reason, or a
	// default allowlist is just a hole nobody can audit.
	out := res.Describe()
	for _, want := range []string{"agent claude", host + ":443", "the agent's own API"} {
		if !strings.Contains(out, want) {
			t.Errorf("describe output is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(res.Name, "agent:claude") {
		t.Errorf("policy name %q does not say the agent layer applied", res.Name)
	}
}

// TestDenyBeatsAnAgentAllow is the property that makes a per-agent default safe to ship: the
// agent layer is an allow like any other, so a deny written anywhere defeats it. If this ever
// fails, an agent definition in this repository can overrule a user's own prohibition.
func TestDenyBeatsAnAgentAllow(t *testing.T) {
	const host = "api.agent.test"
	cases := map[string]func(s *Store, req *Request){
		"global": func(s *Store, _ *Request) { s.Global = append(s.Global, denyRule(host)) },
		"sandbox": func(s *Store, _ *Request) {
			s.Sandboxes = map[string][]RuleSpec{"web": {denyRule(host)}}
		},
		"profile": func(s *Store, req *Request) {
			if err := s.AddProfile("ci", Profile{Preset: PresetStandard, Rules: []RuleSpec{denyRule(host)}}); err != nil {
				t.Fatal(err)
			}
			req.Profile = "ci"
		},
		"flag --deny": func(_ *Store, req *Request) { req.Deny = append(req.Deny, host) },
		"port-only deny": func(s *Store, _ *Request) {
			// A deny that does not name the host at all still wins, because the
			// engine tests every deny before any allow rather than the most specific.
			s.Global = append(s.Global, denyRule("*:443"))
		},
	}
	for name, place := range cases {
		t.Run("deny in "+name, func(t *testing.T) {
			s := storeWith(t, PresetStandard, nil, nil)
			req := agentRequest(s, host)
			place(s, &req)

			res, err := req.Resolve()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			p, err := res.Policy()
			if err != nil {
				t.Fatal(err)
			}
			if v := p.Evaluate(target(t, host, 443)); v.Allowed {
				t.Fatalf("an agent's own allowlist defeated a deny in %s: %s", name, v.Reason)
			}
		})
	}
}

// TestLockedDropsTheAgentLayer: "deny everything; every destination must be added with
// --allow" has to stay true of the locked preset, or the one posture that promises nothing
// gets out would quietly let an agent's own API out — the exact channel someone choosing
// locked is trying to close.
func TestLockedDropsTheAgentLayer(t *testing.T) {
	const host = "api.agent.test"
	res, err := agentRequest(storeWith(t, PresetLocked, nil, nil), host).Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	p, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Evaluate(target(t, host, 443)); v.Allowed {
		t.Fatalf("locked let the agent layer through: %s", v.Reason)
	}
	// Silently dropping it would be its own kind of confusing, so the layer is still
	// listed, with the reason it contributed nothing.
	out := res.Describe()
	if !strings.Contains(out, "agent claude") || !strings.Contains(out, "not applied") {
		t.Errorf("the dropped agent layer is not accounted for:\n%s", out)
	}
	if strings.Contains(res.Name, "agent:claude") {
		t.Errorf("policy name %q claims an agent layer that did not apply", res.Name)
	}
}

// TestAgentLayerCannotChangeTheDefault: an agent contributes allows and nothing else. It
// cannot make a deny-by-default machine allow-by-default, whatever its definition says.
func TestAgentLayerCannotChangeTheDefault(t *testing.T) {
	req := agentRequest(storeWith(t, PresetStandard, nil, nil), "api.agent.test")
	req.AgentAllow = append(req.AgentAllow, RuleSpec{Action: Allow, Spec: "*"})
	res, err := req.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if res.Default != Deny {
		t.Errorf("default became %s; only the base preset may decide it", res.Default)
	}
	// A catch-all in an agent definition would be a bug in this repository, but even then
	// it must not defeat a deny — the same guarantee every other allow gets.
	req.Deny = []string{"blocked.test"}
	res, err = req.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	p, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Evaluate(target(t, "blocked.test", 443)); v.Allowed {
		t.Errorf("a catch-all agent allow defeated a --deny: %s", v.Reason)
	}
}

// TestNarrowerScopesCannotChangeTheDefault: the default action comes from the base and from
// nowhere else, so no stored rule can turn a deny-by-default machine into an allow-by-default
// one without someone changing the posture explicitly.
func TestNarrowerScopesCannotChangeTheDefault(t *testing.T) {
	s := storeWith(t, PresetLocked, []RuleSpec{allowRule("*")}, []RuleSpec{allowRule("*")})
	res, err := (Request{Store: s, Sandbox: "web", Allow: []string{"*"}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if res.Default != Deny {
		t.Errorf("default became %s; only the base preset may decide it", res.Default)
	}
}

func TestResolveRejectsWhatItCannotEnforce(t *testing.T) {
	s := NewStore("")
	if _, err := (Request{Store: s, Profile: "ghost"}).Resolve(); err == nil {
		t.Error("an unknown profile was accepted")
	}
	if _, err := (Request{Store: s, Preset: "paranoid"}).Resolve(); err == nil {
		t.Error("an unknown preset was accepted")
	}
	if _, err := (Request{Store: s, Allow: []string{"*.*.bad"}}).Resolve(); err == nil {
		t.Error("an unusable --allow was accepted")
	}
	if _, err := (Request{Store: s, Deny: []string{"[::1"}}).Resolve(); err == nil {
		t.Error("an unusable --deny was accepted")
	}
}

// TestResolveWithoutAStore is the path every command takes before anything is initialised.
func TestResolveWithoutAStore(t *testing.T) {
	res, err := (Request{}).Resolve()
	if err != nil {
		t.Fatalf("resolve with no store: %v", err)
	}
	if res.Name != DefaultPreset || res.Default != Deny {
		t.Errorf("resolved to %s/%s, want %s/deny", res.Name, res.Default, DefaultPreset)
	}
}

// TestDescribeNamesTheScopeOfEveryRule: `policy ls` has to make precedence unambiguous, and
// the only way to do that is to say where each rule came from.
func TestDescribeNamesTheScopeOfEveryRule(t *testing.T) {
	s := storeWith(t, PresetLocked, []RuleSpec{denyRule("bad.test")}, []RuleSpec{allowRule("good.test:443")})
	res, err := (Request{Store: s, Sandbox: "web", Allow: []string{"flagged.test:443"}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	out := res.Describe()
	for _, want := range []string{
		"deny (always wins):",
		"bad.test",
		"global",
		"good.test:443",
		"sandbox web",
		"flagged.test:443",
		scopeFlagAllow,
		"layers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("describe output is missing %q:\n%s", want, out)
		}
	}
}

func TestSandboxPolicyRecord(t *testing.T) {
	var none *SandboxPolicy
	if !none.IsZero() || none.String() != "the default policy" {
		t.Error("a nil record should read as the default policy")
	}
	rec := &SandboxPolicy{V: SandboxPolicyVersion, Preset: PresetLocked, Allow: []string{"a.test:443"}}
	if rec.IsZero() {
		t.Error("a record with a preset is not empty")
	}
	if got := rec.String(); got != "--policy locked --allow a.test:443" {
		t.Errorf("record renders as %q", got)
	}
	req := rec.Request(NewStore(""), "web")
	if req.Preset != PresetLocked || req.Sandbox != "web" || len(req.Allow) != 1 {
		t.Errorf("record did not rebuild its request: %+v", req)
	}
}
