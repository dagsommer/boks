package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// The canaries. None of these may appear in a log, in a decision, or in anything the guest
// receives.
const (
	oauthAccess  = "sk-ant-oat01-REAL-ACCESS-CANARY"
	oauthRefresh = "sk-ant-ort01-REAL-REFRESH-CANARY"
	oauthRotated = "sk-ant-oat01-ROTATED-ACCESS-CANARY"
)

// oauthRecord builds the stored credential: used on api.creds.test, refreshed at
// console.creds.test, with sentinels shaped like the real tokens.
func oauthRecord(expiry time.Time) secret.OAuthRecord {
	r := secret.OAuthRecord{
		V:               secret.OAuthRecordVersion,
		Service:         "claude-code",
		AccessToken:     oauthAccess,
		RefreshToken:    oauthRefresh,
		Scopes:          []string{"user:inference"},
		Subscription:    "max",
		TokenHost:       "console.creds.test",
		TokenPath:       "/v1/oauth/token",
		ClientID:        "public-client-id",
		Encoding:        secret.EncodingJSON,
		ResourceHosts:   []string{"api.creds.test"},
		Headers:         []string{"Authorization"},
		AccessSentinel:  secret.NewSentinel("sk-ant-oat01-", "claude-code-access", 108),
		RefreshSentinel: secret.NewSentinel("sk-ant-ort01-", "claude-code-refresh", 108),
		EnvName:         "CLAUDE_CODE_OAUTH_TOKEN",
	}
	if !expiry.IsZero() {
		r.ExpiresAt = expiry.UnixMilli()
	}
	return r
}

// oauthInjector binds the record to a store that is provider, oauth provider and saver at
// once — the roles the encrypted file store plays for real.
func oauthInjector(t *testing.T, record secret.OAuthRecord) (*secret.Injector, *secret.MemoryStore) {
	t.Helper()
	store := secret.NewMemoryStore(nil, map[string]secret.OAuthRecord{record.Service: record})
	credential, err := record.Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	inj, err := secret.NewInjector(store, credential)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return inj, store
}

// echoOrigin answers with the Authorization header it received, so a test can see exactly
// what reached the far end.
func echoOrigin(t *testing.T, authority interface {
	LeafFor(string) (*tls.Certificate, error)
}, host string, seen *string) *httptest.Server {
	t.Helper()
	leaf, err := authority.LeafFor(host)
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Get("Authorization")
		fmt.Fprintf(w, "authorization=%s", r.Header.Get("Authorization"))
	}))
	srv.EnableHTTP2 = false
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// hostRefresher is the host-side token exchange, pointed at a test endpoint. It is the real
// HTTPRefresher: the request it composes, the JSON it posts and the response it parses are
// all production code.
func hostRefresher(tokenAddr string, roots *tls.Config) secret.Refresher {
	return secret.HTTPRefresher{Client: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", tokenAddr)
			},
			TLSClientConfig: roots,
		},
	}}
}

// TestOAuthSentinelBecomesARealTokenAtTheOrigin is the feature, end to end: the guest sends
// a sentinel over HTTPS and the origin receives the subscription token, which never existed
// inside the guest.
func TestOAuthSentinelBecomesARealTokenAtTheOrigin(t *testing.T) {
	webCA := newAuthority(t)
	var received string
	origin := echoOrigin(t, webCA, "api.creds.test", &received)

	record := oauthRecord(time.Now().Add(time.Hour))
	inj, _ := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	req, err := http.NewRequest("GET", hostPort(t, origin.URL, "api.creds.test"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// This is all the guest holds: a sentinel from its environment or its credential file.
	req.Header.Set("Authorization", "Bearer "+record.AccessSentinel)

	resp, err := p.client(pool(boksCA)).Do(req)
	if err != nil {
		t.Fatalf("HTTPS through the proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if received != "Bearer "+oauthAccess {
		t.Fatalf("the origin received %q, want the real access token", received)
	}
	if string(body) != "authorization=Bearer "+oauthAccess {
		t.Errorf("echoed body = %q", body)
	}
	assertMode(t, p.Server, "api.creds.test", policy.ModeForward)
	assertNoOAuthCanary(t, p, record)
}

// TestOAuthTokenIsNotSubstitutedElsewhere: the same sentinel, sent to a host with no
// credential, stays a sentinel — and that flow is not even intercepted.
func TestOAuthTokenIsNotSubstitutedElsewhere(t *testing.T) {
	webCA := newAuthority(t)
	var received string
	origin := echoOrigin(t, webCA, "plain.test", &received)

	record := oauthRecord(time.Now().Add(time.Hour))
	inj, _ := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow plain.test", "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	req, err := http.NewRequest("GET", hostPort(t, origin.URL, "plain.test"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+record.AccessSentinel)

	// The client trusts both authorities, so the test has to look at which certificate
	// arrived rather than at whether the handshake worked.
	resp, err := p.client(pool(webCA, boksCA)).Do(req)
	if err != nil {
		t.Fatalf("HTTPS through the proxy: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if received != "Bearer "+record.AccessSentinel {
		t.Fatalf("the origin received %q; a host with no credential must see the sentinel unchanged", received)
	}
	if resp.TLS.PeerCertificates[0].Issuer.CommonName != webCA.Certificate().Subject.CommonName {
		t.Error("a host with no credential was intercepted")
	}
	assertMode(t, p.Server, "plain.test", policy.ModeForwardBypass)
}

// TestOAuthRefreshHappensOnTheHost is the refresh decision demonstrated: an expired
// credential is exchanged by the host, against the real token endpoint, and the origin
// receives the rotated token — while the guest sent, and still holds, the same sentinel.
func TestOAuthRefreshHappensOnTheHost(t *testing.T) {
	webCA := newAuthority(t)
	var received string
	origin := echoOrigin(t, webCA, "api.creds.test", &received)

	var (
		exchanges  atomic.Int32
		gotRefresh atomic.Value
	)
	tokenLeaf, err := webCA.LeafFor("console.creds.test")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	tokenSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		body, _ := io.ReadAll(r.Body)
		gotRefresh.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"sk-ant-ort01-ROTATED-REFRESH","expires_in":3600}`, oauthRotated)
	}))
	tokenSrv.EnableHTTP2 = false
	tokenSrv.TLS = &tls.Config{Certificates: []tls.Certificate{*tokenLeaf}}
	tokenSrv.StartTLS()
	defer tokenSrv.Close()

	record := oauthRecord(time.Now().Add(-time.Minute)) // expired
	inj, store := oauthInjector(t, record)
	inj.SetRefresher(hostRefresher(tokenSrv.Listener.Addr().String(), &tls.Config{RootCAs: pool(webCA)}))

	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	req, err := http.NewRequest("GET", hostPort(t, origin.URL, "api.creds.test"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+record.AccessSentinel)
	resp, err := p.client(pool(boksCA)).Do(req)
	if err != nil {
		t.Fatalf("HTTPS through the proxy: %v", err)
	}
	resp.Body.Close()

	if exchanges.Load() != 1 {
		t.Fatalf("the token endpoint saw %d exchanges, want exactly 1", exchanges.Load())
	}
	// The exchange the host performed carried the real refresh token and no guest input.
	sent, _ := gotRefresh.Load().(string)
	if !strings.Contains(sent, oauthRefresh) || !strings.Contains(sent, "refresh_token") {
		t.Errorf("the host's exchange did not carry the stored refresh token")
	}
	if received != "Bearer "+oauthRotated {
		t.Fatalf("the origin received %q, want the rotated access token", received)
	}
	// The rotated pair is on the host.
	saved, err := store.LookupOAuth(context.Background(), record.Service)
	if err != nil {
		t.Fatalf("LookupOAuth: %v", err)
	}
	if saved.Access.Reveal() != oauthRotated {
		t.Error("the host did not keep the rotated access token")
	}
	if saved.Refresh.Reveal() != "sk-ant-ort01-ROTATED-REFRESH" {
		t.Error("the host did not keep the rotated refresh token")
	}
	// And the guest is exactly where it was.
	if req.Header.Get("Authorization") != "Bearer "+record.AccessSentinel {
		t.Error("the guest's own request object was mutated")
	}
	assertNoOAuthCanary(t, p, record)
}

// TestOAuthTokenRequestIsAnsweredNotForwarded is the other half of the refresh decision: a
// guest that asks for new tokens gets sentinels, and its request never reaches the token
// endpoint at all.
func TestOAuthTokenRequestIsAnsweredNotForwarded(t *testing.T) {
	webCA := newAuthority(t)
	var reached atomic.Int32
	tokenLeaf, err := webCA.LeafFor("console.creds.test")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	tokenSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		fmt.Fprint(w, `{"access_token":"MUST-NOT-BE-REACHED"}`)
	}))
	tokenSrv.EnableHTTP2 = false
	tokenSrv.TLS = &tls.Config{Certificates: []tls.Certificate{*tokenLeaf}}
	tokenSrv.StartTLS()
	defer tokenSrv.Close()

	record := oauthRecord(time.Now().Add(time.Hour)) // healthy: no exchange is needed
	inj, _ := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow console.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	// Exactly what an agent does when it thinks its token has expired.
	url := hostPort(t, tokenSrv.URL, "console.creds.test") + "/v1/oauth/token"
	payload := fmt.Sprintf(`{"grant_type":"refresh_token","refresh_token":%q}`, record.RefreshSentinel)
	resp, err := p.client(pool(boksCA)).Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST to the token endpoint: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if reached.Load() != 0 {
		t.Fatal("the guest's token request was forwarded to the token endpoint")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	if answer["access_token"] != record.AccessSentinel {
		t.Errorf("access_token = %v, want the sentinel the guest already holds", answer["access_token"])
	}
	if answer["refresh_token"] != record.RefreshSentinel {
		t.Errorf("refresh_token = %v, want the sentinel the guest already holds", answer["refresh_token"])
	}
	for _, canary := range []string{oauthAccess, oauthRefresh, oauthRotated} {
		if strings.Contains(string(body), canary) {
			t.Fatalf("a real token was sent to the guest:\n%s", body)
		}
	}
	// The credential file the guest would rewrite from this answer is byte-identical to
	// the one it already has, which is what makes the read-only mount safe.
	assertNoOAuthCanary(t, p, record)
}

// TestOAuthTokenEndpointHostServesOtherThingsNormally: only the configured path is answered.
func TestOAuthTokenEndpointHostServesOtherThingsNormally(t *testing.T) {
	webCA := newAuthority(t)
	var received string
	origin := echoOrigin(t, webCA, "console.creds.test", &received)

	record := oauthRecord(time.Now().Add(time.Hour))
	inj, _ := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow console.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	req, err := http.NewRequest("POST",
		hostPort(t, origin.URL, "console.creds.test")+"/v1/something-else", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+record.AccessSentinel)
	resp, err := p.client(pool(boksCA)).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	if received != "Bearer "+record.AccessSentinel {
		t.Errorf("the origin received %q; the token endpoint's host is not a resource host", received)
	}
}

// TestExpiredOAuthCredentialFailsReadably: with nothing to refresh with, the request is
// refused with an explanation instead of being sent with a dead token.
func TestExpiredOAuthCredentialFailsReadably(t *testing.T) {
	webCA := newAuthority(t)
	var received string
	origin := echoOrigin(t, webCA, "api.creds.test", &received)

	record := oauthRecord(time.Now().Add(-time.Hour))
	record.RefreshToken = ""
	inj, _ := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	req, err := http.NewRequest("GET", hostPort(t, origin.URL, "api.creds.test"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+record.AccessSentinel)
	resp, err := p.client(pool(boksCA)).Do(req)
	if err != nil {
		t.Fatalf("expected a readable refusal, got a transport error: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if received != "" {
		t.Errorf("a request reached the origin with %q", received)
	}
	if !strings.Contains(string(body), "boks secret import") {
		t.Errorf("the refusal does not say how to recover:\n%s", body)
	}
	assertNoOAuthCanary(t, p, record)
	for _, canary := range []string{oauthAccess, oauthRefresh} {
		if strings.Contains(string(body), canary) {
			t.Errorf("the refusal body carries a token:\n%s", body)
		}
	}
}

// assertNoOAuthCanary is the log assertion, extended to sentinels. The proxy may record that
// a credential was used, by name; it may record nothing else about it.
func assertNoOAuthCanary(t *testing.T, p *testProxy, record secret.OAuthRecord) {
	t.Helper()
	logged := p.logBuf.String()
	var decisions strings.Builder
	for _, d := range p.Engine().Log().Recent(0) {
		fmt.Fprintf(&decisions, "%+v\n%s\n", d, d.String())
	}
	for name, canary := range map[string]string{
		"the access token":     oauthAccess,
		"the refresh token":    oauthRefresh,
		"the rotated token":    oauthRotated,
		"the access sentinel":  record.AccessSentinel,
		"the refresh sentinel": record.RefreshSentinel,
	} {
		if strings.Contains(logged, canary) {
			t.Errorf("%s is in the proxy's operational log:\n%s", name, logged)
		}
		if strings.Contains(decisions.String(), canary) {
			t.Errorf("%s is in the decision log:\n%s", name, decisions.String())
		}
	}
}
