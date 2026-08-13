// Package ports publishes a port inside a sandbox on the host.
//
// # What a published port is
//
// A hole. It is worth starting there, because everything else in this package is shaped by
// it: a published port lets something on the host open a TCP connection into a VM that is
// running code nobody trusts. That is the point — you cannot reach a dev server otherwise,
// and an OAuth login whose CLI listens on 127.0.0.1 inside the guest never receives its
// callback without one — but it is still a hole, and it is deliberately the only one.
//
// Two consequences follow, and both are copied from Docker Sandboxes rather than invented:
//
//   - **A port with no HOST_IP binds loopback, never 0.0.0.0.** Binding all interfaces would
//     put a sandbox's dev server on the coffee-shop wifi. sbx binds `127.0.0.1` (and `::1`),
//     and Boks copies that exactly, because the default is the only setting most people will
//     ever use.
//   - **Nothing is opened implicitly.** gvisor-tap-vsock has a `Forwards` map that would do
//     this generically; internal/network asserts it empty and a test pins it. Publishing is
//     per-port, per-sandbox and requested by a human, and it is built as one hole at a time
//     rather than by turning on a mechanism.
//
// # The grammar
//
// sbx's, verbatim from its help:
//
//	--publish     [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]
//	--unpublish   [HOST_IP:]HOST_PORT:SANDBOX_PORT[/PROTOCOL]
//
// with HOST_PORT omitted meaning "allocate an ephemeral one", HOST_IP omitted meaning
// loopback expanded per address family, and PROTOCOL defaulting to tcp.
//
// Unpublish requires HOST_PORT because that is the identity of the thing being removed: a
// sandbox port can be published on several host ports, and "unpublish 3000" would not say
// which.
package ports

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Protocol is one of the six spellings sbx accepts.
//
// The suffix is an address-family selector for the *host* binding, not a second transport:
// `tcp4` and `tcp6` are both TCP, and they differ only in which loopback address is bound
// when the spec names none.
type Protocol string

const (
	TCP  Protocol = "tcp"
	TCP4 Protocol = "tcp4"
	TCP6 Protocol = "tcp6"
	UDP  Protocol = "udp"
	UDP4 Protocol = "udp4"
	UDP6 Protocol = "udp6"
)

// Protocols lists them in the order sbx's help does.
func Protocols() []Protocol { return []Protocol{TCP, TCP4, TCP6, UDP, UDP4, UDP6} }

// IsUDP reports whether a protocol is a datagram one.
func (p Protocol) IsUDP() bool { return p == UDP || p == UDP4 || p == UDP6 }

// family returns the address family the protocol pins the host binding to: 4, 6, or 0 for
// "whichever the sandbox has".
func (p Protocol) family() int {
	switch p {
	case TCP4, UDP4:
		return 4
	case TCP6, UDP6:
		return 6
	}
	return 0
}

func parseProtocol(s string) (Protocol, error) {
	for _, p := range Protocols() {
		if string(p) == strings.ToLower(s) {
			return p, nil
		}
	}
	names := make([]string, 0, len(Protocols()))
	for _, p := range Protocols() {
		names = append(names, string(p))
	}
	return "", fmt.Errorf("unknown protocol %q; use one of: %s", s, strings.Join(names, ", "))
}

// Spec is a parsed publish request: what the user asked for, before anything is bound.
//
// It is deliberately not the same type as Published. A spec may name no host port and no
// host address and therefore describes *several* bindings that do not exist yet; a Published
// is one binding that does. Collapsing the two would make "the port I asked for" and "the
// port I got" the same field, and with an ephemeral allocation they are not.
type Spec struct {
	// HostIP is the address to bind, or the zero Addr for "loopback, expanded per
	// protocol and the sandbox's address families".
	HostIP netip.Addr
	// HostPort is the port to bind on the host, or 0 for an ephemeral one.
	HostPort int
	// SandboxPort is the port inside the sandbox to forward to. Always set.
	SandboxPort int
	Protocol    Protocol
}

// ParsePublish parses a --publish argument: [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL].
func ParsePublish(s string) (Spec, error) { return parse(s, false) }

// ParseUnpublish parses an --unpublish argument: [HOST_IP:]HOST_PORT:SANDBOX_PORT[/PROTOCOL].
//
// The host port is mandatory here and optional there, which is the whole difference: on the
// way in an unnamed host port means "pick one", and on the way out it would mean "guess
// which one I meant".
func ParseUnpublish(s string) (Spec, error) { return parse(s, true) }

func parse(s string, needHostPort bool) (Spec, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Spec{}, fmt.Errorf("empty port specification; expected %s", grammar(needHostPort))
	}

	spec := Spec{Protocol: TCP}
	// The protocol suffix is split from the *end*, and only at a slash that follows the
	// last colon, so that nothing in an IPv6 literal can be mistaken for one.
	if i := strings.LastIndex(text, "/"); i >= 0 {
		proto, err := parseProtocol(text[i+1:])
		if err != nil {
			return Spec{}, fmt.Errorf("%q: %w", s, err)
		}
		spec.Protocol = proto
		text = text[:i]
	}

	host, rest, err := splitHostIP(text, s)
	if err != nil {
		return Spec{}, err
	}
	spec.HostIP = host

	fields := strings.Split(rest, ":")
	if host.IsValid() && len(fields) != 2 {
		return Spec{}, fmt.Errorf("%q: a host address is only meaningful with a host port; expected %s",
			s, grammar(needHostPort))
	}
	switch len(fields) {
	case 1:
		// SANDBOX_PORT alone: an ephemeral host port on loopback.
		if spec.SandboxPort, err = parsePort(fields[0], "sandbox port", s); err != nil {
			return Spec{}, err
		}
	case 2:
		// An empty host port with an explicit address — `127.0.0.1::3000` — is the only
		// way to say "an ephemeral port on this address". It is Docker's spelling and
		// the one form here that sbx's grammar does not show; accepting it cannot
		// misread anything else, and refusing it would leave that combination
		// unsayable.
		if fields[0] != "" {
			if spec.HostPort, err = parsePort(fields[0], "host port", s); err != nil {
				return Spec{}, err
			}
		}
		if spec.SandboxPort, err = parsePort(fields[1], "sandbox port", s); err != nil {
			return Spec{}, err
		}
	default:
		return Spec{}, fmt.Errorf("%q has too many colon-separated fields; expected %s.\n"+
			"An IPv6 host address must be bracketed: [::1]:8080:8080", s, grammar(needHostPort))
	}

	if needHostPort && spec.HostPort == 0 {
		return Spec{}, fmt.Errorf("%q names no host port; --unpublish needs one to say which "+
			"binding to remove, since one sandbox port may be published on several.\n"+
			"Expected %s", s, grammar(true))
	}
	if err := spec.checkFamily(s); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// splitHostIP peels a leading host address off the specification.
//
// A bracketed address is taken as written; an unbracketed one is only recognised in the
// three-field form, where the first field is not a number. A bare IPv6 literal is never
// accepted, because `::1:8080:8080` cannot be told apart from an address with two ports
// after it — which is exactly why Docker requires the brackets too.
func splitHostIP(text, original string) (netip.Addr, string, error) {
	if strings.HasPrefix(text, "[") {
		end := strings.Index(text, "]")
		if end < 0 {
			return netip.Addr{}, "", fmt.Errorf("%q: the bracketed host address is not closed", original)
		}
		addr, err := netip.ParseAddr(text[1:end])
		if err != nil {
			return netip.Addr{}, "", fmt.Errorf("%q: host address %q: %w", original, text[1:end], err)
		}
		rest := strings.TrimPrefix(text[end+1:], ":")
		return addr.Unmap(), rest, nil
	}

	fields := strings.Split(text, ":")
	if len(fields) != 3 {
		return netip.Addr{}, text, nil
	}
	addr, err := netip.ParseAddr(fields[0])
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("%q: host address %q: %w\n"+
			"An IPv6 host address must be bracketed: [::1]:8080:8080", original, fields[0], err)
	}
	return addr.Unmap(), strings.Join(fields[1:], ":"), nil
}

func parsePort(text, what, original string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("%q: %s %q is not a number", original, what, text)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q: %s %d is outside 1-65535", original, what, port)
	}
	return port, nil
}

// checkFamily refuses a host address that contradicts the protocol's family selector.
//
// `127.0.0.1:8080:8080/tcp6` is not a request the parser can honour in either direction, and
// silently preferring one half over the other is how a user ends up with a port bound
// somewhere they did not ask for.
func (s Spec) checkFamily(original string) error {
	if !s.HostIP.IsValid() {
		return nil
	}
	want := s.Protocol.family()
	got := 4
	if s.HostIP.Is6() {
		got = 6
	}
	if want != 0 && want != got {
		return fmt.Errorf("%q: host address %s is IPv%d but the protocol %s selects IPv%d",
			original, s.HostIP, got, s.Protocol, want)
	}
	return nil
}

func grammar(needHostPort bool) string {
	if needHostPort {
		return "[HOST_IP:]HOST_PORT:SANDBOX_PORT[/PROTOCOL]"
	}
	return "[[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]"
}

// String renders a spec back in the grammar it was parsed from, so that what a sandbox
// recorded can be shown to a human and re-parsed by a machine.
func (s Spec) String() string {
	var b strings.Builder
	if s.HostIP.IsValid() {
		if s.HostIP.Is6() {
			b.WriteString("[" + s.HostIP.String() + "]:")
		} else {
			b.WriteString(s.HostIP.String() + ":")
		}
		if s.HostPort > 0 {
			b.WriteString(strconv.Itoa(s.HostPort))
		}
		b.WriteString(":")
	} else if s.HostPort > 0 {
		b.WriteString(strconv.Itoa(s.HostPort) + ":")
	}
	b.WriteString(strconv.Itoa(s.SandboxPort))
	b.WriteString("/" + string(s.Protocol))
	return b.String()
}

// Loopback addresses. These are the defaults, and the security-relevant part of this
// package: a specification that names no address gets one of these and never 0.0.0.0.
var (
	loopback4 = netip.MustParseAddr("127.0.0.1")
	loopback6 = netip.MustParseAddr("::1")
)

// Binds returns the host addresses this specification asks to bind.
//
// The expansion is sbx's, and the sandbox's own address families are part of it: `tcp` and
// `udp` bind both loopbacks on a dual-stack sandbox and only `127.0.0.1` on an IPv4-only one,
// while `tcp4`/`udp4` and `tcp6`/`udp6` say which they want and get it.
//
// Boks' virtual network is IPv4-only today — internal/network drops IPv6 at the link — so in
// practice hasIPv6 is false and the default binds `127.0.0.1` alone. An explicit `tcp6` is
// still honoured, because the host binding and the guest's address family are two different
// things: the forwarder accepts on `::1` and dials the guest over IPv4. That is what makes a
// tool whose loopback callback is `[::1]` reachable at all.
func (s Spec) Binds(hasIPv6 bool) []netip.Addr {
	if s.HostIP.IsValid() {
		return []netip.Addr{s.HostIP}
	}
	switch s.Protocol.family() {
	case 4:
		return []netip.Addr{loopback4}
	case 6:
		return []netip.Addr{loopback6}
	}
	if hasIPv6 {
		return []netip.Addr{loopback4, loopback6}
	}
	return []netip.Addr{loopback4}
}

// Published is one binding that exists: a host address and port that is forwarding into a
// sandbox port right now.
type Published struct {
	HostIP      string `json:"host_ip"`
	HostPort    int    `json:"host_port"`
	SandboxPort int    `json:"sandbox_port"`
	Protocol    string `json:"protocol"`
	// LastError is the most recent reason a connection to this port failed to reach the
	// guest, or "" if none has.
	//
	// It exists because the failure that matters most cannot be reported when the port is
	// published: at that moment nothing needs to be listening inside the sandbox yet. The
	// error arrives later, on the first connection, in a process nobody is watching — so
	// it is kept here, where `boks ports` shows it.
	LastError string `json:"last_error,omitempty"`
}

// String renders a published port the way Docker and sbx do in a listing:
// `127.0.0.1:8080->3000/tcp`.
func (p Published) String() string {
	host := p.HostIP
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d->%d/%s", host, p.HostPort, p.SandboxPort, p.Protocol)
}

// matches reports whether a published binding is the one an unpublish specification names.
// A specification with no host address matches every address that port was bound on, which
// is what makes `--unpublish 8080:3000` undo `--publish 8080:3000` without the user having
// to know how the default expanded.
func (p Published) matches(s Spec) bool {
	if p.HostPort != s.HostPort || p.SandboxPort != s.SandboxPort {
		return false
	}
	if !sameTransport(Protocol(p.Protocol), s.Protocol) {
		return false
	}
	return !s.HostIP.IsValid() || s.HostIP.String() == p.HostIP
}

// sameTransport compares two protocols ignoring the address-family suffix, because the
// suffix chose which address to bind and the binding's own address already records that.
// Without this, a port published as `tcp` on 127.0.0.1 could not be removed with `tcp4`.
func sameTransport(a, b Protocol) bool { return a.IsUDP() == b.IsUDP() }
