package secret

// The service registry: a name the user already knows, and everything Boks needs to carry
// that vendor's credential.
//
// # Why this exists
//
// Storing an API key used to require knowing the vendor's header name:
//
//	boks secret set anth
//	boks run claude --inject 'anth@api.anthropic.com=x-api-key' \
//	                --guest-credential 'anth=ANTHROPIC_API_KEY=sk-ant-placeholder'
//
// Docker Sandboxes asks for none of that — `sbx secret set -g anthropic` is the whole
// ceremony — because it has a fixed list of known services and already knows what each one
// means. The grammar above is strictly more expressive and much worse to use, and nobody
// should have to know Anthropic's header name to put a key in a sandbox.
//
// So a service is **data**, in the shape internal/agent already uses: a struct, an ordered
// registry, and an Add seam a user-defined entry will arrive through. `--inject` stays for
// everything that is not on the list, and still overrides an entry that is.
//
// # What may go in a row, and what may not
//
// The bar is the one internal/agent sets for an allowlist: vendor documentation, cited in the
// entry, or nothing. **A guessed header is worse than an absent one.** An absent one produces
// "boks knows this name but has no rule for it", which names the gap and can be worked around
// in the next line of the terminal. A guessed one produces a request the origin rejects for a
// reason nobody can see — the header is on the wire, it is simply the wrong header — and a
// placeholder that should have been replaced travels to the real API instead.
//
// The same bar applies to the *host*, and it is the one that disqualified two entries here.
// The destination decides both where a credential is attached and whose TLS is terminated, so
// a documented header sent to an undocumented host is not a partial success: the credential is
// never attached, and the guest's fake goes out in its place.
//
// A service that could not be sourced is therefore registered **with its name and no rule**,
// exactly as `kiro` is registered as an agent with no image. Asking for it says what is
// missing rather than "unknown service".
//
// # The placeholder is not decoration
//
// Every configured row carries the observable prefix and length of a real key for that
// vendor, because the guest holds a *fake* and clients validate credential format locally:
// `gh` checks a token's prefix, SDKs check lengths, Claude Code refuses an OAuth token that
// does not start with `sk-ant-oat01-`. All of that happens inside the guest, before any
// request reaches the proxy that would have replaced the value — so a placeholder spelled
// "boks-managed" breaks the tool invisibly. See NewSentinel, which mints them.
//
// Prefixes below are documented; lengths mostly are not, and the comments say which is which.
// That asymmetry is fine and worth stating plainly: a prefix is a fact about the wire, and
// getting it wrong breaks a real client. A length only shapes a fake, and the worst a wrong
// one can do is fail a local check that a documented prefix has already passed.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ServiceInject is one attachment rule in a service definition: a set of hosts, and how the
// credential rides on a request to them.
//
// It is a list rather than a single rule because one credential legitimately travels two ways.
// A GitHub token is basic-auth on a Git fetch and a bearer token on the REST API, and the
// difference is per host, not per service.
type ServiceInject struct {
	// Hosts are the destinations this rule covers — and therefore exactly the hosts whose
	// TLS Boks terminates for it. Bare hostnames: an injection domain has no port
	// dimension, because interception is decided per host.
	Hosts []string
	// Header carries the credential. Empty means Authorization.
	Header string
	// Format is the header value with exactly one %s. Empty means the bare secret.
	// Mutually exclusive with Scheme.
	Format string
	// Scheme is the bearer/basic shorthand. Mutually exclusive with Format.
	Scheme Scheme
	// Username is the basic-auth user part, which for a Git host is a fixed string the
	// vendor documents rather than the person's login.
	Username string
	// Why says what this rule is for, in the same spirit as an agent allow rule's Why.
	Why string
}

// Service is one vendor's credential arrangement: where its key goes, in which header, what
// the guest holds instead of it, and — where the vendor documents one Boks could actually
// use — the OAuth token endpoint, so an OAuth credential for the service needs no further
// configuration.
type Service struct {
	// Name is what the user types, and the key the credential is stored under.
	Name string
	// Summary is one line for help output and `boks secret ls`.
	Summary string

	// Inject is where the credential goes and how it is attached. An empty Inject means
	// Boks knows this name and nothing else — see Configured.
	Inject []ServiceInject
	// Ports is the port list the *allow-rule hint* names — "443" everywhere, because every
	// API here is HTTPS-only and allowing port 80 beside it would add a plaintext
	// downgrade path nobody asked for. It never reaches an injection rule.
	Ports string

	// EnvName is the environment variable the guest's own client reads the credential
	// from. It is what makes `boks secret set <service>` enough on its own: the guest is
	// given a placeholder there, and its tooling finds a credential where it looked.
	EnvName string

	// KeyPrefix and KeyLength are the observable shape of a real key for this vendor, used
	// to mint a placeholder a client's local format check will accept. KeyLength counts
	// the prefix.
	KeyPrefix string
	KeyLength int

	// TokenEndpoint is the vendor's OAuth token endpoint, recorded only where the vendor
	// documents one that accepts the refresh-token grant Boks actually performs. A
	// token-exchange endpoint for workload-identity federation is *not* that, and
	// recording one here would produce a refresh request no endpoint understands.
	TokenEndpoint Endpoint
	// TokenEncoding is how a refresh request to that endpoint is serialised.
	TokenEncoding Encoding
	// OAuthProfile names the credential document `boks secret adopt` can read an OAuth
	// credential for this service out of, when one exists. See import.go.
	OAuthProfile string

	// Source cites where the rest of this row was read from. It is required for a
	// configured service, in the same spirit as the Why on an agent's allow rule: a rule
	// nobody can check is a rule nobody can correct.
	Source string
}

// Configured reports whether Boks knows how to attach this service's credential. A service
// that is registered but not configured is a name Boks recognises and cannot act on.
func (s Service) Configured() bool { return len(s.Inject) > 0 }

// HasOAuth reports whether the vendor's token endpoint is known, so that an OAuth credential
// for this service needs no further configuration.
func (s Service) HasOAuth() bool { return s.TokenEndpoint.Host != "" }

// Hosts lists every destination this service's credential may be attached to, in definition
// order and deduplicated. It is also exactly the set of hosts Boks decrypts for it.
func (s Service) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, rule := range s.Inject {
		for _, h := range rule.Hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// ports is the port list an allow-rule hint uses. HTTPS, unless a row says otherwise.
func (s Service) ports() string {
	if s.Ports == "" {
		return "443"
	}
	return s.Ports
}

// Placeholder is the fake the guest holds: this vendor's real prefix, a plausible length, and
// a body that says "boks" unmistakably if it ever escapes.
//
// It is derived rather than stored so that the shape and the fake cannot drift apart, and it
// is deterministic in the service name so that a sandbox restarted tomorrow presents the same
// value it presented today — an agent that copied the placeholder into a config file of its
// own must not find a different one next time.
func (s Service) Placeholder() string {
	if !s.Configured() {
		return ""
	}
	length := s.KeyLength
	if length == 0 {
		length = 64
	}
	return NewSentinel(s.KeyPrefix, s.Name, length)
}

// AllowSpecs renders the destinations as `boks policy allow` rules.
//
// They are separate from the injection rule on purpose, and it is worth restating wherever a
// user meets it: naming a host for a credential says where a value may go, not what is
// reachable. Without an allow rule the default policy denies these hosts and the run fails at
// the network layer with no hint that a credential was ever involved.
func (s Service) AllowSpecs() []string {
	hosts := s.Hosts()
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h+":"+s.ports())
	}
	return out
}

// InjectSpecs renders the service as `--inject` rules, one per attachment.
//
// Rendering to the flag grammar rather than building Inject values directly is deliberate. It
// keeps one parser: a registry entry cannot express something a user could not have typed,
// the same validation rejects both, and the process that runs a sandbox's proxy — which
// receives these specs verbatim and knows nothing about the registry — needs no new field.
func (s Service) InjectSpecs() []string {
	if !s.Configured() {
		return nil
	}
	out := make([]string, 0, len(s.Inject))
	for _, rule := range s.Inject {
		out = append(out, s.Name+"@"+strings.Join(rule.Hosts, ",")+"="+rule.attachment())
	}
	return out
}

// Describe renders the header this rule sets, with the credential's place in it marked. It
// is printed where a user is choosing to store something, so that "which header does my key
// ride in" is answered on the screen rather than in a vendor's documentation.
func (r ServiceInject) Describe() string {
	header := r.Header
	if header == "" {
		header = DefaultHeader
	}
	switch r.Scheme {
	case SchemeBearer:
		return header + ": Bearer <credential>"
	case SchemeBasic:
		return header + ": Basic base64(" + r.Username + ":<credential>)"
	}
	if r.Format == "" {
		return header + ": <credential>"
	}
	return header + ": " + strings.Replace(r.Format, "%s", "<credential>", 1)
}

func (r ServiceInject) attachment() string {
	switch r.Scheme {
	case SchemeBearer:
		return "bearer"
	case SchemeBasic:
		if r.Username != "" {
			return "basic:" + r.Username
		}
		return "basic"
	}
	header := r.Header
	if header == "" {
		header = DefaultHeader
	}
	if r.Format == "" {
		return header
	}
	return header + ":" + r.Format
}

// GuestSpec renders the service as a `--guest-credential` rule: what the guest holds, and
// where it finds it. Empty when the service names no environment variable, in which case the
// guest is given nothing and the credential is attached to its requests all the same.
func (s Service) GuestSpec() string {
	if !s.Configured() || s.EnvName == "" {
		return ""
	}
	return s.Name + "=" + s.EnvName + "=" + s.Placeholder()
}

// Credential builds the runtime credential for this service, through the same parser the
// flags go through.
func (s Service) Credential() (Credential, error) {
	if err := RequireConfigured(s); err != nil {
		return Credential{}, err
	}
	var guest []string
	if spec := s.GuestSpec(); spec != "" {
		guest = []string{spec}
	}
	credentials, err := ParseCredentials(s.InjectSpecs(), guest)
	if err != nil {
		return Credential{}, fmt.Errorf("service %q: %w", s.Name, err)
	}
	c := credentials[0]
	c.Description = s.Summary
	return c, nil
}

// Validate rejects a definition that could not work. It runs over the built-in table at
// start-up, so a bad row fails there rather than inside somebody's request.
func (s Service) Validate() error {
	if !serviceNameRe.MatchString(s.Name) {
		return fmt.Errorf("service name %q is invalid: use letters and digits, separated by '.', '_' or '-'", s.Name)
	}
	if !s.Configured() {
		// A name with no rule is the honest state for a service nobody has sourced, and
		// the whole reason it is registered at all. Nothing else about it need hold.
		return nil
	}
	if s.Source == "" {
		return fmt.Errorf("service %q has a credential rule but cites no source; a rule nobody can check is a rule nobody can correct", s.Name)
	}
	if s.KeyPrefix == "" && s.KeyLength == 0 {
		return fmt.Errorf("service %q has no key shape; the guest's placeholder would not look like a credential and its own client would reject it before boks saw the request", s.Name)
	}
	for _, rule := range s.Inject {
		if len(rule.Hosts) == 0 {
			return fmt.Errorf("service %q has an injection rule with no hosts", s.Name)
		}
		if rule.Why == "" {
			return fmt.Errorf("service %q: injection rule for %s says what it does not do", s.Name, strings.Join(rule.Hosts, ", "))
		}
	}
	if _, err := s.Credential(); err != nil {
		return err
	}
	if s.TokenEndpoint.Host != "" && !strings.HasPrefix(s.TokenEndpoint.Path, "/") {
		return fmt.Errorf("service %q: oauth token endpoint path %q must start with '/'", s.Name, s.TokenEndpoint.Path)
	}
	return nil
}

// RequireConfigured reports why a known service cannot be used as it stands.
//
// The services Boks could not source are registered anyway, so that asking for one gives this
// answer instead of "unknown service" — the name is right, the rule is missing — which is
// exactly what internal/agent does for an agent it ships no image for.
func RequireConfigured(s Service) error {
	if s.Configured() {
		return nil
	}
	return fmt.Errorf("boks knows the service %q but has no credential rule for it.\n\n"+
		"What is missing: the host its credential is sent to, and the header that carries it.\n"+
		"No vendor documentation naming both was found, and boks will not guess — a guessed\n"+
		"rule sends the wrong header to the right host, or the right header to the wrong one,\n"+
		"and either way your placeholder reaches the real API instead of your credential.\n\n"+
		"Store it under a name of your own and say where it goes:\n"+
		"  boks secret set my-%s\n"+
		"  boks run --inject 'my-%s@api.example.com=Authorization:Bearer %%s' \\\n"+
		"           --guest-credential 'my-%s=SOME_API_KEY=placeholder' ...\n\n"+
		"If you have the vendor's documentation, the row to fill in is in internal/secret/service.go.",
		s.Name, s.Name, s.Name, s.Name)
}

// serviceNameRe is the grammar a service name has to satisfy. It is the same one an agent
// name uses, and for a related reason: the name is typed on a command line, stored as a key
// and printed in a table, so anything that would need quoting is a mistake rather than a
// preference.
var serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$`)

// ServiceRegistry is an ordered set of services. Order is preserved so that help and listings
// are stable, and so a user-defined service appears where it was added rather than wherever a
// map iteration puts it.
type ServiceRegistry struct {
	services []Service
}

// Add registers a service, replacing any existing one with the same name.
//
// Replacement rather than rejection is the seam a user-defined service will arrive through: a
// loader for a declarative definition has only to call this, and nothing above this package
// changes. It is also how someone holding the vendor documentation Boks could not find
// overrides an empty row, without waiting for a release.
func (r *ServiceRegistry) Add(s Service) error {
	if err := s.Validate(); err != nil {
		return err
	}
	for i, existing := range r.services {
		if existing.Name == s.Name {
			r.services[i] = s
			return nil
		}
	}
	r.services = append(r.services, s)
	return nil
}

// Lookup returns the named service.
func (r *ServiceRegistry) Lookup(name string) (Service, bool) {
	for _, s := range r.services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// Known reports whether a name is one of the services Boks recognises, configured or not.
func (r *ServiceRegistry) Known(name string) bool {
	_, ok := r.Lookup(name)
	return ok
}

// All returns the registered services in registration order.
func (r *ServiceRegistry) All() []Service { return slices.Clone(r.services) }

// Names returns the registered service names, for help text and error messages.
func (r *ServiceRegistry) Names() []string {
	out := make([]string, 0, len(r.services))
	for _, s := range r.services {
		out = append(out, s.Name)
	}
	return out
}

// Resolve returns the named service, or an error naming what Boks does know.
func (r *ServiceRegistry) Resolve(name string) (Service, error) {
	s, ok := r.Lookup(name)
	if !ok {
		return Service{}, fmt.Errorf("unknown service %q.\nServices: %s.\n"+
			"Any other name is stored as an ordinary credential; say where it goes with\n"+
			"'boks run --inject' when you use it.",
			name, strings.Join(r.Names(), ", "))
	}
	return s, nil
}

// PreferOAuth drops an API-key credential whose destinations are already covered by an OAuth
// credential in the same set, and reports what it dropped.
//
// Docker Sandboxes states the rule in its own help — an OAuth credential takes precedence over
// an API key for the same service — and it is the right way round for a reason worth writing
// down. An OAuth credential is the one a person acquired by logging in, it refreshes itself,
// and it is usually attached to the subscription they are paying for. An API key left in a
// store months ago silently taking its place is a failure nobody would think to look for: the
// requests succeed, against the wrong account and the wrong bill.
//
// Coverage is decided on destinations rather than on names, because the two credentials
// usually have different names — an OAuth credential adopted as `claude-code` and an API key
// stored as `anthropic` both end at api.anthropic.com — and it is the collision at the host
// that matters, not the spelling. An API key with any host of its own is kept: it is doing
// something the OAuth credential is not.
func PreferOAuth(credentials []Credential) (kept []Credential, dropped []string) {
	covered := map[string]bool{}
	for _, c := range credentials {
		if c.OAuth == nil {
			continue
		}
		for _, h := range c.OAuth.ResourceHosts {
			covered[strings.ToLower(h.String())] = true
		}
	}
	if len(covered) == 0 {
		return credentials, nil
	}
	for _, c := range credentials {
		if c.OAuth != nil || len(c.Inject) == 0 {
			kept = append(kept, c)
			continue
		}
		all := true
		for _, r := range c.Inject {
			if !covered[strings.ToLower(r.Domain.String())] {
				all = false
				break
			}
		}
		if all {
			dropped = append(dropped, c.Service)
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

// Services returns the services Boks knows about.
//
// The names and their order are Docker Sandboxes' own, verbatim from its `secret set --help`,
// so that a habit formed there works here. Nine of the eleven carry a rule; `cursor` and
// `droid` do not, and the comments on them say exactly what was missing.
//
// Every configured row was read from the vendor's own documentation, cited in Source. Two
// findings from that reading are worth having at the top, because both are the kind of thing
// a plausible guess gets wrong:
//
//   - **Anthropic and Google do not use bearer tokens.** Anthropic takes `x-api-key` and
//     Google takes `x-goog-api-key`; an API key sent as `Authorization: Bearer` fails on both,
//     and on Anthropic that header is reserved for OAuth tokens, so the failure is a 401 with
//     no hint that the header was the problem.
//   - **A documented header is not enough on its own.** `cursor` and `droid` both document a
//     header and an env var, and neither documents the host their CLI sends that env var to.
//     A rule built from the half that is documented would put a placeholder in the variable
//     and ship it to a host with no rule to replace it.
func Services() *ServiceRegistry {
	r := &ServiceRegistry{}
	for _, s := range builtinServices {
		if err := r.Add(s); err != nil {
			// A built-in definition is ours, so a bad one is a programming error.
			panic("secret: invalid built-in service: " + err.Error())
		}
	}
	return r
}

var builtinServices = []Service{
	{
		Name:    "anthropic",
		Summary: "Anthropic's Claude API",
		Inject: []ServiceInject{{
			Hosts: []string{"api.anthropic.com"},
			// Not bearer. The authentication page shows the header verbatim as
			// `x-api-key: YOUR_API_KEY`, and the API overview's header table
			// reserves `Authorization: Bearer` for a short-lived OAuth token from
			// /v1/oauth/token. An API key sent as a bearer token does not work.
			Header: "x-api-key",
			Why:    "the Claude API; the key is the whole header value",
		}},
		EnvName: "ANTHROPIC_API_KEY",
		// The prefix is documented — `export ANTHROPIC_API_KEY=sk-ant-api03-...` on the
		// authentication page. The length is not documented anywhere; 108 is the
		// observed length of a real key and only shapes the fake.
		KeyPrefix: "sk-ant-api03-",
		KeyLength: 108,
		// The endpoint a Claude.ai subscription login refreshes against, and the one
		// `boks secret adopt claude-code` already uses — see the claude-code profile in
		// import.go. Deliberately *not* api.anthropic.com/v1/oauth/token, which the
		// platform documents for workload-identity federation: that one exchanges an
		// IdP-issued JWT and would reject the refresh-token grant Boks performs.
		TokenEndpoint: Endpoint{Host: "console.anthropic.com", Path: "/v1/oauth/token"},
		TokenEncoding: EncodingJSON,
		OAuthProfile:  "claude-code",
		Source:        "platform.claude.com/docs/en/manage-claude/authentication and /docs/en/api/overview",
	},
	{
		Name:    "cursor",
		Summary: "Cursor",
		// Registered with no rule, and the reason is specific rather than a shrug.
		//
		// Cursor documents an API-key-authenticated HTTP API — api.cursor.com, taking
		// `Authorization: Bearer YOUR_API_KEY` or basic auth with the key as the
		// username — but that is the Admin and Cloud Agents API. The CLI's own
		// authentication page documents only `CURSOR_API_KEY` and `--api-key`, and
		// names no host, header or protocol for cursor-agent itself; the destinations
		// in that agent's allowlist (api2.cursor.sh, api5.cursor.sh) are not
		// api.cursor.com.
		//
		// So the two documented halves do not join up. A rule pointing CURSOR_API_KEY at
		// api.cursor.com would put a placeholder in the variable the CLI reads and send
		// it to a host no rule covers, which is the failure this file exists to avoid.
		// The row to fill in is here, the day Cursor documents what cursor-agent talks
		// to.
	},
	{
		Name:    "droid",
		Summary: "Factory Droid",
		// Registered with no rule, for the same reason as cursor and with the same one
		// piece missing.
		//
		// Factory documents `FACTORY_API_KEY`, the `fk-` prefix, and
		// `Authorization: Bearer $FACTORY_API_KEY` against api.factory.ai — but that is
		// the *Analytics* API. The droid CLI reference and droid-exec pages document the
		// variable and nothing about the wire: no host for the agent's own traffic. The
		// agent registry reached the same dead end when it looked for an allowlist for
		// droid and left it empty.
		//
		// Injecting on the strength of the analytics endpoint would attach the
		// credential to the one API the agent does not use, and hand the agent a
		// placeholder for the one it does.
	},
	{
		Name:    "github",
		Summary: "GitHub: the REST API and Git over HTTPS",
		// One credential, two attachments, because a GitHub token genuinely travels two
		// ways and the difference is per host.
		Inject: []ServiceInject{
			{
				Hosts:  []string{"api.github.com"},
				Scheme: SchemeBearer,
				// The REST authentication page's own curl uses
				// `--header "Authorization: Bearer YOUR-TOKEN"`. It notes that
				// `token YOUR-TOKEN` also works for a personal access token;
				// Bearer is used here because it is valid for every token type,
				// JWTs included.
				Why: "the REST API (docs.github.com REST authentication)",
			},
			{
				Hosts:    []string{"github.com"},
				Scheme:   SchemeBasic,
				Username: "x-access-token",
				// Git over HTTPS takes the token as the HTTP *password*. GitHub
				// documents `https://x-access-token:TOKEN@github.com/owner/repo`
				// for a GitHub App installation token; for a personal access
				// token the username is ignored by the server, so the same
				// username works and is the convention every tool uses. That
				// second half is convention rather than documentation, and is
				// harmless: the server reads the password.
				Why: "Git over HTTPS (docs.github.com, App installation token form)",
			},
		},
		// gh reads GH_TOKEN first and GITHUB_TOKEN second, and git's credential
		// helpers read neither — the basic-auth rule above is what makes a push work,
		// whatever the guest has in its environment. GITHUB_TOKEN is the variable set
		// here because it is the one both gh and the wider ecosystem honour.
		EnvName: "GITHUB_TOKEN",
		// `ghp_` is documented in GitHub's credential-types reference. The 40
		// characters are *not* documented: they follow from the pre-2021 format and
		// GitHub's "the length is remaining the same for now", and GitHub explicitly
		// tells integrators to match on prefix and to expect up to 255 characters. This
		// length shapes a fake and is not a validation rule anywhere in Boks.
		KeyPrefix: "ghp_",
		KeyLength: 40,
		Source:    "docs.github.com REST authentication, GitHub credential types, and github.blog token formats",
	},
	{
		Name:    "google",
		Summary: "Google's Gemini API (AI Studio)",
		Inject: []ServiceInject{{
			Hosts: []string{"generativelanguage.googleapis.com"},
			// Not bearer, and this is the one most likely to be guessed wrong: the
			// Gemini API key page's curl is `-H "x-goog-api-key: YOUR_API_KEY"`.
			Header: "x-goog-api-key",
			Why:    "the Gemini API (ai.google.dev api-key page)",
		}},
		// Google documents both GEMINI_API_KEY and GOOGLE_API_KEY, with GOOGLE_API_KEY
		// winning when both are set. GEMINI_API_KEY is the one placed here: it is the
		// narrower of the two, and setting the variable that wins over everything would
		// override a Google credential the guest may hold for something else entirely.
		EnvName: "GEMINI_API_KEY",
		// Documented, unusually completely: "The API key string is an encrypted string,
		// for example, AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe" — 39 characters, prefix
		// included.
		KeyPrefix: "AIza",
		KeyLength: 39,
		// Google's OAuth 2.0 token endpoint takes the standard refresh-token grant,
		// form-encoded, which is the grant Boks performs. Recorded so that an OAuth
		// credential for Google needs no endpoint typed in; nothing here performs a
		// login.
		TokenEndpoint: Endpoint{Host: "oauth2.googleapis.com", Path: "/token"},
		TokenEncoding: EncodingForm,
		Source:        "ai.google.dev/gemini-api/docs/api-key, docs.cloud.google.com api-keys, developers.google.com/identity/protocols/oauth2",
	},
	{
		Name:    "groq",
		Summary: "GroqCloud",
		Inject: []ServiceInject{{
			Hosts:  []string{"api.groq.com"},
			Scheme: SchemeBearer,
			Why:    "the GroqCloud API (console.groq.com api-reference)",
		}},
		EnvName: "GROQ_API_KEY",
		// `gsk_` appears in Groq's own security-onboarding page as
		// "gsk_your_secret_key_here". No length is published; 56 is a plausible fake.
		KeyPrefix: "gsk_",
		KeyLength: 56,
		Source:    "console.groq.com/docs/api-reference, /docs/quickstart, /docs/production-readiness/security-onboarding",
	},
	{
		Name:    "mistral",
		Summary: "Mistral AI",
		Inject: []ServiceInject{{
			Hosts:  []string{"api.mistral.ai"},
			Scheme: SchemeBearer,
			Why:    "the Mistral API (docs.mistral.ai chat endpoint)",
		}},
		EnvName: "MISTRAL_API_KEY",
		// No prefix at all, and that is a finding rather than an omission: Mistral's
		// key-generation and admin pages publish no format, and inventing a prefix would
		// hand a client a fake that fails a check a real key passes. An unprefixed
		// opaque string of a plausible length is the honest placeholder.
		KeyLength: 32,
		Source:    "docs.mistral.ai/api/endpoint/chat and /admin/identity-access/api-keys",
	},
	{
		Name:    "nebius",
		Summary: "Nebius Token Factory (formerly Nebius AI Studio)",
		Inject: []ServiceInject{{
			// The host to be careful about. Nebius AI Studio is now Nebius Token
			// Factory, and docs.nebius.com/studio/api/* redirects to
			// docs.tokenfactory.nebius.com. The api.studio.nebius.com host is still
			// all over third-party model routers and is not in current vendor docs,
			// so it is left out: a stale injection domain would decrypt a host for
			// nothing and attach the credential to no request at all.
			Hosts:  []string{"api.tokenfactory.nebius.com"},
			Scheme: SchemeBearer,
			Why:    "the Token Factory inference API (docs.tokenfactory.nebius.com quickstart)",
		}},
		EnvName: "NEBIUS_API_KEY",
		// No prefix documented — the reference shows only "ABC123...". Unprefixed, as
		// for Mistral.
		KeyLength: 64,
		Source:    "docs.tokenfactory.nebius.com/quickstart and /api-reference",
	},
	{
		Name:    "openai",
		Summary: "the OpenAI API",
		Inject: []ServiceInject{{
			Hosts:  []string{"api.openai.com"},
			Scheme: SchemeBearer,
			Why:    "the OpenAI API (developers.openai.com reference overview)",
		}},
		EnvName: "OPENAI_API_KEY",
		// Only the generic `sk-` is documented, via the API-key object's
		// redacted_value example "sk-abc...def". The `sk-proj-` form everyone has seen
		// is not in the documentation, so it is not used here: a fake carrying an
		// undocumented prefix asserts something about the vendor that cannot be
		// checked. No length is documented either; 51 is the observed classic shape.
		KeyPrefix: "sk-",
		KeyLength: 51,
		// No token endpoint is recorded. OpenAI documents
		// auth.openai.com/oauth/token, but it performs an RFC 8693 *token exchange*
		// for workload-identity federation and does not accept the refresh-token
		// grant Boks would send it. An endpoint that cannot answer the request Boks
		// makes is worse than none: it turns a missing feature into a failing one.
		Source: "developers.openai.com/api/reference/overview and /api/docs/quickstart",
	},
	{
		Name:    "openrouter",
		Summary: "OpenRouter",
		Inject: []ServiceInject{{
			Hosts:  []string{"openrouter.ai"},
			Scheme: SchemeBearer,
			Why:    "the OpenRouter API (openrouter.ai/docs authentication)",
		}},
		EnvName: "OPENROUTER_API_KEY",
		// `sk-or-v1-` is documented, shown as "sk-or-v1-abc...123" on the provisioning
		// page. The 64-character tail is observed and not documented.
		KeyPrefix: "sk-or-v1-",
		KeyLength: 73,
		// No token endpoint. OpenRouter's OAuth is a PKCE flow whose exchange is
		// POST /api/v1/auth/keys taking {code, code_verifier} and returning a *key*,
		// not an RFC 6749 token endpoint taking a refresh-token grant. Boks' refresher
		// would send it a request it does not implement.
		Source: "openrouter.ai/docs/api_reference/authentication and /docs/features/provisioning-api-keys",
	},
	{
		Name:    "xai",
		Summary: "xAI's Grok API",
		Inject: []ServiceInject{{
			Hosts:  []string{"api.x.ai"},
			Scheme: SchemeBearer,
			Why:    "the xAI API (docs.x.ai overview)",
		}},
		EnvName: "XAI_API_KEY",
		// `xai-` with an 80-character tail, from the one real-shaped key in xAI's
		// management API reference. xAI does not state a guaranteed length.
		KeyPrefix: "xai-",
		KeyLength: 84,
		Source:    "docs.x.ai/docs/overview and /developers/rest-api-reference/management/auth",
	},
}
