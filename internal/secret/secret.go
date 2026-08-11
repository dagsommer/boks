// Package secret keeps credentials on the host and attaches them to outbound requests.
//
// The principle, borrowed from Docker Sandboxes: **the real secret never enters the
// guest.** The guest holds a placeholder — or nothing at all — and the host forward proxy
// replaces it on requests to destinations you configured by hand. A compromised agent can
// therefore *use* a credential against approved hosts while the sandbox runs, but cannot
// keep it, print it, or send it anywhere else. That is a reduction in damage, not an
// elimination, and this package does not pretend otherwise.
//
// # One credential, many injection rules
//
// A credential names a service and owns a set of injection rules, each saying which domain
// it applies to and how the value is attached. The indirection is the point: several hosts
// routinely share one secret — an enterprise Git host and its API endpoint, a package feed
// and its mirror — and a flat host-to-scheme map forces you to repeat the scheme, which is
// how a rotation ends up applied to three places out of four.
//
// The shape follows the credential grammar Docker Sandboxes' kits use (`credentials[]`
// with a per-credential `inject[]` array), so a kit loader can be written later without
// reworking this model.
//
// Attachment is a header name plus a value format with exactly one %s:
//
//	Authorization: "Bearer %s"      a model API or registry token
//	x-api-key:     "%s"             an API-key header
//	Authorization: "token %s"       whatever a vendor invented this year
//
// `bearer` and `basic` exist as shorthands for the two common cases, and are mutually
// exclusive with a format string. Basic auth *with* a username is the one attachment that
// is not the secret itself, because the value is base64("user:secret").
//
// Not every credential mechanism is a single header, and this model does not assume one.
// An interactive OAuth flow — sentinel access and refresh tokens written into a guest
// credential file and swapped by the proxy on requests to a set of resource hosts — is a
// second mechanism that belongs on Credential beside Inject, not inside an injection rule.
// Boks does not implement it.
//
// # Invariants
//
//   - There is no host API a guest can call to enumerate, request or unseal a secret. The
//     only consumer of a Provider is the proxy's request path, running on the host, and
//     nothing in the guest can address it. Do not add a lookup endpoint.
//   - Injection is keyed on the destination host only, never on request content. A guest
//     cannot ask for a credential by naming it.
//   - An injection rule's domain may not be a catch-all. Sending a token to "wherever this
//     request happens to be going" is the whole failure mode this package exists to avoid.
//   - Being allowed to reach a host and being eligible for a credential are separate
//     decisions, held in separate places: the network policy says what is reachable, the
//     injection rules here say what receives a secret. A host can be reachable and never
//     be sent anything, which is the common case and the safe default.
//   - Values are wrapped in Value, whose String, GoString and JSON forms are redacted, so
//     a log line or a %+v on a config struct cannot leak one. There is a test for this.
//
// # HTTPS costs an interception
//
// Injection needs to see the request, and an HTTPS request is only visible to something
// that terminates the TLS session. Every credential worth injecting — model APIs, Git
// hosts, registries — is HTTPS-only, so "no interception, ever" and "credential injection"
// cannot both be true. Boks resolves that by terminating TLS **only for domains an
// injection rule names**, with a locally generated CA (internal/ca); every other flow stays
// a blind tunnel. The consequences are set out in internal/proxy and
// docs/security-model.md, and they are real: for those hosts Boks can read the request and
// the response.
//
// That is why injection domains must be written out and why a catch-all is rejected. A
// catch-all rule would not merely send a token to the wrong place; under this design it
// would also decide to decrypt everything.
package secret

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
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

// Scheme is a shorthand for the two attachment forms common enough to have names.
type Scheme string

const (
	// SchemeBearer sets Authorization: Bearer <secret>.
	SchemeBearer Scheme = "bearer"
	// SchemeBasic sets Authorization: Basic base64(<username>:<secret>).
	SchemeBasic Scheme = "basic"
)

// DefaultHeader is the header an injection rule uses when none is named.
const DefaultHeader = "Authorization"

// Inject is one rule: on requests to this domain, set this header to this value.
type Inject struct {
	// Domain is the destination pattern this rule applies to. Never a catch-all.
	Domain policy.Pattern
	// Header is the header to set. Empty means Authorization.
	Header string
	// Format is the header value with exactly one %s where the secret goes. Mutually
	// exclusive with Scheme.
	Format string
	// Scheme is the shorthand form. Mutually exclusive with Format.
	Scheme Scheme
	// Username is the user part for basic auth, and is meaningless otherwise.
	Username string
}

// Validate rejects rules that could not attach a credential correctly, or that could
// attach one somewhere it does not belong.
func (r Inject) Validate() error {
	if r.Domain.String() == "" {
		return errors.New("no injection domain")
	}
	if r.Domain.IsAny() {
		return errors.New("'*' is not allowed as an injection domain; name the hosts that may receive this secret, because those are also the hosts whose TLS boks will terminate")
	}
	if r.Format != "" && r.Scheme != "" {
		return fmt.Errorf("domain %s sets both a format and a scheme; they are alternatives", r.Domain)
	}
	if r.Scheme != "" {
		if r.Scheme != SchemeBearer && r.Scheme != SchemeBasic {
			return fmt.Errorf("unknown scheme %q; use bearer or basic, or write a format such as \"Bearer %%s\"", r.Scheme)
		}
		if r.Username != "" && r.Scheme != SchemeBasic {
			return fmt.Errorf("a username is only meaningful for basic auth, not for %q", r.Scheme)
		}
		return validHeaderName(r.header())
	}
	if r.Username != "" {
		return errors.New("a username is only meaningful with the basic scheme")
	}
	format := r.effectiveFormat()
	if n := strings.Count(format, "%s"); n != 1 {
		return fmt.Errorf("value format %q must contain exactly one %%s, where the secret goes", format)
	}
	if strings.Count(format, "%") != 1 {
		return fmt.Errorf("value format %q may use %%s and no other %% verb", format)
	}
	if strings.ContainsAny(format, "\r\n") {
		return fmt.Errorf("value format %q contains a newline, which would let one header become two", format)
	}
	return validHeaderName(r.header())
}

func (r Inject) header() string {
	if r.Header != "" {
		return r.Header
	}
	return DefaultHeader
}

func (r Inject) effectiveFormat() string {
	switch r.Scheme {
	case SchemeBearer:
		return "Bearer %s"
	case SchemeBasic:
		return "Basic %s"
	}
	if r.Format == "" {
		return "%s"
	}
	return r.Format
}

// headerValue renders the header value for a secret. It is the only place a revealed
// secret is combined with anything, and what it returns is never logged.
func (r Inject) headerValue(v Value) string {
	raw := v.Reveal()
	if r.Scheme == SchemeBasic {
		raw = base64.StdEncoding.EncodeToString([]byte(r.Username + ":" + raw))
	}
	return fmt.Sprintf(r.effectiveFormat(), raw)
}

func (r Inject) String() string {
	out := r.Domain.String() + " → " + r.header() + ": " + r.effectiveFormat()
	if r.Scheme == SchemeBasic && r.Username != "" {
		out += "  (user " + r.Username + ")"
	}
	return out
}

// Credential is one service's secret and everywhere it may be attached.
type Credential struct {
	// Service names the credential. It is also the key in the secret store.
	Service string
	// Description is free text for `boks policy ls`.
	Description string
	// EnvName is the environment variable a guest reads this credential from. Nothing in
	// Boks writes a guest's environment yet; it is recorded so that whatever does will
	// not have to invent the mapping.
	EnvName string
	// ProxyManaged marks a credential the proxy supplies, so the value in the guest is a
	// placeholder rather than a real secret. It is stated explicitly rather than inferred
	// from "there is an injection rule", because the difference between a guest holding a
	// real token and a guest holding a sentinel is exactly the property this package
	// exists to keep true, and it should be legible in the configuration.
	ProxyManaged bool
	// Placeholder is the fake value the guest is given.
	//
	// It must look real. Clients validate credential format locally — `gh` checks that a
	// token has a known prefix, cloud SDKs check lengths — so a marker like "boks-managed"
	// makes those tools fail before a request ever reaches the proxy. The placeholder
	// therefore belongs to the credential, where whoever knows the service can shape it,
	// rather than being one constant Boks picks for everything.
	Placeholder string
	// Inject lists where and how the secret is attached.
	Inject []Inject
}

// Validate checks a credential and all of its rules.
func (c Credential) Validate() error {
	if c.Service == "" {
		return errors.New("no service name")
	}
	if len(c.Inject) == 0 {
		return fmt.Errorf("credential %q has no injection rules; a credential with nowhere to go is a secret with no purpose", c.Service)
	}
	if strings.ContainsAny(c.Placeholder, "\r\n") {
		return fmt.Errorf("credential %q: the placeholder contains a newline", c.Service)
	}
	for _, r := range c.Inject {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("credential %q: %w", c.Service, err)
		}
	}
	return nil
}

// Domains lists the destinations this credential may be sent to.
func (c Credential) Domains() []string {
	out := make([]string, 0, len(c.Inject))
	for _, r := range c.Inject {
		out = append(out, r.Domain.String())
	}
	return out
}

func (c Credential) String() string {
	var b strings.Builder
	b.WriteString(c.Service)
	if c.ProxyManaged {
		b.WriteString(" (proxy-managed)")
	}
	for _, r := range c.Inject {
		b.WriteString("\n    " + r.String())
	}
	return b.String()
}

// ParseInject builds — or extends — a credential from the compact CLI form:
//
//	service@host[,host...]=spec
//
// where spec is one of:
//
//	bearer               Authorization: Bearer <secret>
//	basic[:username]     Authorization: Basic base64(username:<secret>)
//	header-name          <header-name>: <secret>
//	header-name:format   <header-name>: format with one %s
//
// Examples:
//
//	anthropic@api.anthropic.com=x-api-key
//	gh@github.com,api.github.com=basic:x-access-token
//	registry@registry.example.com=bearer
//	odd@api.example.com=Authorization:token %s
func ParseInject(spec string) (service string, rules []Inject, err error) {
	service, rest, ok := strings.Cut(spec, "@")
	if !ok {
		return "", nil, fmt.Errorf("injection rule %q: expected service@host[,host]=spec", spec)
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return "", nil, fmt.Errorf("injection rule %q: no service name before '@'", spec)
	}
	hostsPart, attach, ok := strings.Cut(rest, "=")
	if !ok {
		return "", nil, fmt.Errorf("injection rule %q: expected service@host[,host]=spec", spec)
	}

	template, err := parseAttachment(strings.TrimSpace(attach))
	if err != nil {
		return "", nil, fmt.Errorf("injection rule %q: %w", spec, err)
	}
	for _, h := range strings.Split(hostsPart, ",") {
		p, perr := policy.ParsePattern(h)
		if perr != nil {
			return "", nil, fmt.Errorf("injection rule %q: %w", spec, perr)
		}
		rule := template
		rule.Domain = p
		if verr := rule.Validate(); verr != nil {
			return "", nil, fmt.Errorf("injection rule %q: %w", spec, verr)
		}
		rules = append(rules, rule)
	}
	return service, rules, nil
}

// parseAttachment turns the part after '=' into a rule without a domain.
func parseAttachment(attach string) (Inject, error) {
	if attach == "" {
		return Inject{}, errors.New("no attachment; use bearer, basic[:user], a header name, or header:format")
	}
	head, tail, hasTail := strings.Cut(attach, ":")
	switch strings.ToLower(strings.TrimSpace(head)) {
	case string(SchemeBearer):
		if hasTail {
			return Inject{}, errors.New("\"bearer\" takes no further field")
		}
		return Inject{Scheme: SchemeBearer}, nil
	case string(SchemeBasic):
		return Inject{Scheme: SchemeBasic, Username: strings.TrimSpace(tail)}, nil
	}
	// A header name, optionally followed by a format. The format is not trimmed: it may
	// legitimately contain a colon or end in a space.
	r := Inject{Header: strings.TrimSpace(head)}
	if hasTail {
		r.Format = tail
	}
	return r, nil
}

// ParseCredentials assembles a set of credentials from the two CLI spellings: the
// injection rules and the placeholders the guest holds instead of a secret.
//
// It lives here rather than in the command layer because two different processes build the
// same credentials from the same strings — the CLI, to validate them and to tell the user
// which hosts will be decrypted, and the process that runs the proxy, which receives the
// specs verbatim. Parsing them in one place is what keeps those two from disagreeing about
// what the user asked for.
//
// Rules for one service accumulate onto one credential, which is the point of the two-level
// model: four hosts sharing one enterprise token are four rules and one secret.
func ParseCredentials(inject, guest []string) ([]Credential, error) {
	var order []string
	byService := map[string]*Credential{}
	for _, spec := range inject {
		service, rules, err := ParseInject(spec)
		if err != nil {
			return nil, err
		}
		if _, ok := byService[service]; !ok {
			byService[service] = &Credential{Service: service}
			order = append(order, service)
		}
		byService[service].Inject = append(byService[service].Inject, rules...)
	}
	for _, spec := range guest {
		service, env, placeholder, err := ParseGuestCredential(spec)
		if err != nil {
			return nil, err
		}
		c, ok := byService[service]
		if !ok {
			return nil, fmt.Errorf("-guest-credential %s: no -inject rule mentions service %q, so nothing would ever replace that placeholder", spec, service)
		}
		c.EnvName, c.Placeholder, c.ProxyManaged = env, placeholder, true
	}
	out := make([]Credential, 0, len(order))
	for _, name := range order {
		out = append(out, *byService[name])
	}
	return out, nil
}

// ParseGuestCredential fills in the guest-side half of a credential:
//
//	service=placeholder
//	service=ENV_NAME=placeholder
//
// Setting either marks the credential proxy-managed: the guest holds a stand-in and the
// host supplies the real value.
func ParseGuestCredential(spec string) (service, env, placeholder string, err error) {
	service, rest, ok := strings.Cut(spec, "=")
	if !ok {
		return "", "", "", fmt.Errorf("guest credential %q: expected service=[ENV_NAME=]placeholder", spec)
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return "", "", "", fmt.Errorf("guest credential %q: no service name", spec)
	}
	if name, value, ok := strings.Cut(rest, "="); ok {
		env, placeholder = strings.TrimSpace(name), value
	} else {
		placeholder = rest
	}
	if placeholder == "" {
		return "", "", "", fmt.Errorf("guest credential %q: no placeholder value; it should look like a real credential for that service, or the guest's own client will reject it before boks sees the request", spec)
	}
	return service, env, placeholder, nil
}

// Injector applies credentials to outbound requests.
type Injector struct {
	provider    Provider
	credentials []Credential
}

// NewInjector validates the credentials and binds them to a provider.
func NewInjector(p Provider, credentials ...Credential) (*Injector, error) {
	if p == nil && len(credentials) > 0 {
		return nil, errors.New("credentials were configured but no secret provider is available")
	}
	seen := map[string]bool{}
	for _, c := range credentials {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if seen[c.Service] {
			return nil, fmt.Errorf("credential %q is defined twice", c.Service)
		}
		seen[c.Service] = true
	}
	return &Injector{provider: p, credentials: credentials}, nil
}

// Credentials returns the configured credentials, sorted by service, for display. They name
// secrets; they never carry values.
func (i *Injector) Credentials() []Credential {
	if i == nil {
		return nil
	}
	out := append([]Credential(nil), i.credentials...)
	sort.Slice(out, func(a, b int) bool { return out[a].Service < out[b].Service })
	return out
}

// Handles reports whether any injection rule applies to t.
//
// The proxy asks this *before* a request exists, to decide whether a TLS flow has to be
// terminated at all. Keeping the question here means one definition of "this host is
// configured for credentials" governs both injection and interception, and the two cannot
// drift apart into a host that gets decrypted for no reason.
func (i *Injector) Handles(t policy.Target) bool {
	if i == nil {
		return false
	}
	for _, c := range i.credentials {
		for _, r := range c.Inject {
			if r.Domain.Match(t) {
				return true
			}
		}
	}
	return false
}

// Hosts lists every domain that could receive a credential — which is exactly the set of
// hosts whose traffic Boks will decrypt. Sorted and deduplicated.
func (i *Injector) Hosts() []string {
	if i == nil {
		return nil
	}
	return CredentialHosts(i.credentials)
}

// CredentialHosts is Hosts for credentials that have not been bound to a provider yet,
// which is the shape the CLI has when it prints what a run would do.
func CredentialHosts(credentials []Credential) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range credentials {
		for _, d := range c.Domains() {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Placeholders returns the stand-in values a guest should be given, keyed by the
// environment variable name where one was configured and by the service otherwise.
//
// Nothing consumes this yet: no part of Boks writes into a guest's environment. It is here
// because the placeholder is a property of the credential, and because a guest with no
// placeholder at all fails differently — and less obviously — than one holding a
// well-shaped fake.
func (i *Injector) Placeholders() map[string]string {
	if i == nil {
		return nil
	}
	out := map[string]string{}
	for _, c := range i.credentials {
		if c.Placeholder == "" {
			continue
		}
		key := c.EnvName
		if key == "" {
			key = c.Service
		}
		out[key] = c.Placeholder
	}
	return out
}

// Apply sets the credential headers for t on h, and reports the services whose secrets it
// used so the caller can log *that* an injection happened without logging what.
//
// Existing headers are overwritten. That is the point: the guest holds a placeholder, and
// a placeholder that survived to the wire would be a silent authentication failure at best
// and a leaked placeholder at worst.
func (i *Injector) Apply(ctx context.Context, t policy.Target, h http.Header) ([]string, error) {
	if i == nil || len(i.credentials) == 0 {
		return nil, nil
	}
	var used []string
	for _, c := range i.credentials {
		matched := false
		for _, r := range c.Inject {
			if !r.Domain.Match(t) {
				continue
			}
			if !matched {
				matched = true
			}
			v, err := i.provider.Lookup(ctx, c.Service)
			if err != nil {
				// Name the secret, never the value, and never the provider's raw error
				// in case a provider is ever careless with it.
				if errors.Is(err, ErrNotFound) {
					return nil, fmt.Errorf("secret %q is configured for %s but is not in the store; add it with 'boks secret set %s'", c.Service, t.Host, c.Service)
				}
				return nil, fmt.Errorf("looking up secret %q for %s: %w", c.Service, t.Host, err)
			}
			if v.IsZero() {
				return nil, fmt.Errorf("secret %q is empty", c.Service)
			}
			h.Set(r.header(), r.headerValue(v))
		}
		if matched {
			used = append(used, c.Service)
		}
	}
	return used, nil
}

// validHeaderName rejects anything that could split a header or forge a second one.
func validHeaderName(name string) error {
	if name == "" {
		return errors.New("no header name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("-_.", c) >= 0
		if !ok {
			return fmt.Errorf("header name %q contains %q, which is not valid in a header name", name, string(rune(c)))
		}
	}
	return nil
}
