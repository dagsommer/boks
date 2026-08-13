package ports

import (
	"net/netip"
	"strings"
	"testing"
)

// TestParsePublishCoversEveryDocumentedForm walks the grammar sbx documents, form by form.
//
// The specification is short enough to enumerate exhaustively, and the parser is the part of
// this feature a user meets first: a specification read wrongly does not fail, it publishes
// the wrong port somewhere the user did not ask for.
func TestParsePublishCoversEveryDocumentedForm(t *testing.T) {
	tests := []struct {
		in   string
		want Spec
	}{
		// SANDBOX_PORT alone: ephemeral host port, loopback, tcp.
		{"3000", Spec{SandboxPort: 3000, Protocol: TCP}},
		// HOST_PORT:SANDBOX_PORT.
		{"8080:3000", Spec{HostPort: 8080, SandboxPort: 3000, Protocol: TCP}},
		// HOST_IP:HOST_PORT:SANDBOX_PORT.
		{"127.0.0.1:8080:3000", Spec{HostIP: netip.MustParseAddr("127.0.0.1"),
			HostPort: 8080, SandboxPort: 3000, Protocol: TCP}},
		// A bracketed IPv6 host address, which is the only spelling accepted. The
		// protocol still defaults to plain tcp — the address decides the family here,
		// and the suffix exists for the case where no address was named.
		{"[::1]:8080:3000", Spec{HostIP: netip.MustParseAddr("::1"),
			HostPort: 8080, SandboxPort: 3000, Protocol: TCP}},
		// An address that is not loopback. Allowed, because the user said it in as many
		// words; never a default.
		{"0.0.0.0:8080:3000", Spec{HostIP: netip.MustParseAddr("0.0.0.0"),
			HostPort: 8080, SandboxPort: 3000, Protocol: TCP}},
		// Every protocol, on the shortest form that can carry it.
		{"3000/tcp", Spec{SandboxPort: 3000, Protocol: TCP}},
		{"3000/tcp4", Spec{SandboxPort: 3000, Protocol: TCP4}},
		{"3000/tcp6", Spec{SandboxPort: 3000, Protocol: TCP6}},
		{"3000/udp", Spec{SandboxPort: 3000, Protocol: UDP}},
		{"3000/udp4", Spec{SandboxPort: 3000, Protocol: UDP4}},
		{"3000/udp6", Spec{SandboxPort: 3000, Protocol: UDP6}},
		// A protocol on the full form, with the address family agreeing.
		{"127.0.0.1:8080:3000/tcp4", Spec{HostIP: netip.MustParseAddr("127.0.0.1"),
			HostPort: 8080, SandboxPort: 3000, Protocol: TCP4}},
		{"[::1]:8080:3000/tcp6", Spec{HostIP: netip.MustParseAddr("::1"),
			HostPort: 8080, SandboxPort: 3000, Protocol: TCP6}},
		// Case is not significant in the protocol.
		{"8080:3000/TCP", Spec{HostPort: 8080, SandboxPort: 3000, Protocol: TCP}},
		// An explicit address with an ephemeral port. Docker's spelling, and the only
		// way to say it; sbx's grammar does not show it.
		{"127.0.0.1::3000", Spec{HostIP: netip.MustParseAddr("127.0.0.1"),
			SandboxPort: 3000, Protocol: TCP}},
		// The extremes of the port range.
		{"1:65535", Spec{HostPort: 1, SandboxPort: 65535, Protocol: TCP}},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParsePublish(tc.in)
			if err != nil {
				t.Fatalf("ParsePublish(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParsePublish(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			// Whatever the parser produced has to survive being written down and read
			// back, because a sandbox records the specifications it was created with.
			round, err := ParsePublish(got.String())
			if err != nil {
				t.Fatalf("re-parsing %q: %v", got.String(), err)
			}
			if round != got {
				t.Errorf("%q round-tripped through %q as %+v", tc.in, got.String(), round)
			}
		})
	}
}

// TestParsePublishRejects covers the mistakes, each with the reason it must be caught.
func TestParsePublishRejects(t *testing.T) {
	tests := map[string]string{
		"":               "an empty specification says nothing",
		"   ":            "whitespace is not a port",
		"0":              "port 0 means 'any' to bind(2) and 'none' here; the ephemeral form is an omitted host port",
		"70000":          "a port outside 16 bits",
		"-1":             "a negative port",
		"http":           "a service name is not a port number; nothing here resolves one",
		"3000/sctp":      "a protocol boks does not carry, named exactly",
		"3000/":          "an empty protocol",
		"::1:8080:3000":  "a bare IPv6 address, which cannot be told from an address with two ports after it",
		"[::1:8080:3000": "an unclosed bracket",
		"[nonsense]:8:9": "a bracketed value that is not an address",
		"1:2:3:4":        "too many fields",
		"1.2.3.4:8080":   "a host address with no sandbox port, which reads as a host port of '1.2.3.4'",
		// The family selector and the address have to agree, or the parser would have
		// to pick one and bind somewhere the user did not ask for.
		"127.0.0.1:8080:3000/tcp6": "an IPv4 address with an IPv6-only protocol",
		"[::1]:8080:3000/tcp4":     "an IPv6 address with an IPv4-only protocol",
	}
	for in, why := range tests {
		t.Run(in, func(t *testing.T) {
			if got, err := ParsePublish(in); err == nil {
				t.Errorf("ParsePublish(%q) = %+v, want an error: %s", in, got, why)
			}
		})
	}
}

// TestParseUnpublishNeedsAHostPort is the one difference between the two grammars, and it is
// not arbitrary: one sandbox port can be published on several host ports, so "unpublish 3000"
// would not say which binding to remove.
func TestParseUnpublishNeedsAHostPort(t *testing.T) {
	if _, err := ParseUnpublish("3000"); err == nil {
		t.Error("--unpublish 3000 was accepted; it names no binding")
	} else if !strings.Contains(err.Error(), "host port") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
	if _, err := ParseUnpublish("127.0.0.1::3000"); err == nil {
		t.Error("--unpublish with an empty host port was accepted")
	}
	for _, in := range []string{"8080:3000", "127.0.0.1:8080:3000", "[::1]:8080:3000/tcp6"} {
		if _, err := ParseUnpublish(in); err != nil {
			t.Errorf("ParseUnpublish(%q): %v", in, err)
		}
	}
}

// TestLoopbackIsTheDefaultBinding is the security-relevant assertion in this package.
//
// A published port is a hole from the host into a VM running untrusted code. On 0.0.0.0 it
// would be a hole from the local network into it — a dev server in a sandbox, reachable from
// the coffee shop. sbx binds loopback by default and Boks copies that exactly, so this test
// exists to make a change to it impossible to make by accident.
func TestLoopbackIsTheDefaultBinding(t *testing.T) {
	for _, proto := range Protocols() {
		t.Run(string(proto), func(t *testing.T) {
			spec, err := ParsePublish("8080:3000/" + string(proto))
			if err != nil {
				t.Fatal(err)
			}
			for _, dual := range []bool{false, true} {
				for _, addr := range spec.Binds(dual) {
					if !addr.IsLoopback() {
						t.Errorf("a specification naming no address bound %s, "+
							"which is not loopback", addr)
					}
				}
			}
		})
	}
}

// TestAddressFamilyExpansion pins sbx's rule: tcp/udp bind both loopbacks on a dual-stack
// sandbox and only 127.0.0.1 on an IPv4-only one, while the numbered forms say which they
// want. Boks' virtual network is IPv4-only today, so the hasIPv6=false column is the one
// every sandbox actually gets.
func TestAddressFamilyExpansion(t *testing.T) {
	tests := []struct {
		spec    string
		hasIPv6 bool
		want    []string
	}{
		{"8080:3000/tcp", false, []string{"127.0.0.1"}},
		{"8080:3000/tcp", true, []string{"127.0.0.1", "::1"}},
		{"8080:3000/udp", false, []string{"127.0.0.1"}},
		{"8080:3000/udp", true, []string{"127.0.0.1", "::1"}},
		{"8080:3000/tcp4", true, []string{"127.0.0.1"}},
		{"8080:3000/udp4", true, []string{"127.0.0.1"}},
		// An explicit v6 request is honoured even on an IPv4-only sandbox: the host
		// binding and the guest's address family are two different things, and a tool
		// whose callback is [::1] has to be reachable.
		{"8080:3000/tcp6", false, []string{"::1"}},
		{"8080:3000/udp6", true, []string{"::1"}},
		// A named address is used as given and never expanded.
		{"127.0.0.1:8080:3000", true, []string{"127.0.0.1"}},
		{"[::1]:8080:3000", true, []string{"::1"}},
	}
	for _, tc := range tests {
		t.Run(tc.spec+dualLabel(tc.hasIPv6), func(t *testing.T) {
			spec, err := ParsePublish(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			got := spec.Binds(tc.hasIPv6)
			if len(got) != len(tc.want) {
				t.Fatalf("Binds = %v, want %v", got, tc.want)
			}
			for i, addr := range got {
				if addr.String() != tc.want[i] {
					t.Errorf("Binds[%d] = %s, want %s", i, addr, tc.want[i])
				}
			}
		})
	}
}

func dualLabel(dual bool) string {
	if dual {
		return " (dual-stack)"
	}
	return " (v4-only)"
}

// TestPublishedRendersLikeDocker: the PORTS column of `boks ls` is sbx's column, and its
// notation is Docker's. A user reading it should not have to learn a third spelling.
func TestPublishedRendersLikeDocker(t *testing.T) {
	tests := map[Published]string{
		{HostIP: "127.0.0.1", HostPort: 8080, SandboxPort: 3000, Protocol: "tcp"}: "127.0.0.1:8080->3000/tcp",
		{HostIP: "::1", HostPort: 8080, SandboxPort: 3000, Protocol: "tcp"}:       "[::1]:8080->3000/tcp",
	}
	for p, want := range tests {
		if got := p.String(); got != want {
			t.Errorf("Published.String() = %q, want %q", got, want)
		}
	}
}

// TestUnpublishMatchesWhatWasPublished: a user who typed `--publish 8080:3000` must be able to
// type `--unpublish 8080:3000` without knowing how the default expanded, or which address
// family suffix the binding ended up recorded under.
func TestUnpublishMatchesWhatWasPublished(t *testing.T) {
	published := Published{HostIP: "127.0.0.1", HostPort: 8080, SandboxPort: 3000, Protocol: "tcp"}

	for _, in := range []string{"8080:3000", "8080:3000/tcp", "8080:3000/tcp4", "127.0.0.1:8080:3000"} {
		spec, err := ParseUnpublish(in)
		if err != nil {
			t.Fatal(err)
		}
		if !published.matches(spec) {
			t.Errorf("--unpublish %q did not match %s", in, published)
		}
	}
	for _, in := range []string{"8081:3000", "8080:3001", "8080:3000/udp", "[::1]:8080:3000/tcp6"} {
		spec, err := ParseUnpublish(in)
		if err != nil {
			t.Fatal(err)
		}
		if published.matches(spec) {
			t.Errorf("--unpublish %q matched %s, which it does not name", in, published)
		}
	}
}
