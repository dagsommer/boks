package policy

import (
	"fmt"
	"sort"
	"strings"
)

// Preset names. Docker Sandboxes offers Open / Balanced / Locked Down; Boks offers the
// same three postures under names that say what they do rather than how they feel.
const (
	// PresetOpen allows anything except the machine Boks is running on.
	PresetOpen = "open"
	// PresetStandard denies by default and allows a small, justified set of package
	// and source hosts.
	PresetStandard = "standard"
	// PresetLocked denies by default with no allowances at all.
	PresetLocked = "locked"
)

// DefaultPreset is what `boks run` uses when nothing is specified.
//
// Standard, not open: a default that is convenient before it is safe is how tools end up
// with a network posture nobody chose. Standard breaks visibly (a denial names the host
// and the flag that would permit it) rather than silently permitting everything.
const DefaultPreset = PresetStandard

// PresetNames lists the presets in increasing order of restriction.
func PresetNames() []string { return []string{PresetOpen, PresetStandard, PresetLocked} }

// Preset returns a named policy by value.
func Preset(name string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case PresetOpen:
		return openPolicy(), nil
	case PresetStandard, "": // empty means the default
		return standardPolicy(), nil
	case PresetLocked:
		return lockedPolicy(), nil
	}
	return Policy{}, fmt.Errorf("unknown network policy %q; choose one of: %s",
		name, strings.Join(PresetNames(), ", "))
}

// PresetDescription is the one-line summary shown in CLI help.
func PresetDescription(name string) string {
	switch name {
	case PresetOpen:
		return "allow everything except this machine's own loopback and link-local addresses"
	case PresetStandard:
		return "deny by default; allow a small set of package registries and source hosts over HTTPS"
	case PresetLocked:
		return "deny everything; every destination must be added with -allow"
	}
	return ""
}

// openPolicy is "the internet is open", which is not the same as "your laptop is open".
//
// Two deny rules survive even here, and both are about the host rather than the internet:
//
//   - loopback. Under the current VM runtime the guest has no NIC at all; libkrun's TSI
//     performs its connections on the host, so the guest's 127.0.0.1 is *the host's*
//     127.0.0.1. Every unauthenticated dev server, database and debug endpoint bound to
//     loopback is inside the sandbox's reach. Nobody choosing "open network" is choosing
//     that.
//   - link-local, which contains 169.254.169.254, the cloud instance metadata endpoint —
//     the single most reliable credential source on a hosted machine.
//
// Both are written for IPv4 and IPv6. IPv6 is not a later addition: TSI supported no IPv6
// at all, so the guest had none, but a real NIC on a host-side stack does — a spike saw the
// guest emit IPv6 MLD reports the moment it had one. The change that closes the loopback
// hole opens an address family, and a rule set that only mentions IPv4 would be half a
// policy.
//
// Private LAN ranges are deliberately *not* denied here: reaching a machine on your own
// network is a plausible thing to want from a policy called "open", and unlike loopback it
// is not an artefact of how the runtime happens to work. Use standard or locked if you
// disagree; deny rules cannot be removed by -allow, so the ones that remain here are the
// ones worth being unable to unsay.
func openPolicy() Policy {
	return Policy{
		Name:    PresetOpen,
		Default: Allow,
		Rules: []Rule{
			MustRule(Deny, "127.0.0.0/8", "the host's own loopback; under TSI the guest shares it"),
			MustRule(Deny, "::1/128", "the host's own loopback (IPv6)"),
			MustRule(Deny, "169.254.0.0/16", "link-local, including the 169.254.169.254 metadata endpoint"),
			MustRule(Deny, "fe80::/10", "link-local (IPv6)"),
			MustRule(Deny, "localhost", "the host's own loopback, by name"),
		},
	}
}

// standardPolicy is the recommended starting posture: deny by default, with a short
// allowlist covering what a coding agent genuinely needs to fetch dependencies and talk to
// a Git host.
//
// Every entry is exact. There are no broad wildcards, and that is the point — Docker's own
// documentation flags entries like *.googleapis.com in its balanced preset as a risk, and
// it is right to. A wildcard over a multi-tenant domain (googleapis.com, amazonaws.com,
// blob.core.windows.net, githubusercontent.com) allows every tenant's bucket, which means
// it allows an exfiltration destination that the attacker controls and you have never
// heard of. If a tool needs one, add it deliberately with -allow and know what you bought.
//
// Everything is restricted to port 443. Allowing port 80 adds a plaintext path to the same
// host, which is a downgrade target for anything that follows redirects; the OS package
// managers that still default to HTTP (apt, apk) are therefore absent, and adding
// `-allow deb.debian.org:80` is an explicit choice rather than a default.
//
// Also absent on purpose:
//   - raw.githubusercontent.com — arbitrary attacker-controlled content, and the fetch half
//     of every `curl … | sh` install. Convenient, so add it when you mean to.
//   - any general-purpose CDN or object store, for the multi-tenancy reason above.
//   - DNS. Traffic through the proxy carries hostnames, and the host resolves them; a
//     sandbox using the proxy needs no DNS allowance of its own. When the netstack lands
//     and raw UDP exists, DNS becomes a covert channel that has to be mediated separately.
func standardPolicy() Policy {
	return Policy{
		Name:    PresetStandard,
		Default: Deny,
		Rules: []Rule{
			MustRule(Allow, "github.com:443", "git clone/fetch/push over HTTPS"),
			MustRule(Allow, "api.github.com:443", "GitHub REST and GraphQL APIs"),
			MustRule(Allow, "codeload.github.com:443", "source archives git and go get fetch"),
			MustRule(Allow, "proxy.golang.org:443", "Go module proxy"),
			MustRule(Allow, "sum.golang.org:443", "Go checksum database"),
			MustRule(Allow, "registry.npmjs.org:443", "npm registry"),
			MustRule(Allow, "pypi.org:443", "PyPI index"),
			MustRule(Allow, "files.pythonhosted.org:443", "PyPI package downloads"),
			MustRule(Allow, "crates.io:443", "crates.io API"),
			MustRule(Allow, "index.crates.io:443", "Cargo sparse index"),
			MustRule(Allow, "static.crates.io:443", "crate downloads"),
		},
	}
}

// lockedPolicy denies everything. It is the right default for running something you have
// no reason to trust at all, and the right starting point for building a minimal allowlist
// by watching `boks policy log` and adding what the workload actually asked for.
func lockedPolicy() Policy {
	return Policy{Name: PresetLocked, Default: Deny}
}

// Describe renders a policy for `boks policy ls`, deny rules first because they are the
// ones that win.
func (p Policy) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "policy %s\n", p.Name)
	fmt.Fprintf(&b, "default: %s\n", p.Default)

	var denies, allows []Rule
	for _, r := range p.Rules {
		if r.Action == Deny {
			denies = append(denies, r)
			continue
		}
		allows = append(allows, r)
	}
	section := func(title string, rules []Rule, empty string) {
		fmt.Fprintf(&b, "\n%s\n", title)
		if len(rules) == 0 {
			fmt.Fprintf(&b, "  (%s)\n", empty)
			return
		}
		width := 0
		for _, r := range rules {
			if n := len(r.Spec()); n > width {
				width = n
			}
		}
		specs := make([]string, 0, len(rules))
		byspec := map[string]Rule{}
		for _, r := range rules {
			specs = append(specs, r.Spec())
			byspec[r.Spec()] = r
		}
		sort.Strings(specs)
		for _, s := range specs {
			r := byspec[s]
			if r.Why == "" {
				fmt.Fprintf(&b, "  %s\n", s)
				continue
			}
			fmt.Fprintf(&b, "  %-*s  %s\n", width, s, r.Why)
		}
	}
	section("deny (always wins):", denies, "none")
	section("allow:", allows, "none")
	return b.String()
}
