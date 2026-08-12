package policy

// Resolution: turning a base posture, stored rules and per-run flags into one policy.
//
// # The layers, and what each is for
//
//	1. base       a preset, or a stored profile — the default action and its rules
//	2. profile    the selected profile's own rules
//	3. global     stored rules that apply to every sandbox on this machine
//	4. sandbox    stored rules for this sandbox only
//	5. flags      -allow / -deny on this invocation (a Boks addition, not sbx parity)
//
// # Precedence, and why it is this way
//
// Only **one** thing about a layer's position matters: the base decides the default action.
// Everything else is concatenation, because the decision function already has a precedence
// rule that does not care about order — **every deny is tested before any allow** — and
// layering a second, order-dependent rule on top of it would produce a policy nobody can
// predict from reading it.
//
// So:
//
//   - **A deny in any layer beats an allow in any layer.** A sandbox-scoped allow cannot
//     unsay a global deny; nor can a per-run `-allow`. This is the property that makes
//     scoping safe to hand to a user: adding a rule to one sandbox can never widen access
//     past what the machine's policy forbids, so the blast radius of a scoped rule is
//     bounded by construction rather than by care.
//   - **Narrower scopes only add.** They cannot remove a rule from a wider one, and they
//     cannot change the default action. Removing a global rule is done where it was written,
//     with `boks policy rm`, which is visible and deliberate.
//   - **The base is replaceable, the rules are not.** `-policy locked` or a profile replaces
//     the preset the default action comes from. That is the one thing a run can override,
//     and it can only ever be observed as a *different set of preset rules*, never as the
//     disappearance of a stored deny.
//
// The asymmetry is deliberate and is the whole design: the thing a run can change is the
// posture it starts from; the thing it cannot change is any prohibition someone wrote down.

import (
	"fmt"
	"sort"
	"strings"
)

// Scope labels used in verdicts and in `boks policy ls`. They are the words a user would
// have to act on, so they name the place a rule is written rather than an internal concept.
const (
	scopeFlagAllow = "flag -allow"
	scopeFlagDeny  = "flag -deny"
	scopeGlobal    = "global"
)

// Layer is one contribution to a resolved policy, kept for display so that `policy ls` can
// show where each rule came from and in what order the layers were applied.
type Layer struct {
	// Source is the scope label its rules carry.
	Source string `json:"source"`
	// Detail is a human note: the preset's description, or why the layer is empty.
	Detail string `json:"detail,omitempty"`
	Count  int    `json:"count"`
}

// Resolution is a policy in a form that survives being written to a pipe.
//
// The supervisor process receives this rather than re-reading the store, so the rules a
// sandbox is running under are fixed at the moment it starts. Editing the store while a
// sandbox runs therefore changes the *next* run, not the one in flight — which is the
// behaviour a user can reason about, and the one that cannot be used to widen a sandbox's
// access from outside it.
type Resolution struct {
	Version int `json:"version"`
	// Name is the policy name that appears in decisions and in the log.
	Name string `json:"name"`
	// Default is the disposition for destinations no rule mentions.
	Default Action `json:"default"`
	// Rules are every layer's rules, each labelled with the scope it came from.
	Rules []RuleSpec `json:"rules,omitempty"`
	// Layers describes how the rule set was assembled, for display only.
	Layers []Layer `json:"layers,omitempty"`
	// Profile is the profile that was selected, if any.
	Profile string `json:"profile,omitempty"`
	// Sandbox is the sandbox the resolution was made for, if any.
	Sandbox string `json:"sandbox,omitempty"`
}

// Policy compiles the resolution into the matchable form the engine takes.
func (r Resolution) Policy() (Policy, error) {
	p := Policy{Name: r.Name, Default: r.Default}
	if p.Name == "" {
		p.Name = DefaultPreset
	}
	p.Rules = make([]Rule, 0, len(r.Rules))
	for _, spec := range r.Rules {
		rule, err := spec.Rule()
		if err != nil {
			return Policy{}, fmt.Errorf("policy %s: rule %q from %s: %w", p.Name, spec.Spec, spec.Scope, err)
		}
		p.Rules = append(p.Rules, rule)
	}
	return p, nil
}

// Request is everything that decides one sandbox's policy.
type Request struct {
	// Store is the durable policy. A nil store means "nothing is stored", which is what a
	// machine with no policy file has and which resolves to the built-in defaults.
	Store *Store
	// Sandbox selects the per-sandbox scope. Empty means no sandbox scope applies, which
	// is the case for `boks policy ls` with no sandbox named.
	Sandbox string
	// Profile selects a stored profile as the base.
	Profile string
	// Preset overrides the base preset (`-policy`). It wins over Profile's preset.
	Preset string
	// Allow and Deny are this run's own rules (`-allow`, `-deny`).
	Allow []string
	Deny  []string
}

// Resolve assembles the effective policy.
//
// Every error it returns is a refusal to produce a policy: an unknown profile, an unknown
// preset, a rule that does not parse. None of them fall back to a default, because a caller
// that received a policy it did not ask for would enforce the wrong one.
func (req Request) Resolve() (Resolution, error) {
	res := Resolution{Version: StoreVersion, Sandbox: req.Sandbox, Profile: req.Profile}

	base, baseName, err := req.base()
	if err != nil {
		return Resolution{}, err
	}
	res.Default = base.Default
	baseScope := "preset " + base.Name
	if req.Profile != "" && req.Preset == "" {
		baseScope = "profile " + req.Profile
	}
	for _, r := range base.Rules {
		res.Rules = append(res.Rules, RuleSpec{Action: r.Action, Spec: r.Spec(), Note: r.Why, Scope: baseScope})
	}
	baseDetail := presetDetail(base)
	if strings.HasPrefix(baseScope, "profile ") {
		baseDetail = "preset " + base.Name + ", plus the profile's own rules"
	}
	res.Layers = append(res.Layers, Layer{Source: baseScope, Detail: baseDetail, Count: len(base.Rules)})
	names := []string{baseName}

	if req.Profile != "" {
		profile, ok := req.profile()
		if !ok {
			return Resolution{}, fmt.Errorf("no policy profile named %q; list them with 'boks policy profile ls'", req.Profile)
		}
		if req.Preset != "" {
			// The flag replaced the profile's preset, so the profile still contributes
			// its rules but no longer its posture. Say so where it is visible.
			res.Rules = appendScoped(res.Rules, profile.Rules, "profile "+req.Profile)
			res.Layers = append(res.Layers, Layer{
				Source: "profile " + req.Profile,
				Detail: "-policy " + req.Preset + " replaced this profile's preset",
				Count:  len(profile.Rules),
			})
			names = append(names, "profile:"+req.Profile)
		}
	}

	if req.Store != nil {
		res.Rules = appendScoped(res.Rules, req.Store.Global, scopeGlobal)
		res.Layers = append(res.Layers, Layer{
			Source: scopeGlobal,
			Detail: "rules for every sandbox on this machine",
			Count:  len(req.Store.Global),
		})
		if len(req.Store.Global) > 0 {
			names = append(names, scopeGlobal)
		}
		if req.Sandbox != "" {
			scoped, _ := req.Store.Rules(SandboxScope(req.Sandbox))
			res.Rules = appendScoped(res.Rules, scoped, "sandbox "+req.Sandbox)
			res.Layers = append(res.Layers, Layer{
				Source: "sandbox " + req.Sandbox,
				Detail: "rules for this sandbox only",
				Count:  len(scoped),
			})
			if len(scoped) > 0 {
				names = append(names, "sandbox:"+req.Sandbox)
			}
		}
	}

	// Flags last, and denies before allows within them, for the same reason the engine
	// tests denies first: the rule that wins should be the one a reader expects.
	flagRules, err := parseFlagRules(req.Allow, req.Deny)
	if err != nil {
		return Resolution{}, err
	}
	res.Rules = append(res.Rules, flagRules...)
	res.Layers = append(res.Layers, Layer{
		Source: "flags",
		Detail: "-allow/-deny on this run; a Boks addition, not carried by the sandbox",
		Count:  len(flagRules),
	})
	if len(flagRules) > 0 {
		names = append(names, "local")
	}

	res.Name = strings.Join(names, "+")
	return res, nil
}

// base picks the preset the default action comes from.
func (req Request) base() (Policy, string, error) {
	switch {
	case req.Preset != "":
		p, err := Preset(req.Preset)
		if err != nil {
			return Policy{}, "", err
		}
		return p, p.Name, nil
	case req.Profile != "":
		profile, ok := req.profile()
		if !ok {
			return Policy{}, "", fmt.Errorf("no policy profile named %q; list them with 'boks policy profile ls'", req.Profile)
		}
		p, err := Preset(profile.Preset)
		if err != nil {
			return Policy{}, "", fmt.Errorf("profile %q: %w", req.Profile, err)
		}
		// The profile's own rules are part of its base, so that selecting a profile is
		// selecting one policy rather than a preset with a footnote.
		for _, r := range profile.Rules {
			rule, err := r.Rule()
			if err != nil {
				return Policy{}, "", fmt.Errorf("profile %q: %w", req.Profile, err)
			}
			p.Rules = append(p.Rules, rule)
		}
		return p, "profile:" + req.Profile, nil
	}
	name := DefaultPreset
	if req.Store != nil && req.Store.Preset != "" {
		name = req.Store.Preset
	}
	p, err := Preset(name)
	if err != nil {
		return Policy{}, "", err
	}
	return p, p.Name, nil
}

func (req Request) profile() (Profile, bool) {
	if req.Store == nil {
		return Profile{}, false
	}
	p, ok := req.Store.Profiles[req.Profile]
	return p, ok
}

func presetDetail(p Policy) string {
	if d := PresetDescription(p.Name); d != "" {
		return d
	}
	return "default: " + p.Default.String()
}

func appendScoped(dst []RuleSpec, rules []RuleSpec, scope string) []RuleSpec {
	for _, r := range rules {
		r.Scope = scope
		dst = append(dst, r)
	}
	return dst
}

// parseFlagRules turns the per-run flags into rules, denies first.
func parseFlagRules(allow, deny []string) ([]RuleSpec, error) {
	var out []RuleSpec
	for _, spec := range deny {
		r := RuleSpec{Action: Deny, Spec: spec, Scope: scopeFlagDeny}
		if _, err := r.Rule(); err != nil {
			return nil, fmt.Errorf("-deny %s: %w", spec, err)
		}
		out = append(out, r)
	}
	for _, spec := range allow {
		r := RuleSpec{Action: Allow, Spec: spec, Scope: scopeFlagAllow}
		if _, err := r.Rule(); err != nil {
			return nil, fmt.Errorf("-allow %s: %w", spec, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// Describe renders a resolution the way `boks policy ls` shows it: the layers that were
// applied, then the rules, denies first because they are the ones that win, each labelled
// with the scope that would have to change to remove it.
func (r Resolution) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "policy %s\n", r.Name)
	fmt.Fprintf(&b, "default: %s\n", r.Default)

	fmt.Fprint(&b, "\nlayers (later layers add rules; none of them can unsay a deny):\n")
	for _, l := range r.Layers {
		detail := ""
		if l.Detail != "" {
			detail = "  " + l.Detail
		}
		fmt.Fprintf(&b, "  %-24s %2d rule(s)%s\n", l.Source, l.Count, detail)
	}

	var denies, allows []RuleSpec
	for _, rule := range r.Rules {
		if rule.Action == Deny {
			denies = append(denies, rule)
			continue
		}
		allows = append(allows, rule)
	}
	writeRuleSection(&b, "deny (always wins):", denies)
	writeRuleSection(&b, "allow:", allows)
	return b.String()
}

// writeRuleSection prints one disposition's rules, sorted by destination so that a policy
// reads the same way twice.
func writeRuleSection(b *strings.Builder, title string, rules []RuleSpec) {
	fmt.Fprintf(b, "\n%s\n", title)
	if len(rules) == 0 {
		fmt.Fprint(b, "  (none)\n")
		return
	}
	sorted := append([]RuleSpec(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Spec < sorted[j].Spec })

	specWidth, scopeWidth := 0, 0
	for _, r := range sorted {
		if n := len(r.Spec); n > specWidth {
			specWidth = n
		}
		if n := len(r.Scope); n > scopeWidth {
			scopeWidth = n
		}
	}
	for _, r := range sorted {
		if r.Note == "" {
			fmt.Fprintf(b, "  %-*s  %s\n", specWidth, r.Spec, r.Scope)
			continue
		}
		fmt.Fprintf(b, "  %-*s  %-*s  %s\n", specWidth, r.Spec, scopeWidth, r.Scope, r.Note)
	}
}

// SandboxPolicy is what a sandbox remembers about how its policy was chosen.
//
// It is recorded on the container when the sandbox is created and read back by every later
// command that has to bring its network up. Without it, `boks start` and `boks exec` served
// a sandbox the *default* preset rather than the one it was created with, so a sandbox's
// containment changed when you restarted it.
//
// It records the *selection*, not the resolved rules: the profile, the preset, and the
// per-run allow and deny specs. The stored global and per-sandbox rules are deliberately
// left out and re-read at start time, so that a rule added after a sandbox was created still
// reaches it, and so that this record stays small enough to live in a container label.
//
// **Nothing here is a secret.** The fields are destinations and preset names. Credential
// rules name a service and are not recorded; credential values are never written anywhere
// but the encrypted store.
type SandboxPolicy struct {
	// V is the record version, so that a sandbox created by an older Boks is recognisable
	// rather than misread.
	V       int      `json:"v"`
	Profile string   `json:"profile,omitempty"`
	Preset  string   `json:"preset,omitempty"`
	Allow   []string `json:"allow,omitempty"`
	Deny    []string `json:"deny,omitempty"`
}

// SandboxPolicyVersion is the record version this build writes.
const SandboxPolicyVersion = 1

// IsZero reports whether the record says nothing, in which case there is no reason to store
// it on a container.
func (p *SandboxPolicy) IsZero() bool {
	return p == nil || (p.Profile == "" && p.Preset == "" && len(p.Allow) == 0 && len(p.Deny) == 0)
}

// String renders the record the way the flags that produced it would have been typed, for
// the note a command prints when it restores a sandbox's policy.
func (p *SandboxPolicy) String() string {
	if p.IsZero() {
		return "the default policy"
	}
	var parts []string
	if p.Profile != "" {
		parts = append(parts, "-profile "+p.Profile)
	}
	if p.Preset != "" {
		parts = append(parts, "-policy "+p.Preset)
	}
	for _, a := range p.Allow {
		parts = append(parts, "-allow "+a)
	}
	for _, d := range p.Deny {
		parts = append(parts, "-deny "+d)
	}
	return strings.Join(parts, " ")
}

// Request builds the resolution request for a sandbox that already exists, taking the
// selection from what the sandbox recorded.
func (p *SandboxPolicy) Request(store *Store, sandbox string) Request {
	req := Request{Store: store, Sandbox: sandbox}
	if p == nil {
		return req
	}
	req.Profile, req.Preset, req.Allow, req.Deny = p.Profile, p.Preset, p.Allow, p.Deny
	return req
}
