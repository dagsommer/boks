package policy

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Target is a normalised network destination a policy decision is made about.
//
// Normalisation happens once, at the edge, so that matching is a plain comparison and
// every rule sees the same shape of input. The guest controls this string, so the
// normalisation rules are part of the security surface: two spellings of the same
// destination must not produce two different decisions.
type Target struct {
	// Host is the lowercased hostname without a trailing dot, or the canonical text of
	// an IP literal without brackets or zone.
	Host string
	// Addr is valid when Host is an IP literal.
	Addr netip.Addr
	// Port is the TCP port. Always set; callers supply a default when the wire format
	// omitted one.
	Port int
}

// IsIP reports whether the target names an address literal rather than a hostname.
//
// The distinction matters: hostname rules never match IP literals and IP rules never
// match hostnames, because Boks decides before it resolves.
func (t Target) IsIP() bool { return t.Addr.IsValid() }

func (t Target) String() string {
	if t.IsIP() && t.Addr.Is6() && !t.Addr.Is4In6() {
		return "[" + t.Host + "]:" + strconv.Itoa(t.Port)
	}
	return t.Host + ":" + strconv.Itoa(t.Port)
}

// NewTarget normalises a host and port into a Target.
func NewTarget(host string, port int) (Target, error) {
	if port < 1 || port > 65535 {
		return Target{}, fmt.Errorf("port %d is out of range 1-65535", port)
	}
	h, addr, err := normalizeHost(host)
	if err != nil {
		return Target{}, err
	}
	return Target{Host: h, Addr: addr, Port: port}, nil
}

// ParseTarget normalises a "host:port" or "host" destination, applying defaultPort when
// the port is absent. IPv6 literals must be bracketed when a port is present, as in URLs.
func ParseTarget(hostport string, defaultPort int) (Target, error) {
	host, portText, err := splitHostPort(hostport)
	if err != nil {
		return Target{}, err
	}
	port := defaultPort
	if portText != "" {
		port, err = strconv.Atoi(portText)
		if err != nil {
			return Target{}, fmt.Errorf("port %q in %q is not a number", portText, hostport)
		}
	}
	return NewTarget(host, port)
}

// splitHostPort separates an optional :port suffix without requiring one.
//
// net.SplitHostPort insists on a port and mangles bare IPv6 literals, so this does the
// small amount of work directly. Unbracketed text containing more than one colon is
// treated as an IPv6 literal, which is why "[::1]:443" is the only way to give an IPv6
// address a port.
func splitHostPort(hostport string) (host, port string, err error) {
	s := strings.TrimSpace(hostport)
	if s == "" {
		return "", "", fmt.Errorf("empty destination")
	}
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", fmt.Errorf("destination %q has an unclosed '['", hostport)
		}
		host = s[1:end]
		rest := s[end+1:]
		switch {
		case rest == "":
			return host, "", nil
		case strings.HasPrefix(rest, ":"):
			return host, rest[1:], nil
		default:
			return "", "", fmt.Errorf("destination %q has trailing text after ']'", hostport)
		}
	}
	if strings.Count(s, ":") == 1 {
		i := strings.Index(s, ":")
		return s[:i], s[i+1:], nil
	}
	return s, "", nil
}

// normalizeHost canonicalises a hostname or address literal.
//
// Decisions encoded here, each of which a test pins down:
//   - case is folded, because DNS is case-insensitive and "GitHub.com" must not slip
//     past a rule written as "github.com";
//   - a single trailing dot (the DNS root) is stripped, so "example.com." and
//     "example.com" are one destination;
//   - IPv6 brackets and zone identifiers are removed, and the address is written in its
//     canonical form, so "[::1]", "::1" and "0:0:0:0:0:0:0:1" agree;
//   - IPv4-mapped IPv6 ("::ffff:127.0.0.1") is unmapped, so it cannot be used to dodge
//     an IPv4 rule;
//   - non-ASCII is rejected rather than guessed at: what travels on the wire is the
//     A-label form, and silently accepting a U-label would make rules that never match.
func normalizeHost(host string) (string, netip.Addr, error) {
	h := strings.TrimSpace(host)
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") && len(h) > 2 {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return "", netip.Addr{}, fmt.Errorf("empty host")
	}
	// Only one trailing dot is the DNS root; "example.com.." is malformed.
	if strings.HasSuffix(h, ".") && !strings.HasSuffix(h, "..") {
		h = h[:len(h)-1]
	}
	h = strings.ToLower(h)

	if addr, err := netip.ParseAddr(h); err == nil {
		addr = addr.WithZone("").Unmap()
		return addr.String(), addr, nil
	}
	if err := validHostname(h); err != nil {
		return "", netip.Addr{}, err
	}
	return h, netip.Addr{}, nil
}

// validHostname rejects shapes that could not appear as a real destination, so that
// nonsense fails at parse time rather than silently never matching.
func validHostname(h string) error {
	if len(h) > 253 {
		return fmt.Errorf("host %q is longer than 253 characters", h)
	}
	for _, r := range h {
		if r > 127 {
			return fmt.Errorf("host %q is not ASCII; use its punycode (xn--) form, which is what appears on the wire", h)
		}
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return fmt.Errorf("host %q has an empty label", h)
		}
		if len(label) > 63 {
			return fmt.Errorf("host %q has a label longer than 63 characters", h)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
			if !ok {
				return fmt.Errorf("host %q contains %q, which is not valid in a hostname", h, string(rune(c)))
			}
		}
	}
	return nil
}

// ProbeTarget builds the destination a rule specification names, for the callers that want
// to ask the engine what a rule they just typed would actually do.
//
// It reports false for a pattern that names a set rather than a place — a wildcard or a CIDR
// prefix — because there is no single destination to ask about, and inventing one would
// produce an answer about a host the user never mentioned. The port defaults to 443 when the
// spec names none, which is the port every check that matters is about.
func ProbeTarget(spec string) (Target, bool) {
	host, port, err := splitHostPort(spec)
	if err != nil || strings.Contains(host, "*") || strings.Contains(host, "/") {
		return Target{}, false
	}
	p := 443
	if port != "" {
		// A port set ("80,443", "8000-8100") has no single member to probe with; only a
		// plain port does.
		n, err := strconv.Atoi(port)
		if err != nil {
			return Target{}, false
		}
		p = n
	}
	t, err := NewTarget(host, p)
	if err != nil {
		return Target{}, false
	}
	return t, true
}

// TargetFromAddr builds a Target for an already-resolved address, used when a decision
// must be re-checked against the address a name resolved to.
func TargetFromAddr(ap netip.AddrPort) Target {
	addr := ap.Addr().WithZone("").Unmap()
	return Target{Host: addr.String(), Addr: addr, Port: int(ap.Port())}
}

// HostPortString renders a target the way a dialer expects it.
func (t Target) HostPortString() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}
