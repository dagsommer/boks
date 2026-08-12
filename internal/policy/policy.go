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
}

// WithScope labels a rule with where it came from.
func (r Rule) WithScope(scope string) Rule {
	r.Scope = scope
	return r
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

// MustRule is ParseRule for preset definitions.
func MustRule(action Action, spec, why string) Rule {
	r, err := ParseRule(action, spec)
	if err != nil {
		panic(err)
	}
	r.Why = why
	return r
}

// Match reports whether the rule covers the target.
func (r Rule) Match(t Target) bool {
	return r.Host.Match(t) && r.Ports.Match(t.Port)
}

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

func (r Rule) String() string { return r.Action.String() + " " + r.Spec() }

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
func (p Policy) Evaluate(t Target) Verdict {
	for i := range p.Rules {
		r := p.Rules[i]
		if r.Action == Deny && r.Match(t) {
			return Verdict{
				Allowed: false,
				Rule:    r.Spec(),
				Scope:   r.Scope,
				Reason:  fmt.Sprintf("denied by rule %q", r.Spec()),
			}
		}
	}
	for i := range p.Rules {
		r := p.Rules[i]
		if r.Action == Allow && r.Match(t) {
			return Verdict{
				Allowed: true,
				Rule:    r.Spec(),
				Scope:   r.Scope,
				Reason:  fmt.Sprintf("allowed by rule %q", r.Spec()),
			}
		}
	}
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
// because an allow written for a name says nothing about the address behind it.
func (p Policy) EvaluateDeny(t Target) Verdict {
	for i := range p.Rules {
		r := p.Rules[i]
		if r.Action == Deny && r.Match(t) {
			return Verdict{
				Allowed: false,
				Rule:    r.Spec(),
				Scope:   r.Scope,
				Reason:  fmt.Sprintf("denied by rule %q", r.Spec()),
			}
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
	// "sandbox web", "flag -deny" — or "default" when no rule matched. It tells a user
	// which file or flag to change, which is the question they ask next.
	Scope  string
	Reason string
}
