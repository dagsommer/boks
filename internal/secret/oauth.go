package secret

// OAuth credentials: the second credential mechanism, and the one most users of the
// flagship agent actually have.
//
// # Why this exists
//
// An API key is a single header value, and the Inject rules above are enough for it. A
// subscription login is not: it is a *pair* of tokens with an expiry and an endpoint that
// mints new ones. `claude /login` leaves no ANTHROPIC_API_KEY behind — it leaves an OAuth
// access token and a refresh token in the macOS Keychain — so a sandbox that can only carry
// header values cannot run Claude Code at all, no matter what is allowlisted.
//
// # The shape, and where it came from
//
// This follows Docker Sandboxes' kit-format v2 `oauth` block: a token endpoint, a pair of
// sentinels, a set of resource hosts, and a credential file template. The guest is given
// **sentinels** — fake tokens, shaped like the real ones — in an environment variable, a
// credential file, or both. The host proxy swaps a sentinel for the real access token on
// requests to the resource hosts, in a named header and nowhere else. The real token never
// exists inside the guest, in any file, environment or response.
//
// # Refresh happens on the host, and the guest's refresh call is answered, not forwarded
//
// An access token expires. Two designs are possible and both are defensible; this package
// implements the second and refuses the first:
//
//  1. **Forward the guest's refresh** to the token endpoint with the real refresh token
//     substituted in, then rewrite the response so the guest sees sentinels again.
//  2. **Refresh on the host** — never forward a guest's token request at all. Boks answers
//     it itself, from the host, with a synthetic response carrying the same sentinels.
//
// (1) was rejected for three reasons. It lets a guest-composed request reach the token
// endpoint carrying a real refresh token, so a request Boks does not understand — a
// different grant type, an extra parameter, a second endpoint on the same host — produces a
// response Boks cannot rewrite, and a real token lands in the guest. It requires buffering
// and re-serialising a response body, which is the one thing the inspected path in
// internal/proxy deliberately never does. And it puts a rotated refresh token in flight
// before it is durably stored, so a crash mid-exchange loses the credential entirely.
//
// Under (2) the guest observes: a normal `200` with `access_token`, `refresh_token` and
// `expires_in`, whose token values are **the very sentinels it already holds**. That
// matters for the case the reference format leaves implicit — an agent that persists what a
// refresh returned, rewriting its own credential file. Here that rewrite is a fixed point:
// it writes back the same sentinels. If the credential file is mounted read-only (it is; see
// internal/enforce) the write fails instead, and the file on disk is already correct.
//
// The cost of (2), stated plainly: Boks has to know the token endpoint's request and
// response shape rather than blindly relaying bytes. That is a real coupling to a provider,
// and it is why the endpoint, the encoding and the response field names are configuration
// rather than constants.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/dagsommer/boks/internal/policy"
)

// OAuthTokens is the secret half of an OAuth credential: the pair, and when the access
// token stops working.
//
// Both fields are Values, so neither can reach a log through a format verb. Expiry is not a
// secret and is deliberately plain: knowing *when* a token dies is what makes a refresh
// decidable, and it is printed in diagnostics.
type OAuthTokens struct {
	Access  Value
	Refresh Value
	// Expiry is when the access token stops being accepted. The zero time means the
	// credential did not say, and is treated as "cannot be refreshed proactively" rather
	// than as "expired" — guessing in either direction is worse than injecting what we
	// have and letting the origin decide.
	Expiry time.Time
}

// IsZero reports whether there is no access token at all.
func (t OAuthTokens) IsZero() bool { return t.Access.IsZero() }

// Expired reports whether the access token is past its expiry, allowing for skew.
func (t OAuthTokens) Expired(now time.Time, skew time.Duration) bool {
	if t.Expiry.IsZero() {
		return false
	}
	return !now.Add(skew).Before(t.Expiry)
}

// String is redacted. A struct holding two Values would already print them redacted, but an
// OAuthTokens is exactly the thing a careless %v is most likely to reach for.
func (t OAuthTokens) String() string {
	return "secret.OAuthTokens(" + Redacted + ", expires " + t.expiryText() + ")"
}

// GoString covers %#v.
func (t OAuthTokens) GoString() string { return t.String() }

func (t OAuthTokens) expiryText() string {
	if t.Expiry.IsZero() {
		return "unknown"
	}
	return t.Expiry.UTC().Format(time.RFC3339)
}

// Endpoint is the host and path a token is refreshed against. It is split because the host
// is a policy destination — it decides interception — while the path only selects which
// request Boks answers itself.
type Endpoint struct {
	Host string
	Path string
}

func (e Endpoint) String() string { return e.Host + e.Path }

// URL is the absolute HTTPS URL of the endpoint. Always https: a refresh token is the most
// valuable thing this package holds and there is no plaintext story for it.
func (e Endpoint) URL() string { return "https://" + e.Host + e.Path }

// Sentinels are the fake tokens the guest holds.
//
// They must look real, for the same reason a placeholder must: clients validate credential
// format locally. Claude Code checks that an OAuth access token starts with `sk-ant-oat01-`
// before it will send one, so a sentinel spelled "boks-managed" fails inside the guest and
// no request ever reaches the proxy that would have fixed it. See NewSentinel.
type Sentinels struct {
	Access  string
	Refresh string
}

// ResponseFields names the fields Boks reads out of a token response, for providers that
// spell them differently. Empty means the RFC 6749 name.
type ResponseFields struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    string
}

func (f ResponseFields) access() string {
	if f.AccessToken != "" {
		return f.AccessToken
	}
	return "access_token"
}

func (f ResponseFields) refresh() string {
	if f.RefreshToken != "" {
		return f.RefreshToken
	}
	return "refresh_token"
}

func (f ResponseFields) expiresIn() string {
	if f.ExpiresIn != "" {
		return f.ExpiresIn
	}
	return "expires_in"
}

// CredentialFile is a file rendered into the guest from a Go template, containing sentinels.
//
// Path is the absolute path *inside the guest*. Boks materialises the file on the host and
// shares its directory read-only, exactly as it does for the CA certificate — nothing is
// written into an image and no secret is ever in the file, because the template is only ever
// executed with sentinel data.
type CredentialFile struct {
	Path     string
	Template string
}

// Encoding is how a token request is serialised.
type Encoding string

const (
	// EncodingJSON posts a JSON object. Anthropic's token endpoint wants this.
	EncodingJSON Encoding = "json"
	// EncodingForm posts application/x-www-form-urlencoded, which is what RFC 6749 says.
	EncodingForm Encoding = "form"
)

// DefaultRefreshSkew is how long before expiry a token is refreshed. A request that starts
// valid and finishes expired is a failure the guest cannot diagnose, so the host moves first.
const DefaultRefreshSkew = 2 * time.Minute

// sentinelLifetime is the expiry Boks reports to the guest, in the credential file and in
// the answer to a refresh.
//
// It is deliberately far away. Under this design expiry is a **host-side** concern: the
// proxy refreshes when the real token is close to death and substitutes whatever is current.
// The guest's copy is a stand-in, not a token, so its expiry field describes nothing real —
// and a value in the past would only make a cooperating agent thrash against a refresh it
// does not need. What the guest is being told is "this does not expire from where you are
// standing", which is true.
const sentinelLifetime = 365 * 24 * time.Hour

// OAuth is the non-secret half of an OAuth credential: everything needed to place sentinels
// in a guest, recognise the hosts where one is swapped, and refresh the pair on the host.
type OAuth struct {
	// TokenEndpoint is where tokens are refreshed. Its host is a credential host: Boks
	// terminates TLS for it, because a token request has to be answered rather than
	// forwarded.
	TokenEndpoint Endpoint
	// ClientID identifies this client to the token endpoint. It is not a secret.
	ClientID string
	// Encoding is how the refresh request is serialised. Empty means JSON.
	Encoding Encoding
	// ResourceHosts are the API hosts where a sentinel becomes the real access token.
	// Never a catch-all, for the same reason an injection domain never is.
	ResourceHosts []policy.Pattern
	// Headers are the headers a sentinel is substituted in. Empty means Authorization.
	//
	// This is an allowlist rather than "every header", and the difference is a real
	// exfiltration boundary: an origin that echoes an arbitrary request header back in its
	// response would hand the guest the substituted token. Restricting substitution to the
	// header the credential is actually sent in means an echo attack needs an origin that
	// reflects Authorization, which is the same exposure the API-key path already has.
	Headers []string
	// Sentinels are what the guest holds.
	Sentinels Sentinels
	// EnvName is an environment variable the guest reads the access-token sentinel from.
	// Optional; a credential file may be used instead, or both.
	EnvName string
	// File is the credential file rendered into the guest. Optional.
	File CredentialFile
	// ResponseFields overrides the token response field names.
	ResponseFields ResponseFields
	// Scopes and SubscriptionType are non-secret metadata carried through to the
	// credential file, because agents read them back and behave differently.
	Scopes           []string
	SubscriptionType string
}

// String describes an OAuth block without printing its sentinels.
//
// A sentinel is a fake by construction, so this is not protecting a secret. It is protecting
// something else worth as much: a reader's ability to tell, by looking at a log, whether a
// token-shaped string is real. If sentinels appear in logs then "sk-ant-oat01-…" in a log
// line stops being alarming, and the day a real one leaks nobody notices. There is a test.
func (o *OAuth) String() string {
	if o == nil {
		return "<nil>"
	}
	return fmt.Sprintf("secret.OAuth{token:%s resources:%v headers:%v sentinels:%s}",
		o.TokenEndpoint, o.Domains()[:len(o.ResourceHosts)], o.headers(), Redacted)
}

// GoString covers %#v, which would otherwise print every field including the sentinels.
func (o *OAuth) GoString() string { return o.String() }

// Validate rejects an OAuth block that could not work, or that could send a token somewhere
// it does not belong.
func (o *OAuth) Validate() error {
	if o == nil {
		return nil
	}
	if o.TokenEndpoint.Host == "" {
		return errors.New("no oauth token endpoint host")
	}
	if strings.ContainsAny(o.TokenEndpoint.Host, "/ ") {
		return fmt.Errorf("oauth token endpoint host %q looks like a URL; give a host and a path separately", o.TokenEndpoint.Host)
	}
	if !strings.HasPrefix(o.TokenEndpoint.Path, "/") {
		return fmt.Errorf("oauth token endpoint path %q must start with '/'", o.TokenEndpoint.Path)
	}
	if len(o.ResourceHosts) == 0 {
		return errors.New("an oauth credential with no resource hosts has nowhere to be used")
	}
	for _, h := range o.ResourceHosts {
		if h.IsAny() {
			return errors.New("'*' is not allowed as an oauth resource host; name the hosts that may receive this token, because those are also the hosts whose TLS boks will terminate")
		}
		if h.String() == "" {
			return errors.New("an oauth resource host is empty")
		}
	}
	switch o.Encoding {
	case "", EncodingJSON, EncodingForm:
	default:
		return fmt.Errorf("unknown oauth request encoding %q; use json or form", o.Encoding)
	}
	for _, h := range o.headers() {
		if err := validHeaderName(h); err != nil {
			return err
		}
	}
	if err := validSentinel("access", o.Sentinels.Access); err != nil {
		return err
	}
	if o.Sentinels.Refresh != "" {
		if err := validSentinel("refresh", o.Sentinels.Refresh); err != nil {
			return err
		}
		if o.Sentinels.Refresh == o.Sentinels.Access {
			return errors.New("the access and refresh sentinels are the same value")
		}
	}
	if o.File.Path != "" {
		if !strings.HasPrefix(o.File.Path, "/") {
			return fmt.Errorf("oauth credential file %q must be an absolute guest path", o.File.Path)
		}
		if o.File.Template == "" {
			return errors.New("an oauth credential file needs a template")
		}
		if _, err := o.renderFile(); err != nil {
			return err
		}
	}
	return nil
}

// validSentinel enforces the two properties substitution depends on: a sentinel is long
// enough that it cannot appear in a header by accident, and it cannot forge a second header.
func validSentinel(which, s string) error {
	if s == "" {
		return fmt.Errorf("no oauth %s sentinel; the guest has to hold something shaped like a token", which)
	}
	if len(s) < 16 {
		return fmt.Errorf("the oauth %s sentinel is %d characters; it must be at least 16, or a substitution could fire on a coincidence", which, len(s))
	}
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("the oauth %s sentinel contains a newline", which)
	}
	return nil
}

func (o *OAuth) headers() []string {
	if len(o.Headers) == 0 {
		return []string{DefaultHeader}
	}
	return o.Headers
}

func (o *OAuth) encoding() Encoding {
	if o.Encoding == "" {
		return EncodingJSON
	}
	return o.Encoding
}

// MatchesResource reports whether t is a host where a sentinel becomes a real token.
func (o *OAuth) MatchesResource(t policy.Target) bool {
	if o == nil {
		return false
	}
	for _, h := range o.ResourceHosts {
		if h.Match(t) {
			return true
		}
	}
	return false
}

// MatchesTokenEndpoint reports whether t is the token endpoint's host.
func (o *OAuth) MatchesTokenEndpoint(t policy.Target) bool {
	if o == nil {
		return false
	}
	return strings.EqualFold(t.Host, o.TokenEndpoint.Host)
}

// Domains lists every destination this OAuth block causes to be decrypted: the resource
// hosts, plus the token endpoint, whose requests are answered rather than forwarded.
func (o *OAuth) Domains() []string {
	if o == nil {
		return nil
	}
	out := make([]string, 0, len(o.ResourceHosts)+1)
	for _, h := range o.ResourceHosts {
		out = append(out, h.String())
	}
	return append(out, o.TokenEndpoint.Host)
}

// substitute replaces the access sentinel with the real token in the headers this credential
// is allowed to touch, and reports whether it did.
//
// Substitution, not assignment: a header the guest did not put a sentinel in is left alone.
// That keeps the guest's own request shape intact and makes the mechanism auditable — a
// token appears exactly where a sentinel was, and nowhere else.
func (o *OAuth) substitute(h http.Header, access Value) bool {
	replaced := false
	for _, name := range o.headers() {
		values := h.Values(name)
		if len(values) == 0 {
			continue
		}
		out := make([]string, 0, len(values))
		changed := false
		for _, v := range values {
			if strings.Contains(v, o.Sentinels.Access) {
				v = strings.ReplaceAll(v, o.Sentinels.Access, access.Reveal())
				changed = true
			}
			out = append(out, v)
		}
		if changed {
			h.Del(name)
			for _, v := range out {
				h.Add(name, v)
			}
			replaced = true
		}
	}
	return replaced
}

// CredentialFileData is everything a credential file template can see.
//
// There is no field here that could hold a real token. That is the point: the template is
// executed with this struct and nothing else, so no template — including one a user writes —
// can render a secret into a file the guest reads.
type CredentialFileData struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        int64 // unix milliseconds, which is what Claude Code's file uses
	ExpiresAtSeconds int64
	Scopes           []string
	SubscriptionType string
}

// renderFile renders the credential file's content from the sentinels.
func (o *OAuth) renderFile() ([]byte, error) {
	tmpl, err := template.New("credential").Option("missingkey=error").Parse(o.File.Template)
	if err != nil {
		return nil, fmt.Errorf("oauth credential file template: %w", err)
	}
	expiry := time.Now().Add(sentinelLifetime)
	data := CredentialFileData{
		AccessToken:      o.Sentinels.Access,
		RefreshToken:     o.Sentinels.Refresh,
		ExpiresAt:        expiry.UnixMilli(),
		ExpiresAtSeconds: expiry.Unix(),
		Scopes:           o.Scopes,
		SubscriptionType: o.SubscriptionType,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("oauth credential file template: %w", err)
	}
	return buf.Bytes(), nil
}

// RenderedFile is a credential file waiting to be written on the host and shared into a
// guest.
type RenderedFile struct {
	// Service is the credential it belongs to.
	Service string
	// Path is the absolute path inside the guest.
	Path string
	// Content is sentinels only. It is safe to write, safe to share and safe to print.
	Content []byte
}

// CredentialFiles renders every credential file the given credentials ask for.
//
// It is a package function rather than an Injector method because the process that prepares
// a sandbox has credentials but no provider — and must not need one. Rendering a credential
// file requires no secret at all, which is the property worth keeping legible.
func CredentialFiles(credentials []Credential) ([]RenderedFile, error) {
	var out []RenderedFile
	for _, c := range credentials {
		if c.OAuth == nil || c.OAuth.File.Path == "" {
			continue
		}
		content, err := c.OAuth.renderFile()
		if err != nil {
			return nil, fmt.Errorf("credential %q: %w", c.Service, err)
		}
		out = append(out, RenderedFile{Service: c.Service, Path: c.OAuth.File.Path, Content: content})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out, nil
}

// OAuthProvider supplies OAuth token pairs by service. Like Provider, it runs host-side
// only and there is no path from a guest to it.
type OAuthProvider interface {
	LookupOAuth(ctx context.Context, service string) (OAuthTokens, error)
}

// OAuthSaver persists a rotated token pair. A provider that cannot persist simply does not
// implement it, and rotation then lives only as long as the process — which is a real
// limitation, stated where it happens rather than hidden.
type OAuthSaver interface {
	SaveOAuth(ctx context.Context, service string, tokens OAuthTokens) error
}

// Refresher exchanges a refresh token for a new pair. It is an interface so that the
// exchange can be driven by a test without a network, and so that the one place a refresh
// token is put on the wire is a single, greppable implementation.
type Refresher interface {
	Refresh(ctx context.Context, o *OAuth, refresh Value) (OAuthTokens, error)
}

// HTTPRefresher performs the exchange over HTTPS from the host.
//
// This request leaves the host directly. It is not guest traffic: it does not pass through
// the sandbox's proxy, is not subject to the sandbox's network policy, and carries no guest
// input — the body is composed here from the stored refresh token and client id alone.
type HTTPRefresher struct {
	// Client is the HTTP client used. Nil means a client with a 30s timeout.
	Client *http.Client
}

// Refresh implements Refresher.
func (h HTTPRefresher) Refresh(ctx context.Context, o *OAuth, refresh Value) (OAuthTokens, error) {
	if refresh.IsZero() {
		return OAuthTokens{}, errors.New("no refresh token")
	}
	req, err := h.request(ctx, o, refresh)
	if err != nil {
		return OAuthTokens{}, err
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL is in the error but never the body, which held the refresh token.
		return OAuthTokens{}, fmt.Errorf("refreshing against %s: %w", o.TokenEndpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return OAuthTokens{}, fmt.Errorf("reading the token response from %s: %w", o.TokenEndpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Deliberately not the body: a token endpoint's error body is not guaranteed to
		// be free of the credential it was sent.
		return OAuthTokens{}, fmt.Errorf("%s refused the refresh with status %d", o.TokenEndpoint, resp.StatusCode)
	}
	return ParseTokenResponse(body, o.ResponseFields, time.Now())
}

func (h HTTPRefresher) request(ctx context.Context, o *OAuth, refresh Value) (*http.Request, error) {
	var (
		body        io.Reader
		contentType string
	)
	switch o.encoding() {
	case EncodingForm:
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refresh.Reveal()},
		}
		if o.ClientID != "" {
			form.Set("client_id", o.ClientID)
		}
		body, contentType = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	default:
		payload := map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": refresh.Reveal(),
		}
		if o.ClientID != "" {
			payload["client_id"] = o.ClientID
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding the refresh request: %w", err)
		}
		body, contentType = bytes.NewReader(encoded), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenEndpoint.URL(), body)
	if err != nil {
		return nil, fmt.Errorf("building the refresh request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// ParseTokenResponse reads a token endpoint's JSON answer.
//
// A response that carries no access token is an error rather than an empty credential: the
// alternative is silently replacing a working token with nothing.
func ParseTokenResponse(body []byte, fields ResponseFields, now time.Time) (OAuthTokens, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		// The body is not echoed: it is the one place a token is guaranteed to be.
		return OAuthTokens{}, errors.New("the token endpoint's response is not a JSON object")
	}
	access, _ := raw[fields.access()].(string)
	if access == "" {
		return OAuthTokens{}, fmt.Errorf("the token endpoint's response has no %q", fields.access())
	}
	tokens := OAuthTokens{Access: NewValue(access)}
	if refresh, ok := raw[fields.refresh()].(string); ok && refresh != "" {
		tokens.Refresh = NewValue(refresh)
	}
	if seconds, ok := raw[fields.expiresIn()].(float64); ok && seconds > 0 {
		tokens.Expiry = now.Add(time.Duration(seconds) * time.Second)
	}
	return tokens, nil
}

// TokenExchange is Boks' own answer to a guest's token request: a response the proxy writes
// back without ever contacting the token endpoint on the guest's behalf.
type TokenExchange struct {
	// Service is the credential involved, for the log. Never a value.
	Service string
	// Status and Body are what the guest receives. Body contains sentinels only.
	Status int
	Body   []byte
	// Refreshed reports whether the host actually exchanged a refresh token while
	// answering. It is logged as a fact, without values.
	Refreshed bool
}

// tokenResponseBody builds the synthetic answer: the sentinels the guest already holds, and
// a lifetime that says "not your problem".
func (o *OAuth) tokenResponseBody() ([]byte, error) {
	payload := map[string]any{
		o.ResponseFields.access():    o.Sentinels.Access,
		o.ResponseFields.expiresIn(): int64(sentinelLifetime.Seconds()),
		"token_type":                 "Bearer",
	}
	if o.Sentinels.Refresh != "" {
		payload[o.ResponseFields.refresh()] = o.Sentinels.Refresh
	}
	if len(o.Scopes) > 0 {
		payload["scope"] = strings.Join(o.Scopes, " ")
	}
	return json.Marshal(payload)
}

// NewSentinel builds a sentinel shaped like the credential it stands for: the real prefix,
// the real length, and a body that is unmistakably Boks' if it ever escapes.
//
// Shape is not decoration. `gh` checks a token's prefix, cloud SDKs check lengths, and
// Claude Code refuses an OAuth token that does not start with `sk-ant-oat01-` — all of that
// happens inside the guest, before any request reaches the proxy that would have substituted
// a real value. A sentinel that fails a local format check fails invisibly.
//
// It is deterministic in the seed so that a sandbox restarted tomorrow presents the guest
// with the same value it presented today: an agent that persisted the sentinel in a file of
// its own must not find a different one next time.
func NewSentinel(prefix, seed string, length int) string {
	const marker = "boksproxymanaged"
	body := marker + "-" + sanitizeSentinel(seed)
	// A fixed alphabet padding keeps the result inside the character set every token
	// format Boks has seen uses, so a client's own validation cannot trip on it.
	const filler = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for i := 0; len(prefix)+len(body) < length; i++ {
		body += string(filler[(i*7+len(seed))%len(filler)])
	}
	out := prefix + body
	if length > 0 && len(out) > length {
		out = out[:length]
	}
	return out
}

func sanitizeSentinel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
