// Package policy decides whether a sandbox may reach a network destination.
//
// It is pure logic: no sockets, no files, no clock beyond timestamping decisions. That is
// deliberate. The decision function is the part of Boks whose behaviour has to be
// predictable under adversarial input, so it is kept small enough to test exhaustively and
// separate from the machinery that acts on its answers.
//
// # What this package is not
//
// A policy engine is not an enforcement boundary on its own; it is the thing an
// enforcement point asks. Two ask it, and the difference is worth keeping straight:
//
//   - the host-side network stack (internal/network) judges every TCP connection the guest
//     opens, before it dials it, from the address and port in the SYN. That decision holds
//     whether or not the guest cooperates, and is logged with mode "transparent".
//   - the forward proxy (internal/proxy) judges what a cooperating client sends it, where
//     there is a hostname to judge and a readable refusal to return.
//
// **An environment variable is still not a security boundary**, and the proxy is still
// reachable only by a client that chooses it. What changed is that ignoring the proxy no
// longer means escaping the policy: it means being judged on addresses instead of names.
//
// One caveat that no amount of stack-level enforcement removes: a hostname rule cannot be
// applied to a raw flow, because a SYN carries no name. A policy written entirely in
// hostnames therefore denies raw flows by default rather than permitting the addresses
// those names resolve to.
//
// # Evaluation order
//
//  1. every deny rule is tested; the first match denies, and nothing can override it;
//  2. every allow rule is tested; the first match allows;
//  3. otherwise the policy's default action applies.
//
// Deny always beats allow, regardless of specificity or order in the file. This makes a
// deny rule something you can reason about without reading the rest of the policy, at the
// cost of not being able to carve an allow exception out of a deny. That trade is the
// right way round for a security control: the failure mode is "too little access", which
// is visible and fixable, rather than "more access than you thought".
package policy

import (
	"fmt"
	"strings"
)

// Action is the outcome of a rule or the default disposition of a policy.
type Action int

const (
	Deny Action = iota
	Allow
)

func (a Action) String() string {
	if a == Allow {
		return "allow"
	}
	return "deny"
}

// ParseAction parses "allow" or "deny".
func ParseAction(s string) (Action, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return Allow, nil
	case "deny":
		return Deny, nil
	}
	return Deny, fmt.Errorf("unknown action %q; use \"allow\" or \"deny\"", s)
}

// Rule permits or forbids a set of destinations.
type Rule struct {
	Action Action
	Host   Pattern
	Ports  PortSet
	// Why explains the rule to a human reading `boks policy ls`. Presets fill it in;
	// rules from the command line leave it empty.
	Why string
	// Scope says where the rule came from — a preset, a profile, the global store, one
	// sandbox's own rules, or a flag on this run. It carries no weight in the decision:
	// deny beats allow no matter which scope either sits in. It exists so that a verdict
	// can name the thing a user would have to edit to change it.
	Scope string
	// HTTP restricts the rule to HTTP requests matching a method, path and query selector.
	// Nil is an ordinary connection rule. See http.go for what an HTTP rule means for a flow
	// Boks cannot read.
	HTTP *HTTPMatch
}

// WithScope labels a rule with where it came from.
func (r Rule) WithScope(scope string) Rule {
	r.Scope = scope
	return r
}

// WarnSingleLabel reports whether the rule's host pattern looks like a single-label
// hostname — no dot, not an IP, not a wildcard — which almost always means a shell
// glob expansion or tab-completion mishap that will silently never match any real
// network destination.
func (r Rule) WarnSingleLabel() bool {
	return r.Host.IsSingleLabel()
}

// ParseRule builds a rule from an action and a "host[:ports]" specification.
//
// Examples: "github.com", "github.com:443", "*.githubusercontent.com:443",
// "10.0.0.0/8", "[::1]:8080", "*:22". An IPv6 literal needs brackets before a port,
// exactly as in a URL.
func ParseRule(action Action, spec string) (Rule, error) {
	host, ports, err := splitHostPort(spec)
	if err != nil {
		return Rule{}, err
	}
	pattern, err := ParsePattern(host)
	if err != nil {
		return Rule{}, err
	}
	set, err := ParsePorts(ports)
	if err != nil {
		return Rule{}, fmt.Errorf("rule %q: %w", spec, err)
	}
	return Rule{Action: action, Host: pattern, Ports: set}, nil
}

// ParseHTTPRule builds a rule with an optional HTTP selector from the --method, --path and
// --query values. With none of them it is ParseRule.
func ParseHTTPRule(action Action, spec string, methods []string, path string, query []string) (Rule, error) {
	rule, err := ParseRule(action, spec)
	if err != nil {
		return Rule{}, err
	}
	match, err := ParseHTTPMatch(methods, path, query)
	if err != nil {
		return Rule{}, err
	}
	if match == nil {
		return rule, nil
	}
	if rule.Host.kind == patternAddr || rule.Host.kind == patternPrefix {
		return Rule{}, fmt.Errorf("HTTP rule resource %q must be a host pattern, not an IP address", spec)
	}
	rule.HTTP = match
	return rule, nil
}

// MustRule is ParseRule for preset definitions.
func MustRule(action Action, spec, why string) Rule {
	r, err := ParseRule(action, spec)
	if err != nil {
		panic(err)
	}
	r.Why = why
	return r
}

// Match reports whether the rule covers the target's host and port. For an HTTP rule that is
// only half the question — the request has to match too — and an HTTP rule never covers an
// address literal, even through "**": a flow addressed by IP is decided by address rules alone.
func (r Rule) Match(t Target) bool {
	if r.HTTP != nil && t.IsIP() {
		return false
	}
	return r.Host.Match(t) && r.Ports.Match(t.Port)
}

// MatchRequest reports whether the rule covers an HTTP request to the target. A connection
// rule covers every request to a destination it covers.
func (r Rule) MatchRequest(t Target, req HTTPRequest) bool {
	if !r.Match(t) {
		return false
	}
	if r.HTTP == nil {
		return true
	}
	return r.HTTP.matchRequest(req, r.Action == Deny)
}

// IsHTTP reports whether the rule has an HTTP selector.
func (r Rule) IsHTTP() bool { return r.HTTP != nil }

// Spec renders the rule's destination in the syntax ParseRule accepts.
func (r Rule) Spec() string {
	if r.Ports.Any() {
		return r.Host.String()
	}
	host := r.Host.String()
	if strings.Contains(host, ":") { // IPv6 literal or prefix
		host = "[" + host + "]"
	}
	return host + ":" + r.Ports.String()
}

// Describe is the destination plus, for an HTTP rule, its selector in flag form:
// "api.example.com --method GET --path /v1/**". It is what verdicts and the log name.
func (r Rule) Describe() string {
	if r.HTTP == nil {
		return r.Spec()
	}
	return r.Spec() + " " + r.HTTP.String()
}

// RuleSpec is the rule in its stored form.
func (r Rule) RuleSpec() RuleSpec {
	spec := RuleSpec{Action: r.Action, Spec: r.Spec(), Note: r.Why, Scope: r.Scope}
	if r.HTTP != nil {
		spec.Methods = r.HTTP.Methods()
		spec.Path = r.HTTP.Path()
		spec.Query = r.HTTP.Query()
	}
	return spec
}

func (r Rule) String() string { return r.Action.String() + " " + r.Describe() }

// Policy is a named set of rules plus the disposition for destinations no rule mentions.
type Policy struct {
	// Name identifies the policy in decisions and in `boks policy ls`.
	Name string
	// Default applies when no rule matches. Deny-by-default is the intended posture;
	// Allow exists so that an "open" preset can still carry deny rules.
	Default Action
	Rules   []Rule
}

// Evaluate decides a target, applying deny precedence.
//
// Precedence is over the whole rule set, not over the scopes that contributed to it. A deny
// written for one sandbox and a deny written globally are the same kind of thing once they
// are here, and either beats every allow. That is what makes "deny wins" a property a user
// can hold in their head after adding a sandbox-scoped rule.
//
// A connection carries no request, so HTTP rules cannot be applied to it; what they do instead
// is described in http.go. In short, the verdict is a refusal with NeedsHTTP set whenever the
// outcome would depend on an HTTP rule — an HTTP allow is the only thing permitting the host,
// or an HTTP deny covers a host that is otherwise permitted. A caller that cannot read the flow
// as HTTP therefore refuses it without having to know anything about HTTP rules, and a caller
// that can (the proxy, on a flow it reads) judges each request with EvaluateHTTP.
func (p Policy) Evaluate(t Target) Verdict {
	var httpDeny, httpAllow *Rule
	for i := range p.Rules {
		r := &p.Rules[i]
		if !r.Match(t) {
			continue
		}
		switch {
		case r.Action == Deny && r.HTTP == nil:
			return denyVerdict(*r)
		case r.Action == Deny:
			if httpDeny == nil {
				httpDeny = r
			}
		case r.HTTP != nil:
			if httpAllow == nil {
				httpAllow = r
			}
		}
	}

	verdict, allowed := Verdict{}, false
	for i := range p.Rules {
		r := p.Rules[i]
		if r.Action == Allow && r.HTTP == nil && r.Match(t) {
			verdict, allowed = allowVerdict(r), true
			break
		}
	}
	if !allowed && p.Default == Allow {
		verdict, allowed = defaultVerdict(p), true
	}

	switch {
	case allowed && httpDeny != nil:
		return Verdict{
			NeedsHTTP: true,
			Rule:      httpDeny.Describe(),
			Scope:     httpDeny.Scope,
			Reason: fmt.Sprintf("the HTTP deny rule %q covers this host and can only be enforced on "+
				"requests boks reads; a connection boks cannot read as HTTP is refused", httpDeny.Describe()),
		}
	case allowed:
		return verdict
	case httpAllow != nil:
		return Verdict{
			NeedsHTTP: true,
			Rule:      httpAllow.Describe(),
			Scope:     httpAllow.Scope,
			Reason: fmt.Sprintf("only HTTP requests matching %q are allowed to this host; a connection "+
				"boks cannot read as HTTP is refused", httpAllow.Describe()),
		}
	}
	return defaultVerdict(p)
}

// EvaluateHTTP decides one HTTP request, to a target, that Boks has read. Every rule takes
// part: a connection rule covers every request to its destination, an HTTP rule only the
// requests its selector matches. Deny still wins over allow, whichever kind either is.
func (p Policy) EvaluateHTTP(t Target, req HTTPRequest) Verdict {
	for i := range p.Rules {
		r := p.Rules[i]
		if r.Action == Deny && r.MatchRequest(t, req) {
			return denyVerdict(r)
		}
	}
	for i := range p.Rules {
		r := p.Rules[i]
		if r.Action == Allow && r.MatchRequest(t, req) {
			return allowVerdict(r)
		}
	}
	return defaultVerdict(p)
}

// HasHTTPRules reports whether any HTTP rule covers the target's host and port — whether a
// flow to it has to be read as HTTP to be decided. The proxy terminates TLS for such a host.
func (p Policy) HasHTTPRules(t Target) bool {
	for i := range p.Rules {
		if p.Rules[i].HTTP != nil && p.Rules[i].Match(t) {
			return true
		}
	}
	return false
}

func denyVerdict(r Rule) Verdict {
	reason := fmt.Sprintf("denied by rule %q", r.Describe())
	if r.HTTP != nil {
		reason = fmt.Sprintf("denied by HTTP rule %q", r.Describe())
	}
	return Verdict{Allowed: false, Rule: r.Describe(), Scope: r.Scope, Reason: reason}
}

func allowVerdict(r Rule) Verdict {
	reason := fmt.Sprintf("allowed by rule %q", r.Describe())
	if r.HTTP != nil {
		reason = fmt.Sprintf("allowed by HTTP rule %q", r.Describe())
	}
	return Verdict{Allowed: true, Rule: r.Describe(), Scope: r.Scope, Reason: reason}
}

func defaultVerdict(p Policy) Verdict {
	if p.Default == Allow {
		return Verdict{
			Allowed: true,
			Scope:   "default",
			Reason:  fmt.Sprintf("allowed by default (policy %q allows anything not denied)", p.Name),
		}
	}
	return Verdict{
		Allowed: false,
		Scope:   "default",
		Reason:  fmt.Sprintf("denied by default (policy %q allows only listed destinations)", p.Name),
	}
}

// EvaluateDeny applies only the deny rules.
//
// It exists for the second check the proxy makes: a hostname is allowed by name, then
// resolved, and the address it resolved to is tested against the deny rules before any
// packet is sent. Without that step `evil.test A 127.0.0.1` turns a hostname allowlist
// into a path to the host's own services. The allow rules are deliberately not consulted,
// because an allow written for a name says nothing about the address behind it. HTTP rules
// never cover an address, so they take no part here; they were applied to the request.
func (p Policy) EvaluateDeny(t Target) Verdict {
	for i := range p.Rules {
		r := p.Rules[i]
		if r.Action == Deny && r.HTTP == nil && r.Match(t) {
			return denyVerdict(r)
		}
	}
	return Verdict{Allowed: true, Reason: "no deny rule matched the resolved address"}
}

// With returns a copy of the policy with extra rules appended. Presets are values, so
// command-line rules never mutate a shared definition.
func (p Policy) With(rules ...Rule) Policy {
	out := p
	out.Rules = make([]Rule, 0, len(p.Rules)+len(rules))
	out.Rules = append(out.Rules, p.Rules...)
	out.Rules = append(out.Rules, rules...)
	return out
}

// Verdict is the outcome of evaluating a target, with the reasoning attached. The reason
// travels into the decision log and into the proxy's error body, so a user who is blocked
// learns why without turning on a debug flag.
type Verdict struct {
	Allowed bool
	// Rule is the matched rule's specification, empty when the default applied.
	Rule string
	// Scope is where the matching rule was written — "preset standard", "global",
	// "sandbox web", "flag --deny" — or "default" when no rule matched. It tells a user
	// which file or flag to change, which is the question they ask next.
	Scope  string
	Reason string
	// NeedsHTTP marks a refusal that holds only for a flow Boks cannot read as HTTP: the
	// destination has HTTP rules, and the decision has to be taken request by request with
	// EvaluateHTTP. A caller that cannot do that treats it as the refusal it is.
	NeedsHTTP bool
}
