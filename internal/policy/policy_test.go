package policy

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustTarget(t *testing.T, hostport string) Target {
	t.Helper()
	tgt, err := ParseTarget(hostport, 443)
	if err != nil {
		t.Fatalf("ParseTarget(%q): %v", hostport, err)
	}
	return tgt
}

func TestPatternMatching(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		target  string
		want    bool
	}{
		// Exact hostnames, and the normalisations that must not create a bypass.
		{"exact match", "example.com", "example.com:443", true},
		{"case is folded", "example.com", "EXAMPLE.CoM:443", true},
		{"pattern case is folded", "ExAmple.COM", "example.com:443", true},
		{"trailing dot on target", "example.com", "example.com.:443", true},
		{"trailing dot on pattern", "example.com.", "example.com:443", true},
		{"different host", "example.com", "example.org:443", false},
		{"suffix is not a match", "example.com", "notexample.com:443", false},
		{"subdomain needs a wildcard", "example.com", "a.example.com:443", false},

		// Wildcards follow the TLS certificate rule: subdomains, never the apex.
		{"wildcard matches one label", "*.example.com", "a.example.com:443", true},
		{"wildcard matches several labels", "*.example.com", "a.b.c.example.com:443", true},
		{"wildcard does not match the apex", "*.example.com", "example.com:443", false},
		{"wildcard does not match a sibling", "*.example.com", "evilexample.com:443", false},
		{"wildcard does not match a longer parent", "*.example.com", "example.com.evil.com:443", false},
		{"wildcard is case-insensitive", "*.Example.COM", "A.example.com:443", true},

		// * matches everything, including addresses.
		{"any matches a hostname", "*", "example.com:443", true},
		{"any matches an address", "*", "203.0.113.5:443", true},

		// Hostname rules and address rules do not cross over.
		{"hostname rule ignores an address", "example.com", "203.0.113.5:443", false},
		{"wildcard rule ignores an address", "*.example.com", "203.0.113.5:443", false},
		{"address rule ignores a hostname", "203.0.113.5", "example.com:443", false},
		{"cidr rule ignores a hostname", "203.0.113.0/24", "example.com:443", false},

		// Address literals, in every spelling of the same address.
		{"exact address", "203.0.113.5", "203.0.113.5:443", true},
		{"different address", "203.0.113.5", "203.0.113.6:443", false},
		{"cidr contains", "203.0.113.0/24", "203.0.113.99:443", true},
		{"cidr excludes", "203.0.113.0/24", "203.0.114.1:443", false},
		{"ipv6 loopback bracketed", "::1", "[::1]:443", true},
		{"ipv6 loopback expanded", "::1", "[0:0:0:0:0:0:0:1]:443", true},
		{"ipv6 zone is ignored", "fe80::1", "[fe80::1%eth0]:443", true},
		{"ipv6 cidr", "fc00::/7", "[fd12::3]:443", true},
		{"ipv4-mapped ipv6 is unmapped", "127.0.0.0/8", "[::ffff:127.0.0.1]:443", true},
		{"ipv4-mapped literal is unmapped", "127.0.0.1", "[::ffff:127.0.0.1]:443", true},
		{"unmasked cidr is masked", "203.0.113.7/24", "203.0.113.99:443", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePattern(tc.pattern)
			if err != nil {
				t.Fatalf("ParsePattern(%q): %v", tc.pattern, err)
			}
			got := p.Match(mustTarget(t, tc.target))
			if got != tc.want {
				t.Errorf("Pattern(%q).Match(%q) = %v, want %v", tc.pattern, tc.target, got, tc.want)
			}
		})
	}
}

func TestParsePatternRejects(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"foo*.example.com",
		"*.*.example.com",
		"ex*ample.com",
		"*.203.0.113.0",
		"exam ple.com",
		"example..com",
		"münchen.de",
		"203.0.113.0/33",
		strings.Repeat("a", 64) + ".example.com",
	}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			if p, err := ParsePattern(s); err == nil {
				t.Errorf("ParsePattern(%q) = %v, want an error", s, p)
			}
		})
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in       string
		def      int
		wantHost string
		wantPort int
	}{
		{"example.com", 443, "example.com", 443},
		{"example.com:8443", 443, "example.com", 8443},
		{"EXAMPLE.com.:80", 443, "example.com", 80},
		{"[::1]:8080", 443, "::1", 8080},
		{"::1", 443, "::1", 443},
		{"2001:db8::1", 443, "2001:db8::1", 443},
		{"[2001:db8::1]", 443, "2001:db8::1", 443},
		{"::ffff:203.0.113.5", 443, "203.0.113.5", 443},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseTarget(tc.in, tc.def)
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tc.in, err)
			}
			if got.Host != tc.wantHost || got.Port != tc.wantPort {
				t.Errorf("ParseTarget(%q) = %s:%d, want %s:%d", tc.in, got.Host, got.Port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestParseTargetRejects(t *testing.T) {
	bad := []string{"", ":443", "example.com:0", "example.com:70000", "example.com:https", "[::1", "[::1]junk"}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			if got, err := ParseTarget(s, 443); err == nil {
				t.Errorf("ParseTarget(%q) = %v, want an error", s, got)
			}
		})
	}
}

func TestPorts(t *testing.T) {
	tests := []struct {
		spec  string
		port  int
		match bool
	}{
		{"", 1, true},
		{"*", 65535, true},
		{"443", 443, true},
		{"443", 80, false},
		{"80,443", 80, true},
		{"80,443", 8080, false},
		{"8000-8100", 8000, true},
		{"8000-8100", 8100, true},
		{"8000-8100", 8101, false},
		{"80,8000-8100", 8050, true},
	}
	for _, tc := range tests {
		t.Run(tc.spec+"/"+string(rune('0'+tc.port%10)), func(t *testing.T) {
			set, err := ParsePorts(tc.spec)
			if err != nil {
				t.Fatalf("ParsePorts(%q): %v", tc.spec, err)
			}
			if got := set.Match(tc.port); got != tc.match {
				t.Errorf("ParsePorts(%q).Match(%d) = %v, want %v", tc.spec, tc.port, got, tc.match)
			}
		})
	}

	for _, bad := range []string{"0", "70000", "443-80", "http", "80,"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("ParsePorts(%q) succeeded, want an error", bad)
		}
	}
}

func TestRulePortScoping(t *testing.T) {
	r, err := ParseRule(Allow, "example.com:443")
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if !r.Match(mustTarget(t, "example.com:443")) {
		t.Error("rule should match its own port")
	}
	if r.Match(mustTarget(t, "example.com:80")) {
		t.Error("rule matched a port it does not list")
	}

	wild, err := ParseRule(Allow, "*.example.com:8000-8100")
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if !wild.Match(mustTarget(t, "a.example.com:8050")) {
		t.Error("wildcard host with a port range should match")
	}
	if wild.Match(mustTarget(t, "a.example.com:9000")) {
		t.Error("wildcard host matched outside its port range")
	}

	v6, err := ParseRule(Deny, "[::1]:8080")
	if err != nil {
		t.Fatalf("ParseRule ipv6: %v", err)
	}
	if !v6.Match(mustTarget(t, "[::1]:8080")) {
		t.Error("bracketed ipv6 rule should match")
	}
	if v6.Match(mustTarget(t, "[::1]:8081")) {
		t.Error("bracketed ipv6 rule matched the wrong port")
	}
}

func TestDenyAlwaysBeatsAllow(t *testing.T) {
	// Both orders, and a broader allow against a narrower deny, because "deny wins" must
	// not depend on rule order or on which rule is more specific.
	cases := []struct {
		name  string
		rules []Rule
	}{
		{"deny first", []Rule{MustRule(Deny, "a.example.com", ""), MustRule(Allow, "a.example.com", "")}},
		{"allow first", []Rule{MustRule(Allow, "a.example.com", ""), MustRule(Deny, "a.example.com", "")}},
		{"narrow deny under broad allow", []Rule{MustRule(Allow, "*.example.com", ""), MustRule(Deny, "a.example.com", "")}},
		{"broad deny under narrow allow", []Rule{MustRule(Allow, "a.example.com", ""), MustRule(Deny, "*.example.com", "")}},
		{"deny all under specific allow", []Rule{MustRule(Allow, "a.example.com", ""), MustRule(Deny, "*", "")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, def := range []Action{Allow, Deny} {
				p := Policy{Name: "test", Default: def, Rules: tc.rules}
				v := p.Evaluate(mustTarget(t, "a.example.com:443"))
				if v.Allowed {
					t.Errorf("default %s: allowed %q, want denied (%s)", def, "a.example.com", v.Reason)
				}
			}
		})
	}
}

func TestDefaultAction(t *testing.T) {
	deny := Policy{Name: "d", Default: Deny}
	if v := deny.Evaluate(mustTarget(t, "example.com:443")); v.Allowed {
		t.Errorf("default-deny allowed an unlisted host: %s", v.Reason)
	} else if !strings.Contains(v.Reason, "denied by default") {
		t.Errorf("reason = %q, want it to mention the default", v.Reason)
	}

	allow := Policy{Name: "a", Default: Allow}
	if v := allow.Evaluate(mustTarget(t, "example.com:443")); !v.Allowed {
		t.Errorf("default-allow denied an unlisted host: %s", v.Reason)
	}
}

func TestEvaluateDenyIgnoresAllowRules(t *testing.T) {
	// The resolved-address check must not be satisfied by an allow rule written for a
	// name: allowing example.com says nothing about the address it resolves to.
	p := Policy{
		Name:    "test",
		Default: Deny,
		Rules: []Rule{
			MustRule(Allow, "*", ""),
			MustRule(Deny, "127.0.0.0/8", ""),
		},
	}
	if v := p.EvaluateDeny(TargetFromAddr(netip.MustParseAddrPort("127.0.0.1:443"))); v.Allowed {
		t.Errorf("loopback passed the resolved-address check: %s", v.Reason)
	}
	if v := p.EvaluateDeny(TargetFromAddr(netip.MustParseAddrPort("203.0.113.5:443"))); !v.Allowed {
		t.Errorf("public address failed the resolved-address check: %s", v.Reason)
	}
}

func TestPresets(t *testing.T) {
	open, err := Preset(PresetOpen)
	if err != nil {
		t.Fatalf("Preset(open): %v", err)
	}
	if v := open.Evaluate(mustTarget(t, "anything.example:443")); !v.Allowed {
		t.Errorf("open denied an ordinary host: %s", v.Reason)
	}
	for _, hostport := range []string{"127.0.0.1:5432", "[::1]:5432", "169.254.169.254:80", "localhost:8080"} {
		if v := open.Evaluate(mustTarget(t, hostport)); v.Allowed {
			t.Errorf("open allowed %s, want it denied: %s", hostport, v.Reason)
		}
	}

	std, err := Preset(PresetStandard)
	if err != nil {
		t.Fatalf("Preset(standard): %v", err)
	}
	if std.Default != Deny {
		t.Error("standard must deny by default")
	}
	if v := std.Evaluate(mustTarget(t, "github.com:443")); !v.Allowed {
		t.Errorf("standard denied github.com: %s", v.Reason)
	}
	// Port scoping: the same host over plaintext is not allowed.
	if v := std.Evaluate(mustTarget(t, "github.com:80")); v.Allowed {
		t.Error("standard allowed plaintext HTTP to github.com")
	}
	for _, host := range []string{"evil.example:443", "raw.githubusercontent.com:443", "storage.googleapis.com:443"} {
		if v := std.Evaluate(mustTarget(t, host)); v.Allowed {
			t.Errorf("standard allowed %s, want denied", host)
		}
	}
	// No preset entry may be a wildcard over a multi-tenant domain, or over anything.
	for _, r := range std.Rules {
		if r.Action == Allow && r.Host.IsAny() {
			t.Errorf("standard contains a catch-all allow: %s", r)
		}
	}

	locked, err := Preset(PresetLocked)
	if err != nil {
		t.Fatalf("Preset(locked): %v", err)
	}
	if v := locked.Evaluate(mustTarget(t, "github.com:443")); v.Allowed {
		t.Error("locked allowed something")
	}

	if _, err := Preset("balanced"); err == nil {
		t.Error("unknown preset accepted")
	} else if !strings.Contains(err.Error(), PresetStandard) {
		t.Errorf("error %q should list the valid presets", err)
	}
}

// TestIPv6IsCoveredFromTheStart: a guest on a real NIC has IPv6 whether anyone planned for
// it or not, so every posture must decide about it explicitly rather than by omission.
func TestIPv6IsCoveredFromTheStart(t *testing.T) {
	for _, name := range []string{PresetStandard, PresetLocked} {
		p := mustPolicy(t, name)
		for _, hostport := range []string{"[2001:db8::1]:443", "[::1]:443", "[fe80::1]:443"} {
			if v := p.Evaluate(mustTarget(t, hostport)); v.Allowed {
				t.Errorf("%s allowed %s: %s", name, hostport, v.Reason)
			}
		}
	}

	open := mustPolicy(t, PresetOpen)
	// The v6 equivalents of the v4 denials must be there, not just the v4 ones.
	for _, hostport := range []string{"[::1]:5432", "[fe80::1]:80"} {
		if v := open.Evaluate(mustTarget(t, hostport)); v.Allowed {
			t.Errorf("open allowed %s: %s", hostport, v.Reason)
		}
	}
	if v := open.Evaluate(mustTarget(t, "[2001:db8::1]:443")); !v.Allowed {
		t.Errorf("open denied ordinary IPv6: %s", v.Reason)
	}

	// A local rule can name a v6 prefix with ports, in the same syntax as v4.
	p, err := Resolve(PresetLocked, []string{"[2001:db8::/32]:443"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v := p.Evaluate(mustTarget(t, "[2001:db8::5]:443")); !v.Allowed {
		t.Errorf("a v6 prefix rule did not match: %s", v.Reason)
	}
	if v := p.Evaluate(mustTarget(t, "[2001:db8::5]:80")); v.Allowed {
		t.Error("a v6 prefix rule ignored its port scope")
	}
}

func TestResolveAppliesLocalRules(t *testing.T) {
	p, err := Resolve(PresetLocked, []string{"example.com:443"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v := p.Evaluate(mustTarget(t, "example.com:443")); !v.Allowed {
		t.Errorf("-allow did not take effect: %s", v.Reason)
	}

	// A local allow cannot undo a preset deny.
	p, err = Resolve(PresetOpen, []string{"127.0.0.1:5432"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v := p.Evaluate(mustTarget(t, "127.0.0.1:5432")); v.Allowed {
		t.Error("-allow overrode a preset deny rule; deny must always win")
	}

	// A local deny narrows a preset allow.
	p, err = Resolve(PresetStandard, nil, []string{"github.com"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v := p.Evaluate(mustTarget(t, "github.com:443")); v.Allowed {
		t.Error("-deny did not override the preset allow")
	}

	if _, err := Resolve(PresetLocked, []string{"not a host"}, nil); err == nil {
		t.Error("Resolve accepted an invalid -allow value")
	}
	if _, err := Resolve(PresetLocked, nil, []string{"*.*.x"}); err == nil {
		t.Error("Resolve accepted an invalid -deny value")
	}
}

func TestEngineLogsEveryDecision(t *testing.T) {
	e := NewEngine(mustPolicy(t, PresetStandard), NewLog(8))

	if d := e.Check(StageConnect, mustTarget(t, "github.com:443")); !d.Allowed {
		t.Fatalf("github.com denied: %s", d.Reason)
	}
	if d := e.Check(StageConnect, mustTarget(t, "evil.example:443")); d.Allowed {
		t.Fatal("evil.example allowed")
	}

	got := e.Log().Recent(0)
	if len(got) != 2 {
		t.Fatalf("recorded %d decisions, want 2", len(got))
	}
	if got[0].Host != "github.com" || !got[0].Allowed || got[0].Stage != StageConnect {
		t.Errorf("first decision = %+v", got[0])
	}
	if got[1].Host != "evil.example" || got[1].Allowed {
		t.Errorf("second decision = %+v", got[1])
	}
	if got[1].Policy != PresetStandard {
		t.Errorf("policy name = %q, want %q", got[1].Policy, PresetStandard)
	}
	if got[1].Reason == "" {
		t.Error("a denial with no reason is not debuggable")
	}
}

func TestLogRingWrapsAndKeepsOrder(t *testing.T) {
	l := NewLog(3)
	for i := 0; i < 5; i++ {
		l.Record(Decision{Time: time.Unix(int64(i), 0), Host: string(rune('a' + i))})
	}
	got := l.Recent(0)
	if len(got) != 3 {
		t.Fatalf("kept %d entries, want 3", len(got))
	}
	want := []string{"c", "d", "e"}
	for i, w := range want {
		if got[i].Host != w {
			t.Errorf("entry %d = %q, want %q", i, got[i].Host, w)
		}
	}
	if last := l.Recent(1); len(last) != 1 || last[0].Host != "e" {
		t.Errorf("Recent(1) = %+v, want the newest entry", last)
	}
}

func TestSinkRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	l := NewLog(4)
	l.AddSink(NewWriterSink(&buf))
	e := NewEngine(mustPolicy(t, PresetLocked), l)
	e = e.WithSandbox("boks-test")
	e.Check(StageHTTP, mustTarget(t, "example.com:80"))

	got, err := ReadDecisions(&buf, 0)
	if err != nil {
		t.Fatalf("ReadDecisions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d decisions, want 1", len(got))
	}
	if got[0].Host != "example.com" || got[0].Allowed || got[0].Sandbox != "boks-test" {
		t.Errorf("decision = %+v", got[0])
	}
}

func TestDescribeMentionsDenyPrecedence(t *testing.T) {
	out := mustPolicy(t, PresetOpen).Describe()
	if !strings.Contains(out, "deny (always wins)") {
		t.Errorf("Describe output does not explain deny precedence:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.0/8") {
		t.Errorf("Describe output omits a rule:\n%s", out)
	}
}

func mustPolicy(t *testing.T, name string) Policy {
	t.Helper()
	p, err := Preset(name)
	if err != nil {
		t.Fatalf("Preset(%q): %v", name, err)
	}
	return p
}
