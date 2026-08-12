package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// testProxy starts a proxy whose resolver maps every name to the loopback address the test
// servers listen on. Names therefore behave exactly as they would in production — the
// policy sees the name the client asked for — without touching DNS or /etc/hosts.
type testProxy struct {
	*Server
	url    *url.URL
	logBuf *bytes.Buffer
}

func newTestProxy(t *testing.T, p policy.Policy, inj *secret.Injector, opts ...func(*Config)) *testProxy {
	t.Helper()

	logBuf := &bytes.Buffer{}
	cfg := Config{
		Engine:   policy.NewEngine(p, policy.NewLog(64)),
		Injector: inj,
		ErrorLog: log.New(logBuf, "proxy: ", 0),
		Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		ClientHelloTimeout: 2 * time.Second,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	u, err := url.Parse("http://" + l.Addr().String())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &testProxy{Server: srv, url: u, logBuf: logBuf}
}

// client returns an HTTP client that sends everything through the proxy. rootCA is what the
// client trusts: pass the origin's own authority to assert that a flow was tunnelled
// untouched, or the Boks CA to accept an intercepted one.
func (p *testProxy) client(rootCA *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(p.url),
			TLSClientConfig: &tls.Config{RootCAs: rootCA},
		},
	}
}

func mustPolicy(t *testing.T, def policy.Action, rules ...string) policy.Policy {
	t.Helper()
	p := policy.Policy{Name: "test", Default: def}
	for _, spec := range rules {
		action := policy.Allow
		rest := spec
		if a, r, ok := strings.Cut(spec, " "); ok {
			parsed, err := policy.ParseAction(a)
			if err != nil {
				t.Fatalf("rule %q: %v", spec, err)
			}
			action, rest = parsed, r
		}
		rule, err := policy.ParseRule(action, rest)
		if err != nil {
			t.Fatalf("rule %q: %v", spec, err)
		}
		p.Rules = append(p.Rules, rule)
	}
	return p
}

// hostPort rewrites a test server's URL so the request names a host rather than an
// address, while still reaching the server through the proxy's stub resolver.
func hostPort(t *testing.T, rawURL, host string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %q: %v", u.Host, err)
	}
	u.Host = net.JoinHostPort(host, port)
	return u.String()
}

func TestHTTPAllowed(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from the origin")
	}))
	defer origin.Close()

	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow allowed.test"), nil)
	resp, err := p.client(nil).Get(hostPort(t, origin.URL, "allowed.test"))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
	if string(body) != "hello from the origin" {
		t.Errorf("body = %q", body)
	}
	if got := resp.Header.Get("Boks-Policy"); got != "allow" {
		t.Errorf("Boks-Policy = %q, want allow", got)
	}
	assertDecision(t, p.Server, policy.StageHTTP, "allowed.test", true)
}

func TestHTTPDenied(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("origin was reached despite a deny")
	}))
	defer origin.Close()

	p := newTestProxy(t, mustPolicy(t, policy.Deny), nil)
	resp, err := p.client(nil).Get(hostPort(t, origin.URL, "denied.test"))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %q", resp.StatusCode, body)
	}
	// A denial must be self-explanatory: what, why, and what to do.
	for _, want := range []string{"blocked by network policy", "denied.test", "denied by default", "boks run --allow"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("denial body does not mention %q:\n%s", want, body)
		}
	}
	if got := resp.Header.Get("Boks-Policy"); got != "deny" {
		t.Errorf("Boks-Policy = %q, want deny", got)
	}
	assertDecision(t, p.Server, policy.StageHTTP, "denied.test", false)
}

func TestHTTPPortScoping(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()

	// Allow the host on 443 only; the origin is plain HTTP on some other port.
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow allowed.test:443"), nil)
	resp, err := p.client(nil).Get(hostPort(t, origin.URL, "allowed.test"))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: a rule scoped to :443 must not permit another port", resp.StatusCode)
	}
}

// newTLSOrigin starts an HTTPS server whose certificate is valid for the given names.
func newTLSOrigin(t *testing.T, handler http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = false
	srv.StartTLS()
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func TestConnectAllowedPreservesEndToEndTLS(t *testing.T) {
	var sawSNI string
	origin, pool := newTLSOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			sawSNI = r.TLS.ServerName
		}
		fmt.Fprint(w, "secret-free payload")
	}))

	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow example.com"), nil)

	// httptest's certificate is issued for "example.com" and 127.0.0.1, so the client
	// must ask for a name the certificate covers for verification to succeed. That is
	// the point of the test: the client validates the *origin's* chain, which only works
	// because the proxy did not substitute one.
	client := p.client(pool)
	resp, err := client.Get(hostPort(t, origin.URL, "example.com"))
	if err != nil {
		t.Fatalf("HTTPS through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "secret-free payload" {
		t.Errorf("body = %q", body)
	}
	if sawSNI != "example.com" {
		t.Errorf("origin saw SNI %q, want example.com", sawSNI)
	}

	// Both stages should be in the log, proving the SNI was actually parsed out of a
	// real handshake rather than assumed.
	assertDecision(t, p.Server, policy.StageConnect, "example.com", true)
}

func TestConnectDenied(t *testing.T) {
	origin, pool := newTLSOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("origin was reached despite a deny")
	}))

	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow other.test"), nil)
	_, err := p.client(pool).Get(hostPort(t, origin.URL, "example.com"))
	if err == nil {
		t.Fatal("denied HTTPS request succeeded")
	}
	// Go surfaces the proxy's 403 as a transport error naming the status, so the failure
	// is visible rather than a hang.
	if !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error = %v, want it to report the proxy's 403", err)
	}
	assertDecision(t, p.Server, policy.StageConnect, "example.com", false)
}

// TestConnectDeniedIsAnImmediateAnswer checks the CONNECT refusal at the wire level: a
// blocked tunnel must produce a readable HTTP response, not a silent hang.
func TestConnectDeniedIsAnImmediateAnswer(t *testing.T) {
	p := newTestProxy(t, mustPolicy(t, policy.Deny), nil)

	conn, err := net.DialTimeout("tcp", p.url.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprint(conn, "CONNECT blocked.test:443 HTTP/1.1\r\nHost: blocked.test:443\r\n\r\n")
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading the proxy's answer: %v", err)
	}
	got := string(buf[:n])
	if !strings.HasPrefix(got, "HTTP/1.1 403") {
		t.Errorf("answer = %q, want a 403", got)
	}
	if !strings.Contains(got, "blocked by network policy") {
		t.Errorf("answer does not explain itself:\n%s", got)
	}
}

// TestConnectSNIMismatchIsRejected covers a client that asks to tunnel to a permitted host
// and then greets a forbidden one.
func TestConnectSNIMismatchIsRejected(t *testing.T) {
	origin, pool := newTLSOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("origin served a request whose SNI should have been refused")
	}))

	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow allowed.test"), nil)

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "https://"))
	conn, err := net.DialTimeout("tcp", p.url.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	fmt.Fprintf(conn, "CONNECT allowed.test:%s HTTP/1.1\r\nHost: allowed.test:%s\r\n\r\n", port, port)
	br := make([]byte, 1024)
	n, err := conn.Read(br)
	if err != nil {
		t.Fatalf("reading CONNECT answer: %v", err)
	}
	if !strings.HasPrefix(string(br[:n]), "HTTP/1.1 200") {
		t.Fatalf("CONNECT to a permitted host was refused: %q", br[:n])
	}

	// Now speak TLS naming a different, forbidden host.
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "sneaky.test", RootCAs: pool})
	if err := tlsConn.Handshake(); err == nil {
		t.Error("handshake with a forbidden server name succeeded")
	}
	assertDecision(t, p.Server, policy.StageSNI, "sneaky.test", false)
}

// TestResolvedAddressIsRechecked is the DNS-rebinding case: a permitted name whose address
// is denied must not be dialled.
func TestResolvedAddressIsRechecked(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a denied address was reached")
	}))
	defer origin.Close()

	p := mustPolicy(t, policy.Deny, "allow rebind.test", "deny 127.0.0.0/8")
	tp := newTestProxy(t, p, nil)

	resp, err := tp.client(nil).Get(hostPort(t, origin.URL, "rebind.test"))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("request to a name resolving into a denied prefix succeeded: %q", body)
	}
	assertDecision(t, tp.Server, policy.StageDial, "127.0.0.1", false)
}

func TestCredentialInjectionOnlyForConfiguredHosts(t *testing.T) {
	type seen struct {
		auth   string
		apiKey string
	}
	got := map[string]seen{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		got[host] = seen{auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("X-Api-Key")}
	}))
	defer origin.Close()

	inj := mustInjector(t, secret.MapProvider{"apikey": "sk-live-DO-NOT-LOG"}, "apikey@api.creds.test=x-api-key")

	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test", "allow other.test"), inj)
	client := p.client(nil)

	for _, host := range []string{"api.creds.test", "other.test"} {
		resp, err := client.Get(hostPort(t, origin.URL, host))
		if err != nil {
			t.Fatalf("GET %s: %v", host, err)
		}
		resp.Body.Close()
	}

	if got["api.creds.test"].apiKey != "sk-live-DO-NOT-LOG" {
		t.Errorf("configured host received %q, want the injected key", got["api.creds.test"].apiKey)
	}
	if got["other.test"].apiKey != "" || got["other.test"].auth != "" {
		t.Errorf("credential leaked to an unconfigured host: %+v", got["other.test"])
	}
}

func TestPlaceholderIsReplacedNotAppended(t *testing.T) {
	var auth []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Values("Authorization")
	}))
	defer origin.Close()

	inj := mustInjector(t, secret.MapProvider{"tok": "real-token"}, "tok@api.creds.test=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj)

	req, err := http.NewRequest("GET", hostPort(t, origin.URL, "api.creds.test"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// The guest holds a placeholder. It must not reach the origin alongside the real one.
	req.Header.Set("Authorization", "Bearer BOKS_PLACEHOLDER")
	resp, err := p.client(nil).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if len(auth) != 1 || auth[0] != "Bearer real-token" {
		t.Errorf("Authorization = %q, want exactly the injected value", auth)
	}
}

// TestProxyAuthorizationIsNotForwarded: the guest's own proxy credentials stop here.
func TestProxyAuthorizationIsNotForwarded(t *testing.T) {
	var forwarded string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Get("Proxy-Authorization")
	}))
	defer origin.Close()

	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow allowed.test"), nil)
	req, _ := http.NewRequest("GET", hostPort(t, origin.URL, "allowed.test"), nil)
	req.Header.Set("Proxy-Authorization", "Basic Zm9vOmJhcg==")
	resp, err := p.client(nil).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if forwarded != "" {
		t.Errorf("Proxy-Authorization was forwarded upstream: %q", forwarded)
	}
}

// TestSecretsNeverAppearInLogs drives a request that injects a credential and then asserts
// that neither the proxy's operational log nor the decision log contains the value.
func TestSecretsNeverAppearInLogs(t *testing.T) {
	const value = "sk-ant-super-secret-value"

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()

	inj := mustInjector(t, secret.MapProvider{"anthropic": value}, "anthropic@api.creds.test=x-api-key")
	p := newTestProxy(t, mustPolicy(t, policy.Deny, "allow api.creds.test"), inj)

	resp, err := p.client(nil).Get(hostPort(t, origin.URL, "api.creds.test"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if strings.Contains(p.logBuf.String(), value) {
		t.Errorf("the proxy's error log contains the secret:\n%s", p.logBuf.String())
	}
	if !strings.Contains(p.logBuf.String(), "anthropic") {
		t.Errorf("the proxy did not record that an injection happened:\n%s", p.logBuf.String())
	}
	var decisions bytes.Buffer
	for _, d := range p.Engine().Log().Recent(0) {
		fmt.Fprintf(&decisions, "%+v\n%s\n", d, d.String())
	}
	if strings.Contains(decisions.String(), value) {
		t.Errorf("the decision log contains the secret:\n%s", decisions.String())
	}
}

func TestNonProxyRequestIsExplained(t *testing.T) {
	p := newTestProxy(t, mustPolicy(t, policy.Allow), nil)
	resp, err := http.Get(p.url.String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "forward proxy") {
		t.Errorf("body = %q", body)
	}
}

func TestNewRequiresAnEngine(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a proxy without a policy engine was accepted")
	}
}

func assertDecision(t *testing.T, s *Server, stage policy.Stage, host string, allowed bool) {
	t.Helper()
	for _, d := range s.Engine().Log().Recent(0) {
		if d.Stage == stage && d.Host == host && d.Allowed == allowed {
			if d.Reason == "" {
				t.Errorf("decision %+v has no reason", d)
			}
			return
		}
	}
	var b strings.Builder
	for _, d := range s.Engine().Log().Recent(0) {
		fmt.Fprintf(&b, "  %s\n", d)
	}
	t.Errorf("no %s decision for %s (allowed=%v); log was:\n%s", stage, host, allowed, b.String())
}
