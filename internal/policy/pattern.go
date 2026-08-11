package policy

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Pattern matches the host part of a Target.
//
// Five forms, deliberately few — a policy language you cannot predict the behaviour of is
// worse than one that cannot express everything:
//
//   - "example.com" — exactly that hostname
//   - "*.example.com" — any subdomain of example.com, but NOT example.com itself
//   - "203.0.113.7" — exactly that address literal
//   - "203.0.113.0/24" — any address literal in that prefix
//   - "*" — any destination at all, including IP literals
//
// The wildcard follows the TLS certificate rule: `*.example.com` covers `a.example.com`
// and `a.b.example.com` but not the apex. Certificates behave this way, so it is the
// convention users already carry, and the alternative — a wildcard that quietly widens to
// include the apex — makes deny rules harder to reason about. Write both entries when you
// mean both.
//
// Hostname patterns never match IP literals and address patterns never match hostnames.
// Boks decides before it resolves, so it cannot know that `example.com` is `203.0.113.7`;
// pretending otherwise would produce confident, wrong decisions. `*` is the exception and
// matches everything by definition.
type Pattern struct {
	kind   patternKind
	host   string // exact hostname, or the suffix of a wildcard without its leading dot
	addr   netip.Addr
	prefix netip.Prefix
	text   string // the pattern as written, for diagnostics
}

type patternKind int

const (
	patternAny patternKind = iota
	patternExact
	patternSuffix
	patternAddr
	patternPrefix
)

// ParsePattern parses a host pattern. Wildcards are accepted only as a whole leftmost
// label: `foo*.example.com` and `*.*.example.com` are errors rather than surprises.
func ParsePattern(s string) (Pattern, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Pattern{}, fmt.Errorf("empty host pattern")
	}
	if text == "*" {
		return Pattern{kind: patternAny, text: "*"}, nil
	}
	if strings.Contains(text, "/") {
		prefix, err := netip.ParsePrefix(strings.ToLower(text))
		if err != nil {
			return Pattern{}, fmt.Errorf("host pattern %q is not a valid CIDR prefix: %w", s, err)
		}
		prefix = prefix.Masked()
		return Pattern{kind: patternPrefix, prefix: prefix, text: prefix.String()}, nil
	}
	if strings.HasPrefix(text, "*.") {
		rest := text[2:]
		if strings.Contains(rest, "*") {
			return Pattern{}, fmt.Errorf("host pattern %q: '*' may appear only as the leftmost label", s)
		}
		host, addr, err := normalizeHost(rest)
		if err != nil {
			return Pattern{}, fmt.Errorf("host pattern %q: %w", s, err)
		}
		if addr.IsValid() {
			return Pattern{}, fmt.Errorf("host pattern %q: a wildcard cannot be applied to an IP address; use a CIDR prefix", s)
		}
		return Pattern{kind: patternSuffix, host: host, text: "*." + host}, nil
	}
	if strings.Contains(text, "*") {
		return Pattern{}, fmt.Errorf("host pattern %q: '*' may appear only as the leftmost label, as in *.example.com", s)
	}
	host, addr, err := normalizeHost(text)
	if err != nil {
		return Pattern{}, fmt.Errorf("host pattern %q: %w", s, err)
	}
	if addr.IsValid() {
		return Pattern{kind: patternAddr, addr: addr, host: host, text: host}, nil
	}
	return Pattern{kind: patternExact, host: host, text: host}, nil
}

// MustPattern is ParsePattern for compile-time-constant patterns such as presets.
func MustPattern(s string) Pattern {
	p, err := ParsePattern(s)
	if err != nil {
		panic(err)
	}
	return p
}

// Match reports whether the pattern covers the target's host.
func (p Pattern) Match(t Target) bool {
	switch p.kind {
	case patternAny:
		return true
	case patternExact:
		return !t.IsIP() && t.Host == p.host
	case patternSuffix:
		return !t.IsIP() && strings.HasSuffix(t.Host, "."+p.host)
	case patternAddr:
		return t.IsIP() && t.Addr == p.addr
	case patternPrefix:
		return t.IsIP() && p.prefix.Contains(t.Addr)
	}
	return false
}

// IsAny reports whether the pattern matches every destination. Callers that must not
// accept a catch-all — credential injection, above all — check this.
func (p Pattern) IsAny() bool { return p.kind == patternAny }

func (p Pattern) String() string { return p.text }

// PortSet is the set of ports a rule applies to. The zero value means every port.
type PortSet struct {
	ranges []portRange
}

type portRange struct{ lo, hi int }

// ParsePorts parses a comma-separated list of ports and ranges, such as "80,443" or
// "8000-8100". An empty string means every port.
func ParsePorts(s string) (PortSet, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "*" {
		return PortSet{}, nil
	}
	var set PortSet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, found := strings.Cut(part, "-")
		if !found {
			hi = lo
		}
		l, err := parsePort(lo)
		if err != nil {
			return PortSet{}, err
		}
		h, err := parsePort(hi)
		if err != nil {
			return PortSet{}, err
		}
		if l > h {
			return PortSet{}, fmt.Errorf("port range %q runs backwards", part)
		}
		set.ranges = append(set.ranges, portRange{l, h})
	}
	return set, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d is out of range 1-65535", n)
	}
	return n, nil
}

// Any reports whether the set covers every port.
func (s PortSet) Any() bool { return len(s.ranges) == 0 }

// Match reports whether port is in the set.
func (s PortSet) Match(port int) bool {
	if s.Any() {
		return true
	}
	for _, r := range s.ranges {
		if port >= r.lo && port <= r.hi {
			return true
		}
	}
	return false
}

func (s PortSet) String() string {
	if s.Any() {
		return "*"
	}
	parts := make([]string, 0, len(s.ranges))
	for _, r := range s.ranges {
		if r.lo == r.hi {
			parts = append(parts, strconv.Itoa(r.lo))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", r.lo, r.hi))
	}
	return strings.Join(parts, ",")
}
