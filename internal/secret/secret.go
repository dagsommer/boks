// Package secret keeps credentials on the host and attaches them to outbound requests.
//
// The principle, borrowed from Docker Sandboxes: **the real secret never enters the
// guest.** The guest holds a placeholder — or nothing at all — and the host forward proxy
// replaces it on requests to destinations you configured by hand. A compromised agent can
// therefore *use* a credential against approved hosts while the sandbox runs, but cannot
// keep it, print it, or send it anywhere else. That is a reduction in damage, not an
// elimination, and this package does not pretend otherwise.
//
// # Invariants
//
//   - There is no host API a guest can call to enumerate, request or unseal a secret. The
//     only consumer of a Provider is the proxy's request path, running on the host, and
//     nothing in the guest can address it. Do not add a lookup endpoint.
//   - Injection is keyed on the destination host only, never on request content. A guest
//     cannot ask for a credential by naming it.
//   - A rule's host pattern may not be a catch-all. Sending a token to "wherever this
//     request happens to be going" is the whole failure mode this package exists to avoid.
//   - Values are wrapped in Value, whose String, GoString and JSON forms are redacted, so
//     a log line or a %+v on a config struct cannot leak one. There is a test for this.
//
// # Known limit: HTTPS
//
// Injection needs to see the request. Boks does not intercept TLS — no custom CA in the
// guest, end-to-end TLS preserved, the proxy cannot read bodies — and that constraint has a
// direct consequence: **injection applies only to requests the proxy can read, which today
// means plaintext HTTP.** Inside a CONNECT tunnel the proxy sees ciphertext and cannot add
// a header without becoming a man in the middle.
//
// This is a real gap against Docker Sandboxes, which does inject into HTTPS requests and
// must therefore terminate TLS somewhere. Closing it means choosing to terminate TLS for
// specific configured hosts, which is a deliberate MITM and a decision the user has to make
// explicitly, not a convenience Boks helps itself to. Until that decision is made, a rule
// on an HTTPS destination will simply never fire, and the CLI says so.
package secret

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dagsommer/boks/internal/policy"
)

// ErrNotFound is returned by a Provider when no secret has that name.
var ErrNotFound = errors.New("secret not found")

// Value holds a credential. Its textual forms are redacted; Reveal is the only way out,
// and it is spelled loudly so that a reviewer can grep for every place a secret is
// unwrapped.
type Value struct {
	v string
}

// NewValue wraps a raw credential.
func NewValue(s string) Value { return Value{v: s} }

// Reveal returns the raw credential. Call it only where the value is about to be used.
func (v Value) Reveal() string { return v.v }

// IsZero reports whether the value is empty.
func (v Value) IsZero() bool { return v.v == "" }

// Redacted is what a Value renders as anywhere a string is expected.
const Redacted = "[redacted]"

func (v Value) String() string { return Redacted }

// GoString covers %#v, which would otherwise print the struct field verbatim.
func (v Value) GoString() string { return "secret.Value(" + Redacted + ")" }

// MarshalText covers encoding/json and anything else that reaches for a text form. It
// redacts rather than failing: a config dump should print something, just not the secret.
func (v Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// Provider supplies credentials by name. Implementations run on the host only.
//
// The interface is deliberately this small so that an OS keychain — macOS Keychain, Linux
// Secret Service, Windows Credential Manager — can be added later without touching the
// injection path. None of those are implemented yet.
type Provider interface {
	Lookup(ctx context.Context, name string) (Value, error)
}

// MapProvider is an in-memory Provider for tests and for wiring experiments.
type MapProvider map[string]string

// Lookup implements Provider.
func (m MapProvider) Lookup(_ context.Context, name string) (Value, error) {
	v, ok := m[name]
	if !ok {
		return Value{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return NewValue(v), nil
}

// Scheme is how a credential is attached to a request.
type Scheme string

const (
	// SchemeBearer sets Authorization: Bearer <secret>.
	SchemeBearer Scheme = "bearer"
	// SchemeHeader sets an arbitrary header to the secret, for API-key styles such as
	// x-api-key or PRIVATE-TOKEN.
	SchemeHeader Scheme = "header"
	// SchemeBasic sets Authorization: Basic base64(username:secret). This is how Git
	// over HTTPS and most container registries take a token.
	SchemeBasic Scheme = "basic"
)

// Rule attaches one secret to requests for one set of destinations.
//
// Nothing here names a vendor. A GitHub token is basic auth with a username; an Anthropic
// key is an x-api-key header; a registry token is a bearer. The same three primitives
// cover all of them, and adding a fourth vendor should require no code.
type Rule struct {
	// Hosts are the destinations this credential may be sent to. At least one is
	// required and none may be a catch-all.
	Hosts []policy.Pattern
	// Secret is the name passed to the Provider.
	Secret string
	Scheme Scheme
	// Header is the header name for SchemeHeader.
	Header string
	// Username is the user part for SchemeBasic. Git hosts conventionally ignore it or
	// want a fixed value such as "x-access-token".
	Username string
}

// ParseRule builds a Rule from a compact CLI form:
//
//	host[,host...]=name:scheme[:extra]
//
// where extra is the header name for "header" and the username for "basic". Examples:
//
//	api.anthropic.com=anthropic:header:x-api-key
//	github.com,api.github.com=gh:basic:x-access-token
//	registry.example.com=reg:bearer
func ParseRule(spec string) (Rule, error) {
	hostsPart, rest, ok := strings.Cut(spec, "=")
	if !ok {
		return Rule{}, fmt.Errorf("credential rule %q: expected host=name:scheme", spec)
	}
	fields := strings.Split(rest, ":")
	if len(fields) < 2 || len(fields) > 3 {
		return Rule{}, fmt.Errorf("credential rule %q: expected name:scheme[:extra] after '='", spec)
	}
	r := Rule{Secret: strings.TrimSpace(fields[0]), Scheme: Scheme(strings.ToLower(strings.TrimSpace(fields[1])))}
	if len(fields) == 3 {
		extra := strings.TrimSpace(fields[2])
		switch r.Scheme {
		case SchemeHeader:
			r.Header = extra
		case SchemeBasic:
			r.Username = extra
		default:
			return Rule{}, fmt.Errorf("credential rule %q: scheme %q takes no third field", spec, r.Scheme)
		}
	}
	for _, h := range strings.Split(hostsPart, ",") {
		p, err := policy.ParsePattern(h)
		if err != nil {
			return Rule{}, fmt.Errorf("credential rule %q: %w", spec, err)
		}
		r.Hosts = append(r.Hosts, p)
	}
	if err := r.Validate(); err != nil {
		return Rule{}, fmt.Errorf("credential rule %q: %w", spec, err)
	}
	return r, nil
}

// Validate rejects rules that would send a credential somewhere it should not go.
func (r Rule) Validate() error {
	if r.Secret == "" {
		return errors.New("no secret name")
	}
	if len(r.Hosts) == 0 {
		return errors.New("no destination hosts; a credential must be scoped to hosts you name")
	}
	for _, h := range r.Hosts {
		if h.IsAny() {
			return errors.New("'*' is not allowed as a credential destination; name the hosts that may receive this secret")
		}
	}
	switch r.Scheme {
	case SchemeBearer:
	case SchemeBasic:
	case SchemeHeader:
		if r.Header == "" {
			return errors.New("scheme \"header\" needs a header name, as in name:header:x-api-key")
		}
	default:
		return fmt.Errorf("unknown scheme %q; use bearer, basic or header", r.Scheme)
	}
	return nil
}

// Match reports whether the rule applies to a destination.
func (r Rule) Match(t policy.Target) bool {
	for _, h := range r.Hosts {
		if h.Match(t) {
			return true
		}
	}
	return false
}

func (r Rule) String() string {
	hosts := make([]string, 0, len(r.Hosts))
	for _, h := range r.Hosts {
		hosts = append(hosts, h.String())
	}
	out := strings.Join(hosts, ",") + "=" + r.Secret + ":" + string(r.Scheme)
	switch r.Scheme {
	case SchemeHeader:
		out += ":" + r.Header
	case SchemeBasic:
		if r.Username != "" {
			out += ":" + r.Username
		}
	}
	return out
}

// Injector applies credential rules to outbound requests.
type Injector struct {
	provider Provider
	rules    []Rule
}

// NewInjector validates the rules and binds them to a provider.
func NewInjector(p Provider, rules ...Rule) (*Injector, error) {
	if p == nil && len(rules) > 0 {
		return nil, errors.New("credential rules were given but no secret provider is configured")
	}
	for _, r := range rules {
		if err := r.Validate(); err != nil {
			return nil, err
		}
	}
	return &Injector{provider: p, rules: rules}, nil
}

// Rules returns the configured rules, for `boks policy ls`-style display. Rules name
// secrets; they never carry values.
func (i *Injector) Rules() []Rule {
	if i == nil {
		return nil
	}
	return append([]Rule(nil), i.rules...)
}

// Apply sets the credential headers for t on h, and reports the names of the secrets it
// used so the caller can log *that* an injection happened without logging what.
//
// Existing headers are overwritten. That is the point: the guest holds a placeholder, and
// a placeholder that survived to the wire would be a silent authentication failure at best
// and a leaked placeholder at worst.
func (i *Injector) Apply(ctx context.Context, t policy.Target, h http.Header) ([]string, error) {
	if i == nil || len(i.rules) == 0 {
		return nil, nil
	}
	var used []string
	for _, r := range i.rules {
		if !r.Match(t) {
			continue
		}
		v, err := i.provider.Lookup(ctx, r.Secret)
		if err != nil {
			// Name the secret, never the value, and never the provider's raw error
			// in case a provider is ever careless with it.
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("secret %q is configured for %s but is not in the store; add it with 'boks secret set %s'", r.Secret, t.Host, r.Secret)
			}
			return nil, fmt.Errorf("looking up secret %q for %s: %w", r.Secret, t.Host, err)
		}
		if v.IsZero() {
			return nil, fmt.Errorf("secret %q is empty", r.Secret)
		}
		switch r.Scheme {
		case SchemeBearer:
			h.Set("Authorization", "Bearer "+v.Reveal())
		case SchemeBasic:
			creds := r.Username + ":" + v.Reveal()
			h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
		case SchemeHeader:
			h.Set(r.Header, v.Reveal())
		default:
			return nil, fmt.Errorf("unknown credential scheme %q", r.Scheme)
		}
		used = append(used, r.Secret)
	}
	return used, nil
}
