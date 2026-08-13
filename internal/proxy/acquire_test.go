package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// The canaries for acquisition: what a token endpoint mints when the agent inside the sandbox
// completes its own login. Neither may appear in anything the guest receives, in any log, or
// in any decision.
const (
	mintedAccess  = "sk-ant-oat01-MINTED-BY-THE-ORIGIN-CANARY"
	mintedRefresh = "sk-ant-ort01-MINTED-BY-THE-ORIGIN-CANARY"
)

// tokenResponse is the shape Anthropic's endpoint actually returns, observed by driving the
// real Claude Code 2.1.228 against a stand-in origin. The fields beyond the token pair are
// here because the agent reads them back and writes them into its own credential file, so
// masking has to preserve them — see docs/verification.md.
func tokenResponse() string {
	return fmt.Sprintf(`{"token_type":"Bearer","access_token":%q,"refresh_token":%q,`+
		`"expires_in":28800,"scope":"user:inference user:profile",`+
		`"account":{"email_address":"someone@example.test"},"organization":{"name":"an org"}}`,
		mintedAccess, mintedRefresh)
}

// armedRecord is what `boks secret login` writes: the shape, the sentinels, and no tokens.
func armedRecord() secret.OAuthRecord {
	r := oauthRecord(time.Time{})
	r.AccessToken, r.RefreshToken = "", ""
	r.Pending = true
	return r
}

// loginOrigin stands in for the vendor's token endpoint during a login. It records what the
// guest sent and answers with a freshly minted pair.
func loginOrigin(t *testing.T, authority interface {
	LeafFor(string) (*tls.Certificate, error)
}, seen *atomic.Value, count *atomic.Int32) *httptest.Server {
	t.Helper()
	leaf, err := authority.LeafFor("console.creds.test")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		body, _ := io.ReadAll(r.Body)
		seen.Store(string(body) + "\n" + r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, tokenResponse())
	}))
	srv.EnableHTTP2 = false
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestOAuthLoginInsideTheSandboxIsKeptOnTheHost is the acquisition path end to end, and it is
// the test the whole feature exists to pass.
//
// The guest performs the exchange the real agent performs — an authorization_code grant with a
// PKCE verifier, neither of which boks could have composed, because both came from a redirect
// only the guest saw. The origin mints a real pair. What comes back through the proxy is the
// origin's own document with sentinels where the tokens were, and the real pair is on the host.
func TestOAuthLoginInsideTheSandboxIsKeptOnTheHost(t *testing.T) {
	webCA := newAuthority(t)
	var (
		sentToOrigin atomic.Value
		exchanges    atomic.Int32
	)
	tokenSrv := loginOrigin(t, webCA, &sentToOrigin, &exchanges)

	record := armedRecord()
	inj, store := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow console.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	// Verbatim from the observed exchange, with the code and verifier replaced.
	const guestBody = `{"grant_type":"authorization_code","code":"an-authorization-code",` +
		`"redirect_uri":"https://console.creds.test/oauth/code/callback",` +
		`"client_id":"public-client-id","code_verifier":"a-pkce-verifier","state":"a-state"}`

	url := hostPort(t, tokenSrv.URL, "console.creds.test") + "/v1/oauth/token"
	req, err := http.NewRequest("POST", url, strings.NewReader(guestBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client(pool(boksCA)).Do(req)
	if err != nil {
		t.Fatalf("the login exchange failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 1. The guest's request was relayed, unchanged apart from the encoding boks has to be
	//    able to read. This is the difference from a refresh, which is never forwarded.
	if exchanges.Load() != 1 {
		t.Fatalf("the token endpoint saw %d exchanges, want exactly 1", exchanges.Load())
	}
	relayed, _ := sentToOrigin.Load().(string)
	if !strings.Contains(relayed, guestBody) {
		t.Fatalf("the origin did not receive the guest's own exchange:\n%s", relayed)
	}
	if !strings.Contains(relayed, "\nidentity") {
		t.Errorf("the relayed request did not ask for an identity encoding:\n%s", relayed)
	}

	// 2. The guest received sentinels, and no real token in any form.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	if answer["access_token"] != record.AccessSentinel {
		t.Errorf("access_token = %v, want the sentinel", answer["access_token"])
	}
	if answer["refresh_token"] != record.RefreshSentinel {
		t.Errorf("refresh_token = %v, want the sentinel", answer["refresh_token"])
	}
	for _, canary := range []string{mintedAccess, mintedRefresh} {
		if strings.Contains(string(body), canary) {
			t.Fatalf("a real token reached the guest:\n%s", body)
		}
	}

	// 3. The origin's own shape survived. The agent writes these into its credential file,
	//    and a reply composed from scratch would have dropped them.
	if answer["scope"] != "user:inference user:profile" {
		t.Errorf("scope = %v; the origin's own fields must survive masking", answer["scope"])
	}
	if answer["expires_in"] != float64(28800) || answer["token_type"] != "Bearer" {
		t.Errorf("the origin's expiry or token type was lost: %v", answer)
	}
	if account, ok := answer["account"].(map[string]any); !ok || account["email_address"] != "someone@example.test" {
		t.Errorf("a nested field the origin sent was lost: %v", answer["account"])
	}

	// 4. The host has the real pair, and it is no longer awaiting a login.
	saved, err := store.LookupOAuth(context.Background(), record.Service)
	if err != nil {
		t.Fatalf("LookupOAuth: %v", err)
	}
	if saved.Access.Reveal() != mintedAccess {
		t.Error("the host did not keep the minted access token")
	}
	if saved.Refresh.Reveal() != mintedRefresh {
		t.Error("the host did not keep the minted refresh token")
	}
	if held := store.Records()[record.Service]; held.Pending {
		t.Error("the credential is still marked pending after a successful acquisition")
	}
	if saved.Expiry.IsZero() {
		t.Error("the expiry the origin stated was not kept")
	}

	assertMode(t, p.Server, "console.creds.test", policy.ModeForward)
	assertNoAcquisitionCanary(t, p, record)

	// The transcript, for a reader who wants to see the substitution rather than be told
	// about it. `go test -run TestOAuthLoginInsideTheSandboxIsKeptOnTheHost -v ./internal/proxy`.
	t.Logf("the guest sent, and the origin received:\n%s", guestBody)
	t.Logf("the origin answered:\n%s", tokenResponse())
	t.Logf("the guest received:\n%s", body)
}

// TestAcquisitionHappensOnceAndThenTheEndpointIsAnswered: the agent's very next act after a
// login is a refresh with what it was just given. That one must not be relayed — the door
// closes with the first token.
func TestAcquisitionHappensOnceAndThenTheEndpointIsAnswered(t *testing.T) {
	webCA := newAuthority(t)
	var (
		sentToOrigin atomic.Value
		exchanges    atomic.Int32
	)
	tokenSrv := loginOrigin(t, webCA, &sentToOrigin, &exchanges)

	record := armedRecord()
	inj, _ := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow console.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})
	client := p.client(pool(boksCA))
	url := hostPort(t, tokenSrv.URL, "console.creds.test") + "/v1/oauth/token"

	first, err := client.Post(url, "application/json",
		strings.NewReader(`{"grant_type":"authorization_code","code":"c","code_verifier":"v"}`))
	if err != nil {
		t.Fatalf("the login exchange failed: %v", err)
	}
	first.Body.Close()

	// Exactly what Claude Code 2.1.228 does next, observed: a refresh_token grant carrying
	// what the exchange returned — which, here, is the sentinel.
	second, err := client.Post(url, "application/json",
		strings.NewReader(fmt.Sprintf(`{"grant_type":"refresh_token","refresh_token":%q}`, record.RefreshSentinel)))
	if err != nil {
		t.Fatalf("the follow-up refresh failed: %v", err)
	}
	defer second.Body.Close()
	body, _ := io.ReadAll(second.Body)

	if exchanges.Load() != 1 {
		t.Fatalf("the token endpoint saw %d exchanges; the second must be answered, not relayed", exchanges.Load())
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	if answer["access_token"] != record.AccessSentinel {
		t.Errorf("access_token = %v, want the same sentinel", answer["access_token"])
	}
	assertNoAcquisitionCanary(t, p, record)
}

// TestAcquisitionIsRefusedForACredentialThatAlreadyHasAToken: relaying is decided by the
// stored record, so a credential that is not pending never forwards, whatever the guest sends.
func TestAcquisitionIsRefusedForACredentialThatAlreadyHasAToken(t *testing.T) {
	webCA := newAuthority(t)
	var (
		sentToOrigin atomic.Value
		exchanges    atomic.Int32
	)
	tokenSrv := loginOrigin(t, webCA, &sentToOrigin, &exchanges)

	record := oauthRecord(time.Now().Add(time.Hour)) // healthy, not pending
	inj, _ := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow console.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	url := hostPort(t, tokenSrv.URL, "console.creds.test") + "/v1/oauth/token"
	// A guest trying to talk boks into the relay path by sending an acquisition-shaped body.
	resp, err := p.client(pool(boksCA)).Post(url, "application/json",
		strings.NewReader(`{"grant_type":"authorization_code","code":"c","code_verifier":"v"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if exchanges.Load() != 0 {
		t.Fatal("a credential that already holds a token had its request relayed")
	}
	if strings.Contains(string(body), mintedAccess) {
		t.Fatal("the origin's token reached the guest")
	}
	assertNoAcquisitionCanary(t, p, record)
}

// TestAcquisitionPassesAFailedLoginThrough: a login that the origin rejects has to reach the
// agent, or the user is told nothing. There is no token in such an answer to mask.
func TestAcquisitionPassesAFailedLoginThrough(t *testing.T) {
	webCA := newAuthority(t)
	leaf, err := webCA.LeafFor("console.creds.test")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	tokenSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"the code has been used"}`)
	}))
	tokenSrv.EnableHTTP2 = false
	tokenSrv.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
	tokenSrv.StartTLS()
	defer tokenSrv.Close()

	record := armedRecord()
	inj, store := oauthInjector(t, record)
	boksCA := newAuthority(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow console.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	url := hostPort(t, tokenSrv.URL, "console.creds.test") + "/v1/oauth/token"
	resp, err := p.client(pool(boksCA)).Post(url, "application/json",
		strings.NewReader(`{"grant_type":"authorization_code","code":"spent"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want the origin's own 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "invalid_grant") {
		t.Errorf("the agent cannot tell the user what went wrong:\n%s", body)
	}
	// Nothing was stored, so the credential is still armed and the user can try again.
	if held := store.Records()[record.Service]; !held.Pending || held.AccessToken != "" {
		t.Error("a failed login changed the stored credential")
	}
	assertNoAcquisitionCanary(t, p, record)
}

// assertNoAcquisitionCanary is assertNoOAuthCanary for the minted pair: the proxy may record
// that a credential was acquired, by name, and nothing else about it.
func assertNoAcquisitionCanary(t *testing.T, p *testProxy, record secret.OAuthRecord) {
	t.Helper()
	logged := p.logBuf.String()
	var decisions strings.Builder
	for _, d := range p.Engine().Log().Recent(0) {
		fmt.Fprintf(&decisions, "%+v\n%s\n", d, d.String())
	}
	for name, canary := range map[string]string{
		"the minted access token":  mintedAccess,
		"the minted refresh token": mintedRefresh,
		"the access sentinel":      record.AccessSentinel,
		"the refresh sentinel":     record.RefreshSentinel,
	} {
		if strings.Contains(logged, canary) {
			t.Errorf("%s is in the proxy's operational log:\n%s", name, logged)
		}
		if strings.Contains(decisions.String(), canary) {
			t.Errorf("%s is in the decision log:\n%s", name, decisions.String())
		}
	}
}
