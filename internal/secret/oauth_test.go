package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The canaries. Both must be absent from every log line, every error and every byte that
// travels toward a guest — the token because it is the credential, the sentinel because the
// whole design rests on a reader being able to tell the two apart by looking.
const (
	accessCanary  = "sk-ant-oat01-ACCESS-CANARY-must-never-be-printed"
	refreshCanary = "sk-ant-ort01-REFRESH-CANARY-must-never-be-printed"
	rotatedAccess = "sk-ant-oat01-ROTATED-ACCESS-must-never-be-printed"
)

// testRecord builds a stored OAuth credential for a fictional provider whose shape mirrors
// the real one: an api host where the token is used, a separate host for token exchange.
func testRecord(t *testing.T, expiry time.Time) OAuthRecord {
	t.Helper()
	r := OAuthRecord{
		V:               OAuthRecordVersion,
		Service:         "claude-code",
		AccessToken:     accessCanary,
		RefreshToken:    refreshCanary,
		Scopes:          []string{"user:inference", "user:profile"},
		Subscription:    "max",
		TokenHost:       "console.creds.test",
		TokenPath:       "/v1/oauth/token",
		ClientID:        "client-id-is-public",
		Encoding:        EncodingJSON,
		ResourceHosts:   []string{"api.creds.test"},
		Headers:         []string{"Authorization"},
		AccessSentinel:  NewSentinel("sk-ant-oat01-", "claude-code-access", 108),
		RefreshSentinel: NewSentinel("sk-ant-ort01-", "claude-code-refresh", 108),
		EnvName:         "CLAUDE_CODE_OAUTH_TOKEN",
		FilePath:        GuestCredentialDir + "/claude-code.json",
		FileTemplate:    claudeCodeTemplate,
	}
	if !expiry.IsZero() {
		r.ExpiresAt = expiry.UnixMilli()
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("the test record is not valid: %v", err)
	}
	return r
}

// testInjector wires a record to a memory store, which is Provider, OAuthProvider and
// OAuthSaver at once — the same three roles the file store plays in production.
func testInjector(t *testing.T, r OAuthRecord) (*Injector, *MemoryStore, Credential) {
	t.Helper()
	store := NewMemoryStore(nil, map[string]OAuthRecord{r.Service: r})
	c, err := r.Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	inj, err := NewInjector(store, c)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return inj, store, c
}

// stubRefresher stands in for the token endpoint, so that a refresh can be driven without a
// network and its inputs inspected.
type stubRefresher struct {
	calls    int
	gotToken string
	access   string
	refresh  string
	ttl      time.Duration
	err      error
}

func (s *stubRefresher) Refresh(_ context.Context, _ *OAuth, refresh Value) (OAuthTokens, error) {
	s.calls++
	s.gotToken = refresh.Reveal()
	if s.err != nil {
		return OAuthTokens{}, s.err
	}
	out := OAuthTokens{Access: NewValue(s.access), Refresh: NewValue(s.refresh)}
	if s.ttl > 0 {
		out.Expiry = time.Now().Add(s.ttl)
	}
	return out, nil
}

// TestSentinelIsShapedLikeTheCredential: a sentinel that fails a client's own format check
// never reaches the proxy, so shape is a functional requirement rather than a nicety.
func TestSentinelIsShapedLikeTheCredential(t *testing.T) {
	s := NewSentinel("sk-ant-oat01-", "claude-code-access", 108)

	if !strings.HasPrefix(s, "sk-ant-oat01-") {
		t.Errorf("sentinel %q does not carry the provider's prefix", s)
	}
	if len(s) != 108 {
		t.Errorf("sentinel is %d characters, want 108", len(s))
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			t.Fatalf("sentinel %q contains %q, which is outside the token character set", s, string(r))
		}
	}
	if !strings.Contains(s, "boksproxymanaged") {
		t.Error("a sentinel that escapes must be recognisable as boks'; the marker is missing")
	}
	// Determinism: a sandbox restarted tomorrow must present the same value, or an agent
	// that persisted the sentinel finds a different one and re-authenticates for nothing.
	if again := NewSentinel("sk-ant-oat01-", "claude-code-access", 108); again != s {
		t.Errorf("the sentinel is not deterministic: %q then %q", s, again)
	}
	if other := NewSentinel("sk-ant-ort01-", "claude-code-refresh", 108); other == s {
		t.Error("the access and refresh sentinels are identical")
	}
}

// TestSentinelSubstitutedOnResourceHostOnly is the core of the feature and of its containment:
// the swap happens on the configured resource host, and on nothing else — not on the token
// endpoint's own host, not on an unrelated host, not on a host that has some other
// credential's injection rule.
func TestSentinelSubstitutedOnResourceHostOnly(t *testing.T) {
	record := testRecord(t, time.Now().Add(time.Hour))
	store := NewMemoryStore(map[string]string{"other": "some-api-key"},
		map[string]OAuthRecord{record.Service: record})
	oauthCredential, err := record.Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	inj, err := NewInjector(store, oauthCredential, mustCredential(t, "other", "other@elsewhere.test=bearer"))
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	sentinel := record.AccessSentinel

	cases := []struct {
		host string
		want string
	}{
		{"api.creds.test:443", "Bearer " + accessCanary},
		{"console.creds.test:443", "Bearer " + sentinel},
		// A host that belongs to a *different* credential gets that credential's own
		// header and never the OAuth token. Credentials do not leak into each other.
		{"elsewhere.test:443", "Bearer some-api-key"},
		{"unrelated.test:443", "Bearer " + sentinel},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			h := http.Header{}
			h.Set("Authorization", "Bearer "+sentinel)
			if _, err := inj.Apply(context.Background(), mustTarget(t, tc.host), h, FlowTLS); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := h.Get("Authorization"); got != tc.want {
				t.Errorf("Authorization for %s = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

// TestSubstitutionIsLimitedToTheConfiguredHeaders: a guest can put the sentinel anywhere it
// likes, and only the header the credential is actually sent in is swapped. Otherwise an
// origin that echoes an arbitrary request header would hand the guest a real token.
func TestSubstitutionIsLimitedToTheConfiguredHeaders(t *testing.T) {
	record := testRecord(t, time.Now().Add(time.Hour))
	inj, _, _ := testInjector(t, record)

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	h.Set("X-Echo-Me", record.AccessSentinel)
	h.Add("X-Multi", "prefix "+record.AccessSentinel+" suffix")

	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.Get("Authorization"); got != "Bearer "+accessCanary {
		t.Errorf("Authorization = %q, want the real token", got)
	}
	for _, name := range []string{"X-Echo-Me", "X-Multi"} {
		if strings.Contains(h.Get(name), accessCanary) {
			t.Errorf("%s carries the real token: substitution escaped its header allowlist", name)
		}
		if !strings.Contains(h.Get(name), record.AccessSentinel) {
			t.Errorf("%s = %q, want the sentinel left as it was", name, h.Get(name))
		}
	}
}

// TestNoSentinelMeansNoToken: substitution, not assignment. A request the guest sent without
// a sentinel goes out exactly as written.
func TestNoSentinelMeansNoToken(t *testing.T) {
	inj, _, _ := testInjector(t, testRecord(t, time.Now().Add(time.Hour)))

	h := http.Header{}
	h.Set("Authorization", "Bearer something-the-guest-invented")
	used, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.Get("Authorization"); got != "Bearer something-the-guest-invented" {
		t.Errorf("Authorization = %q; a request with no sentinel must be left alone", got)
	}
	if len(used) != 0 {
		t.Errorf("used = %v; nothing was attached", used)
	}
}

// TestOAuthTokenNeverTravelsOnAPlaintextFlow: the guest picks the scheme, and a subscription
// token in the clear would be a downgrade the guest controls.
func TestOAuthTokenNeverTravelsOnAPlaintextFlow(t *testing.T) {
	record := testRecord(t, time.Now().Add(time.Hour))
	inj, _, _ := testInjector(t, record)

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:80"), h, FlowPlaintext); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.Get("Authorization"); got != "Bearer "+record.AccessSentinel {
		t.Errorf("Authorization = %q; an oauth token must not be written onto a plaintext flow", got)
	}
}

// TestExpiredTokenIsRefreshedOnTheHost: the request that finds an expired token gets a fresh
// one, the new pair is saved, and the guest's sentinel is unchanged by any of it.
func TestExpiredTokenIsRefreshedOnTheHost(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Minute)) // already dead
	inj, store, _ := testInjector(t, record)
	stub := &stubRefresher{access: rotatedAccess, refresh: "sk-ant-ort01-ROTATED-REFRESH", ttl: time.Hour}
	inj.SetRefresher(stub)

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if stub.calls != 1 {
		t.Fatalf("the refresher was called %d times, want 1", stub.calls)
	}
	if stub.gotToken != refreshCanary {
		t.Error("the refresher was not given the stored refresh token")
	}
	if got := h.Get("Authorization"); got != "Bearer "+rotatedAccess {
		t.Errorf("Authorization = %q, want the rotated access token", got)
	}

	// The new pair landed on the host.
	saved, err := store.LookupOAuth(context.Background(), record.Service)
	if err != nil {
		t.Fatalf("LookupOAuth: %v", err)
	}
	if saved.Access.Reveal() != rotatedAccess {
		t.Error("the rotated access token was not saved on the host")
	}
	if saved.Refresh.Reveal() != "sk-ant-ort01-ROTATED-REFRESH" {
		t.Error("the rotated refresh token was not saved on the host")
	}

	// And a second request does not refresh again.
	h2 := http.Header{}
	h2.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h2, FlowTLS); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("the refresher was called %d times; a valid token must not be exchanged again", stub.calls)
	}
}

// TestExchangeTokenAnswersWithSentinelsOnly is the refresh decision, tested at the seam the
// guest sees: what comes back is the sentinel pair, so an agent that persists it writes back
// exactly what it already had.
func TestExchangeTokenAnswersWithSentinelsOnly(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Minute))
	inj, store, credential := testInjector(t, record)
	stub := &stubRefresher{access: rotatedAccess, refresh: "sk-ant-ort01-ROTATED-REFRESH", ttl: time.Hour}
	inj.SetRefresher(stub)

	exchange, err := inj.ExchangeToken(context.Background(), credential)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}
	if !exchange.Refreshed {
		t.Error("an expired credential should have been refreshed while answering")
	}

	body := string(exchange.Body)
	for _, forbidden := range []string{accessCanary, refreshCanary, rotatedAccess, "sk-ant-ort01-ROTATED-REFRESH"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the answer to the guest contains a real token:\n%s", body)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(exchange.Body, &parsed); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if parsed["access_token"] != record.AccessSentinel {
		t.Errorf("access_token = %v, want the access sentinel", parsed["access_token"])
	}
	if parsed["refresh_token"] != record.RefreshSentinel {
		t.Errorf("refresh_token = %v, want the refresh sentinel", parsed["refresh_token"])
	}

	// The host, meanwhile, holds the rotated pair.
	saved, err := store.LookupOAuth(context.Background(), record.Service)
	if err != nil {
		t.Fatalf("LookupOAuth: %v", err)
	}
	if saved.Access.Reveal() != rotatedAccess {
		t.Error("the host did not keep the rotated token")
	}

	// A guest asking again while the token is healthy must not make boks burn another
	// refresh: that is how a hostile guest would invalidate a rotating credential.
	if _, err := inj.ExchangeToken(context.Background(), credential); err != nil {
		t.Fatalf("second ExchangeToken: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("the refresher was called %d times; a guest must not be able to force an exchange", stub.calls)
	}
}

// TestTokenEndpointMatchesHostAndPath: a token endpoint's host serves other things, and those
// are ordinary requests.
func TestTokenEndpointMatchesHostAndPath(t *testing.T) {
	record := testRecord(t, time.Now().Add(time.Hour))
	inj, _, _ := testInjector(t, record)

	if _, ok := inj.TokenEndpointFor(mustTarget(t, "console.creds.test:443"), "/v1/oauth/token"); !ok {
		t.Error("the token endpoint was not recognised")
	}
	if _, ok := inj.TokenEndpointFor(mustTarget(t, "console.creds.test:443"), "/v1/other"); ok {
		t.Error("another path on the token endpoint's host was treated as a token request")
	}
	if _, ok := inj.TokenEndpointFor(mustTarget(t, "api.creds.test:443"), "/v1/oauth/token"); ok {
		t.Error("the same path on a different host was treated as a token request")
	}
}

// TestExpiredCredentialWithoutARefreshTokenFailsSafely: no request goes out carrying a dead
// credential, and the error says what to do.
func TestExpiredCredentialWithoutARefreshTokenFailsSafely(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	record.RefreshToken = ""
	inj, _, _ := testInjector(t, record)

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	_, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS)
	if err == nil {
		t.Fatal("an expired credential with no way to renew it was used anyway")
	}
	if got := h.Get("Authorization"); got != "Bearer "+record.AccessSentinel {
		t.Errorf("Authorization = %q; nothing may be attached on the failure path", got)
	}
	if !strings.Contains(err.Error(), "boks secret import") {
		t.Errorf("the error does not say how to recover: %v", err)
	}
	assertNoCanary(t, err.Error())
}

// TestRefreshFailureFailsSafely: the token endpoint refusing must not leave a request going
// out with the dead token, and must not put the credential in the error.
func TestRefreshFailureFailsSafely(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, _ := testInjector(t, record)
	inj.SetRefresher(&stubRefresher{err: errors.New("the endpoint refused the refresh")})

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	_, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS)
	if err == nil {
		t.Fatal("a failed refresh did not fail the request")
	}
	if strings.Contains(h.Get("Authorization"), accessCanary) {
		t.Error("the expired token was attached anyway")
	}
	assertNoCanary(t, err.Error())
}

// TestCorruptStoredCredentialFailsSafely covers the three ways a stored record can be wrong.
func TestCorruptStoredCredentialFailsSafely(t *testing.T) {
	if _, err := decodeOAuth("claude-code", oauthMarker+"{not json"); err == nil {
		t.Error("a corrupt record decoded without complaint")
	} else if strings.Contains(err.Error(), "not json") {
		t.Errorf("the error quotes the stored payload, which holds the tokens: %v", err)
	}

	if _, err := decodeOAuth("claude-code", "sk-plain-api-key"); err == nil {
		t.Error("a plain secret was read as an oauth record")
	}

	if _, err := decodeOAuth("claude-code", oauthMarker+`{"v":99,"service":"x"}`); err == nil {
		t.Error("a record from a future version was accepted")
	}

	// A record that parses but could never work is refused before it is stored.
	bad := testRecord(t, time.Now())
	bad.ResourceHosts = []string{"*"}
	if err := bad.Validate(); err == nil {
		t.Error("a catch-all resource host was accepted")
	}
	bad = testRecord(t, time.Now())
	bad.AccessSentinel = "short"
	if err := bad.Validate(); err == nil {
		t.Error("a sentinel too short to be safe for substitution was accepted")
	}
	bad = testRecord(t, time.Now())
	bad.AccessToken = ""
	if err := bad.Validate(); err == nil {
		t.Error("a record with no access token was accepted")
	}
}

// TestOAuthValuesNeverReachALogOrAnError drives every path that produces a message and
// asserts that neither a token nor a sentinel appears in any of them.
//
// The sentinel is included deliberately. It is not a secret, but the moment one shows up in
// a log a reader can no longer tell by looking whether what they are seeing is real, and the
// property this package sells is exactly that they can.
func TestOAuthValuesNeverReachALogOrAnError(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, store, credential := testInjector(t, record)

	var buf strings.Builder
	logger := log.New(&buf, "", 0)

	// Every printed form of everything that holds a token.
	tokens := record.Tokens()
	logger.Printf("record=%v recordgo=%#v recordstr=%s", record, record, record)
	logger.Printf("tokens=%v tokensgo=%#v tokensstr=%s", tokens, tokens, tokens)
	logger.Printf("credential=%v credentialstr=%s", credential, credential)
	logger.Printf("oauth=%v", credential.OAuth)
	encoded, err := json.Marshal(struct {
		Tokens OAuthTokens
	}{tokens})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	logger.Printf("json=%s", encoded)

	// The failure paths.
	inj.SetRefresher(&stubRefresher{err: fmt.Errorf("refused")})
	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS); err != nil {
		logger.Printf("apply=%v", err)
	} else {
		t.Error("expected the refresh to fail")
	}
	if _, err := inj.ExchangeToken(context.Background(), credential); err != nil {
		logger.Printf("exchange=%v", err)
	}
	if _, err := store.LookupOAuth(context.Background(), "nothing-here"); err != nil {
		logger.Printf("lookup=%v", err)
	}
	if _, err := decodeOAuth("claude-code", oauthMarker+"{broken"); err != nil {
		logger.Printf("decode=%v", err)
	}
	// A token endpoint that answers with rubbish: the body is the one place a token is
	// guaranteed to be, so the error must not quote it.
	if _, err := ParseTokenResponse([]byte(`{"oops":"`+accessCanary+`"}`), ResponseFields{}, time.Now()); err != nil {
		logger.Printf("parse=%v", err)
	}
	if _, err := ParseTokenResponse([]byte(accessCanary), ResponseFields{}, time.Now()); err != nil {
		logger.Printf("parseraw=%v", err)
	}

	assertNoCanary(t, buf.String())
	if !strings.Contains(buf.String(), Redacted) {
		t.Errorf("nothing was redacted, which suggests nothing was printed:\n%s", buf.String())
	}
}

func assertNoCanary(t *testing.T, s string) {
	t.Helper()
	sentinel := NewSentinel("sk-ant-oat01-", "claude-code-access", 108)
	for name, canary := range map[string]string{
		"the access token":  accessCanary,
		"the refresh token": refreshCanary,
		"the rotated token": rotatedAccess,
		"the sentinel":      sentinel,
	} {
		if strings.Contains(s, canary) {
			t.Errorf("%s appears in output that must never carry one:\n%s", name, s)
		}
	}
}

// TestCredentialFileHoldsSentinelsOnly: the file the guest reads is rendered from a data
// struct with nowhere to put a real token, and this asserts the result.
func TestCredentialFileHoldsSentinelsOnly(t *testing.T) {
	record := testRecord(t, time.Now().Add(time.Hour))
	credential, err := record.Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	files, err := CredentialFiles([]Credential{credential})
	if err != nil {
		t.Fatalf("CredentialFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d credential files, want 1", len(files))
	}
	f := files[0]
	if f.Path != GuestCredentialDir+"/claude-code.json" {
		t.Errorf("path = %q", f.Path)
	}
	content := string(f.Content)
	assertNoCanaryTokensOnly(t, content)

	// It must be the document Claude Code expects, or the agent will not read it.
	var doc claudeCodeDocument
	if err := json.Unmarshal(f.Content, &doc); err != nil {
		t.Fatalf("the rendered file is not a Claude Code credential document: %v\n%s", err, content)
	}
	if doc.OAuth.AccessToken != record.AccessSentinel {
		t.Error("the rendered file does not carry the access sentinel")
	}
	if doc.OAuth.RefreshToken != record.RefreshSentinel {
		t.Error("the rendered file does not carry the refresh sentinel")
	}
	if doc.OAuth.SubscriptionType != "max" || len(doc.OAuth.Scopes) != 2 {
		t.Errorf("the non-secret metadata did not survive rendering: %+v", doc.OAuth)
	}
	// The expiry the guest is told is far away on purpose: expiry is a host concern.
	if got := time.UnixMilli(doc.OAuth.ExpiresAt); !got.After(time.Now().Add(300 * 24 * time.Hour)) {
		t.Errorf("expiresAt = %s; the guest should not be invited to refresh", got)
	}
}

// assertNoCanaryTokensOnly checks for real tokens but permits sentinels, which is the whole
// point of a credential file.
func assertNoCanaryTokensOnly(t *testing.T, s string) {
	t.Helper()
	for _, canary := range []string{accessCanary, refreshCanary, rotatedAccess} {
		if strings.Contains(s, canary) {
			t.Fatalf("a real token appears in content bound for the guest:\n%s", s)
		}
	}
}

// TestOAuthCredentialDecidesInterception: the resource hosts and the token endpoint are
// credential hosts, and nothing else becomes one.
func TestOAuthCredentialDecidesInterception(t *testing.T) {
	inj, _, _ := testInjector(t, testRecord(t, time.Now().Add(time.Hour)))

	for _, host := range []string{"api.creds.test:443", "console.creds.test:443"} {
		if !inj.Handles(mustTarget(t, host)) {
			t.Errorf("%s is not treated as a credential host, so its flow would never be terminated", host)
		}
	}
	if inj.Handles(mustTarget(t, "unrelated.test:443")) {
		t.Error("a host with no credential became a credential host")
	}
	hosts := inj.Hosts()
	want := map[string]bool{"api.creds.test": true, "console.creds.test": true}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v, want exactly %v", hosts, want)
	}
	for _, h := range hosts {
		if !want[h] {
			t.Errorf("%s would be decrypted and should not be", h)
		}
	}
}

// TestParseTokenResponse covers the shapes a token endpoint answers with.
func TestParseTokenResponse(t *testing.T) {
	now := time.Now()
	tokens, err := ParseTokenResponse([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600}`), ResponseFields{}, now)
	if err != nil {
		t.Fatalf("ParseTokenResponse: %v", err)
	}
	if tokens.Access.Reveal() != "a" || tokens.Refresh.Reveal() != "r" {
		t.Error("the pair was not read out")
	}
	if got := tokens.Expiry.Sub(now); got != time.Hour {
		t.Errorf("expiry is %s away, want 1h", got)
	}

	// Field-name overrides, for a provider that spells them differently.
	tokens, err = ParseTokenResponse([]byte(`{"token":"a","ttl":60}`),
		ResponseFields{AccessToken: "token", ExpiresIn: "ttl"}, now)
	if err != nil {
		t.Fatalf("ParseTokenResponse with overrides: %v", err)
	}
	if tokens.Access.Reveal() != "a" {
		t.Error("the override field name was not used")
	}

	if _, err := ParseTokenResponse([]byte(`{"refresh_token":"r"}`), ResponseFields{}, now); err == nil {
		t.Error("a response with no access token was accepted")
	}
}
