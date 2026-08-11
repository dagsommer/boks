package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// newAuthority makes a throwaway CA in a temporary directory. Two are used in most tests
// here: one standing in for the public web's trust store, one belonging to Boks.
func newAuthority(t *testing.T) *ca.Authority {
	t.Helper()
	a, err := ca.Create(t.TempDir())
	if err != nil {
		t.Fatalf("ca.Create: %v", err)
	}
	return a
}

// newOrigin starts an HTTPS server whose certificate is issued for host by authority, so
// that a test origin can be verified exactly as strictly as a real one.
func newOrigin(t *testing.T, authority *ca.Authority, host string, handler http.Handler) *httptest.Server {
	t.Helper()
	leaf, err := authority.LeafFor(host)
	if err != nil {
		t.Fatalf("LeafFor(%q): %v", host, err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = false
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// pool builds a trust store from several authorities, standing in for what a guest is
// configured to trust.
func pool(authorities ...*ca.Authority) *x509.CertPool {
	p := x509.NewCertPool()
	for _, a := range authorities {
		p.AddCert(a.Certificate())
	}
	return p
}

// mustInjector builds an injector from injection specs, grouping them by service exactly
// as the CLI does.
func mustInjector(t *testing.T, provider secret.Provider, specs ...string) *secret.Injector {
	t.Helper()
	var order []string
	byService := map[string]*secret.Credential{}
	for _, spec := range specs {
		service, rules, err := secret.ParseInject(spec)
		if err != nil {
			t.Fatalf("ParseInject(%q): %v", spec, err)
		}
		if _, ok := byService[service]; !ok {
			byService[service] = &secret.Credential{Service: service}
			order = append(order, service)
		}
		byService[service].Inject = append(byService[service].Inject, rules...)
	}
	creds := make([]secret.Credential, 0, len(order))
	for _, name := range order {
		creds = append(creds, *byService[name])
	}
	inj, err := secret.NewInjector(provider, creds...)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return inj
}

// TestInspectedFlowInjectsIntoHTTPS is the property the whole feature exists for: the guest
// sends a placeholder over HTTPS, and the origin receives the real credential.
func TestInspectedFlowInjectsIntoHTTPS(t *testing.T) {
	const value = "sk-ant-real-value"

	var (
		gotKey  string
		gotAuth []string
		gotSNI  string
	)
	webCA := newAuthority(t)
	origin := newOrigin(t, webCA, "api.creds.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Values("Authorization")
		if r.TLS != nil {
			gotSNI = r.TLS.ServerName
		}
		fmt.Fprint(w, "origin answered")
	}))

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"anthropic": value}, "anthropic@api.creds.test=x-api-key")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	// The guest trusts the Boks CA, which is the entire cost of the feature.
	client := p.client(pool(boksCA))
	req, err := http.NewRequest("GET", hostPort(t, origin.URL, "api.creds.test"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Api-Key", "BOKS_PLACEHOLDER")
	req.Header.Set("Authorization", "Bearer BOKS_PLACEHOLDER")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS through the proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "origin answered" {
		t.Fatalf("body = %q", body)
	}

	if gotKey != value {
		t.Errorf("origin received X-Api-Key %q, want the injected credential", gotKey)
	}
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer BOKS_PLACEHOLDER" {
		t.Errorf("Authorization = %q; a header no rule touches must pass through unchanged", gotAuth)
	}
	if gotSNI != "api.creds.test" {
		t.Errorf("origin saw SNI %q; the re-originated connection must name the real host", gotSNI)
	}

	// The client is talking to a Boks certificate, not the origin's.
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate on the response")
	}
	issuer := resp.TLS.PeerCertificates[0].Issuer.CommonName
	if issuer != boksCA.Certificate().Subject.CommonName {
		t.Errorf("client saw a certificate issued by %q, want the boks CA %q",
			issuer, boksCA.Certificate().Subject.CommonName)
	}
	assertMode(t, p.Server, "api.creds.test", policy.ModeForward)
}

// TestUnconfiguredHostIsTunnelledWithItsOwnCertificate is the other half of the guarantee:
// a host without a credential rule is carried blind, and the client validates the origin's
// real chain. The two assertions together are what makes interception "opt-in per
// destination" a fact rather than a claim.
func TestUnconfiguredHostIsTunnelledWithItsOwnCertificate(t *testing.T) {
	webCA := newAuthority(t)
	var sawAnyCredential bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != "" {
			sawAnyCredential = true
		}
		fmt.Fprint(w, "plain origin")
	})
	origin := newOrigin(t, webCA, "plain.test", handler)

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"tok": "sk-must-not-travel"}, "tok@api.creds.test=x-api-key")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow plain.test", "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	// The client trusts both authorities, so a substituted certificate would still
	// verify — the test has to look at *which* certificate arrived, not at whether the
	// handshake worked.
	resp, err := p.client(pool(webCA, boksCA)).Get(hostPort(t, origin.URL, "plain.test"))
	if err != nil {
		t.Fatalf("HTTPS through the proxy: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if sawAnyCredential {
		t.Error("a credential reached a host with no rule")
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate on the response")
	}
	leaf := resp.TLS.PeerCertificates[0]
	if leaf.Issuer.CommonName != webCA.Certificate().Subject.CommonName {
		t.Errorf("client saw a certificate issued by %q, want the origin's own CA %q: the flow was intercepted",
			leaf.Issuer.CommonName, webCA.Certificate().Subject.CommonName)
	}
	if err := leaf.CheckSignatureFrom(webCA.Certificate()); err != nil {
		t.Errorf("the certificate the client saw is not the origin's: %v", err)
	}
	assertMode(t, p.Server, "plain.test", policy.ModeForwardBypass)
}

// TestNoCredentialRuleMeansNoInterception: the CA is present and injection is configured
// for some other host, and this destination is still tunnelled.
func TestNoCredentialRuleMeansNoInterception(t *testing.T) {
	webCA := newAuthority(t)
	origin := newOrigin(t, webCA, "unlisted.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"tok": "value"}, "tok@other.test=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow unlisted.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	// Trusting only the web CA: if the proxy substituted a certificate, this fails.
	resp, err := p.client(pool(webCA)).Get(hostPort(t, origin.URL, "unlisted.test"))
	if err != nil {
		t.Fatalf("a host with no credential rule was not tunnelled: %v", err)
	}
	resp.Body.Close()
	assertMode(t, p.Server, "unlisted.test", policy.ModeForwardBypass)
}

// TestCredentialRuleWithoutACAIsRecorded: a rule that cannot fire must not fail silently.
func TestCredentialRuleWithoutACAIsRecorded(t *testing.T) {
	webCA := newAuthority(t)
	var gotKey string
	origin := newOrigin(t, webCA, "api.creds.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
	}))

	inj := mustInjector(t, secret.MapProvider{"tok": "value"}, "tok@api.creds.test=x-api-key")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj) // no CA

	resp, err := p.client(pool(webCA)).Get(hostPort(t, origin.URL, "api.creds.test"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if gotKey != "" {
		t.Errorf("a credential was injected into HTTPS without a CA: %q", gotKey)
	}
	assertMode(t, p.Server, "api.creds.test", policy.ModeForwardBypass)
	if !hasReason(p.Server, "no certificate authority is configured") {
		t.Error("the log does not say why the credential rule did not fire")
	}
}

// TestInspectedFlowVerifiesTheOrigin: once Boks terminates TLS it owns verification, and a
// failure there must stop the request and say so.
func TestInspectedFlowVerifiesTheOrigin(t *testing.T) {
	webCA := newAuthority(t)
	reached := false
	origin := newOrigin(t, webCA, "api.creds.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"tok": "value"}, "tok@api.creds.test=bearer")
	// The proxy's trust store does not contain the origin's authority.
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(newAuthority(t))
	})

	resp, err := p.client(pool(boksCA)).Get(hostPort(t, origin.URL, "api.creds.test"))
	if err != nil {
		t.Fatalf("expected a readable refusal, got a transport error: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(body), "verif") {
		t.Errorf("the refusal does not explain itself:\n%s", body)
	}
	if reached {
		t.Error("the request reached an origin whose certificate did not verify")
	}
}

// TestInspectedFlowChecksTheRequestHost covers the one policy check only interception makes
// possible: a request inside the flow naming a different, forbidden host.
func TestInspectedFlowChecksTheRequestHost(t *testing.T) {
	webCA := newAuthority(t)
	origin := newOrigin(t, webCA, "api.creds.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request for a denied host reached the origin")
	}))

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"tok": "value"}, "tok@api.creds.test=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	conn := connectThrough(t, p, origin.URL, "api.creds.test")
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "api.creds.test", RootCAs: pool(boksCA)})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake with the proxy: %v", err)
	}
	fmt.Fprint(tlsConn, "GET / HTTP/1.1\r\nHost: forbidden.test\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %q", resp.StatusCode, body)
	}
	assertDecision(t, p.Server, policy.StageRequest, "forbidden.test", false)
}

// TestInspectedFlowLogsNoSecretsBodiesOrURLs is the assertion that matters most once the
// proxy can read traffic: it must retain none of it. Every canary here travels through an
// inspected flow in a different position — credential, request body, response body, URL
// query — and none may appear in any log or error the proxy produces.
func TestInspectedFlowLogsNoSecretsBodiesOrURLs(t *testing.T) {
	const (
		credential   = "sk-ant-CANARY-CREDENTIAL"
		requestBody  = "CANARY-REQUEST-BODY"
		responseBody = "CANARY-RESPONSE-BODY"
		queryCanary  = "CANARY-IN-URL"
	)

	webCA := newAuthority(t)
	origin := newOrigin(t, webCA, "api.creds.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, responseBody)
	}))

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"anthropic": credential}, "anthropic@api.creds.test=x-api-key")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	client := p.client(pool(boksCA))
	url := hostPort(t, origin.URL, "api.creds.test") + "/v1/messages?token=" + queryCanary
	resp, err := client.Post(url, "text/plain", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != responseBody {
		t.Fatalf("body = %q", body)
	}

	// Now the malformed-input path, which is where a careless proxy echoes traffic into
	// its own error messages: net/http's parse errors quote the bytes they choked on.
	conn := connectThrough(t, p, origin.URL, "api.creds.test")
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "api.creds.test", RootCAs: pool(boksCA)})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	// net/http reports this as: malformed HTTP request "BAD REQUEST LINE WITH ?token=…".
	// If the proxy passed that error to its logger, the canary would be in the log.
	fmt.Fprintf(tlsConn, "BAD REQUEST LINE WITH ?token=%s\r\n\r\n", queryCanary)
	io.Copy(io.Discard, tlsConn)
	tlsConn.Close()
	if !strings.Contains(p.logBuf.String(), "not an HTTP/1.1 request") {
		t.Errorf("the proxy did not record the malformed request at all:\n%s", p.logBuf.String())
	}

	logged := p.logBuf.String()
	var decisions strings.Builder
	for _, d := range p.Engine().Log().Recent(0) {
		fmt.Fprintf(&decisions, "%+v\n%s\n", d, d.String())
	}
	for _, canary := range []string{credential, requestBody, responseBody, queryCanary} {
		if strings.Contains(logged, canary) {
			t.Errorf("the proxy's operational log contains %q:\n%s", canary, logged)
		}
		if strings.Contains(decisions.String(), canary) {
			t.Errorf("the decision log contains %q:\n%s", canary, decisions.String())
		}
	}
	// It must still record *that* an injection happened, by name.
	if !strings.Contains(logged, "anthropic") {
		t.Errorf("the proxy did not record that a credential was injected:\n%s", logged)
	}
}

// TestInspectedFlowStreamsLargeBodies checks that nothing on the inspected path buffers a
// whole body: a body larger than any reasonable buffer must arrive intact in both
// directions.
func TestInspectedFlowStreamsLargeBodies(t *testing.T) {
	const size = 4 << 20 // 4 MiB
	payload := strings.Repeat("boks", size/4)

	webCA := newAuthority(t)
	var received int
	origin := newOrigin(t, webCA, "api.creds.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received = int(n)
		io.Copy(w, strings.NewReader(payload))
	}))

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"tok": "value"}, "tok@api.creds.test=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	resp, err := p.client(pool(boksCA)).Post(hostPort(t, origin.URL, "api.creds.test"), "application/octet-stream", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	if received != len(payload) {
		t.Errorf("origin received %d bytes, sent %d", received, len(payload))
	}
	if int(n) != len(payload) {
		t.Errorf("client received %d bytes, origin sent %d", n, len(payload))
	}
}

// TestInspectedFlowKeepsAlive drives several requests down one inspected connection, which
// is the path most likely to break in a hand-written HTTP hop.
func TestInspectedFlowKeepsAlive(t *testing.T) {
	webCA := newAuthority(t)
	var count int
	origin := newOrigin(t, webCA, "api.creds.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		fmt.Fprintf(w, "request %d", count)
	}))

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"tok": "value"}, "tok@api.creds.test=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})

	client := p.client(pool(boksCA))
	for i := 1; i <= 3; i++ {
		resp, err := client.Get(hostPort(t, origin.URL, "api.creds.test"))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := fmt.Sprintf("request %d", i); string(body) != got {
			t.Errorf("body = %q, want %q", body, got)
		}
	}
}

// TestNonTLSOnACredentialHostIsNotIntercepted: a tunnel that is not TLS cannot be
// terminated, and the log must not claim it was.
func TestNonTLSOnACredentialHostIsNotIntercepted(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	boksCA := newAuthority(t)
	inj := mustInjector(t, secret.MapProvider{"tok": "value"}, "tok@api.creds.test=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj, func(c *Config) {
		c.CA = boksCA
	})

	conn := connectThrough(t, p, "http://"+echo.Addr().String(), "api.creds.test")
	defer conn.Close()
	fmt.Fprint(conn, "PING\n")
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("the tunnel did not carry plain bytes: %v", err)
	}
	if string(buf) != "PING\n" {
		t.Errorf("echo returned %q", buf)
	}
	if !hasReason(p.Server, "not TLS for this name") {
		t.Error("the log does not record that an inspected flow was carried blind instead")
	}
}

// connectThrough opens a CONNECT tunnel through the proxy to the port of rawURL, naming
// host, and returns the tunnel ready for TLS.
func connectThrough(t *testing.T, p *testProxy, rawURL, host string) net.Conn {
	t.Helper()
	target := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	_, port, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("split %q: %v", target, err)
	}
	conn, err := net.DialTimeout("tcp", p.url.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\n\r\n", host, port, host, port)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the CONNECT answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT returned %s", resp.Status)
	}
	return conn
}

func assertMode(t *testing.T, s *Server, host string, want policy.Mode) {
	t.Helper()
	var seen []string
	for _, d := range s.Engine().Log().Recent(0) {
		if d.Host != host || d.Stage != policy.StageConnect {
			continue
		}
		seen = append(seen, string(d.Mode))
		// The last connect-stage record for a host is its final disposition.
	}
	if len(seen) == 0 {
		t.Fatalf("no connect decision logged for %s", host)
	}
	if got := seen[len(seen)-1]; got != string(want) {
		t.Errorf("%s was recorded as %q, want %q (all: %v)", host, got, want, seen)
	}
}

func hasReason(s *Server, substr string) bool {
	for _, d := range s.Engine().Log().Recent(0) {
		if strings.Contains(d.Reason, substr) {
			return true
		}
	}
	return false
}
