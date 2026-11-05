package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/policy"
)

// httpRulePolicy builds a policy from connection rules (mustPolicy's syntax) plus HTTP rules.
func httpRulePolicy(t *testing.T, def policy.Action, plain []string, httpRules ...policy.Rule) policy.Policy {
	t.Helper()
	p := mustPolicy(t, def, plain...)
	p.Rules = append(p.Rules, httpRules...)
	return p
}

func httpRule(t *testing.T, action policy.Action, spec string, methods []string, path string, query ...string) policy.Rule {
	t.Helper()
	r, err := policy.ParseHTTPRule(action, spec, methods, path, query)
	if err != nil {
		t.Fatalf("ParseHTTPRule: %v", err)
	}
	return r
}

// countingHandler answers 200 with the request line and counts what reached the origin, so a
// test can assert that a refused request never did.
type countingHandler struct{ hits atomic.Int32 }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.hits.Add(1)
	fmt.Fprintf(w, "origin: %s %s", r.Method, r.URL.Path)
}

func doReq(t *testing.T, c *http.Client, method, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestHTTPRulesOnPlainHTTP: plaintext HTTP is read anyway, so HTTP rules apply to it directly.
func TestHTTPRulesOnPlainHTTP(t *testing.T) {
	origin := &countingHandler{}
	srv := httptest.NewServer(origin)
	defer srv.Close()

	p := newTestProxy(t, httpRulePolicy(t, policy.Deny, nil,
		httpRule(t, policy.Allow, "api.rules.test", []string{"GET", "HEAD"}, "/v1/**", "team=platform"),
	), nil)
	c := p.client(nil)
	base := hostPort(t, srv.URL, "api.rules.test")

	if code, body := doReq(t, c, "GET", base+"/v1/items?team=platform"); code != 200 {
		t.Fatalf("an allowed request was refused: %d %s", code, body)
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/items?team=platform"},
		{"GET", "/v2/items?team=platform"},
		{"GET", "/v1/items?team=other"},
		{"GET", "/v1/items"},
	} {
		code, body := doReq(t, c, tc.method, base+tc.path)
		if code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, code)
		}
		if tc.method == "POST" && !strings.Contains(body, "--method POST --path '/v1/items'") {
			t.Errorf("the refusal does not suggest a rule for the request:\n%s", body)
		}
	}
	if n := origin.hits.Load(); n != 1 {
		t.Errorf("origin saw %d requests, want only the allowed one", n)
	}

	// The log says which request was judged, by method and canonical path, and never the query.
	var sawRequest bool
	for _, d := range p.Engine().Log().Recent(0) {
		if d.Method == "POST" && d.Path == "/v1/items" && !d.Allowed {
			sawRequest = true
		}
		if strings.Contains(d.Path, "team") || strings.Contains(d.Reason, "team=other") {
			t.Errorf("the request's query reached the log: %+v", d)
		}
	}
	if !sawRequest {
		t.Error("the refused POST is not in the decision log with its method and path")
	}
}

// TestHTTPRulesOnInspectedHTTPS: a host with HTTP rules is terminated when a CA exists, and each
// request is judged; a host without them is still tunnelled blind.
func TestHTTPRulesOnInspectedHTTPS(t *testing.T) {
	webCA := newAuthority(t)
	origin := &countingHandler{}
	srv := newOrigin(t, webCA, "api.rules.test", origin)

	boksCA := newAuthority(t)
	p := newTestProxy(t, httpRulePolicy(t, policy.Deny, nil,
		httpRule(t, policy.Allow, "api.rules.test", []string{"GET"}, "/v1/**"),
	), nil, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})
	c := p.client(pool(boksCA))
	base := hostPort(t, srv.URL, "api.rules.test")

	code, body := doReq(t, c, "GET", base+"/v1/items")
	if code != 200 || !strings.Contains(body, "origin: GET /v1/items") {
		t.Fatalf("the allowed request did not reach the origin: %d %s", code, body)
	}
	if code, _ := doReq(t, c, "DELETE", base+"/v1/items"); code != http.StatusForbidden {
		t.Errorf("DELETE through a GET-only rule = %d, want 403", code)
	}
	if code, _ := doReq(t, c, "GET", base+"/admin"); code != http.StatusForbidden {
		t.Errorf("GET outside the path = %d, want 403", code)
	}
	if n := origin.hits.Load(); n != 1 {
		t.Errorf("origin saw %d requests, want 1", n)
	}
	assertMode(t, p.Server, "api.rules.test", policy.ModeForward)
	assertDecision(t, p.Server, policy.StageRequest, "api.rules.test", false)
}

// TestHTTPOnlyAllowWithoutACAIsRefused: with no CA the flow cannot be read, so an HTTP allow
// must not turn into a blind tunnel to the host.
func TestHTTPOnlyAllowWithoutACAIsRefused(t *testing.T) {
	webCA := newAuthority(t)
	origin := &countingHandler{}
	srv := newOrigin(t, webCA, "api.rules.test", origin)

	p := newTestProxy(t, httpRulePolicy(t, policy.Deny, nil,
		httpRule(t, policy.Allow, "api.rules.test", []string{"ANY"}, ""),
	), nil)
	_, err := p.client(pool(webCA)).Get(hostPort(t, srv.URL, "api.rules.test") + "/")
	if err == nil {
		t.Fatal("an HTTP-only allow was carried as a blind tunnel")
	}
	if origin.hits.Load() != 0 {
		t.Error("the origin was reached")
	}
	if !hasReason(p.Server, "cannot read as HTTP") {
		t.Error("the log does not say why the connection was refused")
	}
}

// TestHTTPDenyOnAnAllowedHost: deny wins, request by request, and an attempt to reach the
// denied path by another spelling is seen through.
func TestHTTPDenyOnAnAllowedHost(t *testing.T) {
	webCA := newAuthority(t)
	origin := &countingHandler{}
	srv := newOrigin(t, webCA, "api.rules.test", origin)

	boksCA := newAuthority(t)
	p := newTestProxy(t, httpRulePolicy(t, policy.Deny, []string{"allow api.rules.test"},
		httpRule(t, policy.Deny, "api.rules.test", []string{"POST", "DELETE"}, "/admin/**"),
	), nil, func(c *Config) {
		c.CA = boksCA
		c.UpstreamRootCAs = pool(webCA)
	})
	c := p.client(pool(boksCA))
	base := hostPort(t, srv.URL, "api.rules.test")

	if code, _ := doReq(t, c, "POST", base+"/admin/users"); code != http.StatusForbidden {
		t.Errorf("POST /admin/users = %d, want 403", code)
	}
	if code, _ := doReq(t, c, "GET", base+"/admin/users"); code != 200 {
		t.Errorf("GET /admin/users = %d, want 200 (the deny names POST and DELETE)", code)
	}
	if code, _ := doReq(t, c, "POST", base+"/v1/users"); code != 200 {
		t.Errorf("POST /v1/users = %d, want 200", code)
	}

	// Raw request lines, so that no client library cleans the path first.
	for _, path := range []string{"/v1/../admin/users", "/v1/%2e%2e/admin/users", "/ADMIN/users", "//admin/users"} {
		conn := connectThrough(t, p, srv.URL, "api.rules.test")
		tlsConn := tls.Client(conn, &tls.Config{ServerName: "api.rules.test", RootCAs: pool(boksCA)})
		if err := tlsConn.Handshake(); err != nil {
			t.Fatalf("handshake: %v", err)
		}
		fmt.Fprintf(tlsConn, "POST %s HTTP/1.1\r\nHost: api.rules.test\r\nContent-Length: 0\r\n\r\n", path)
		resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
		if err != nil {
			t.Fatalf("reading the response to %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403", path, resp.StatusCode)
		}
		tlsConn.Close()
	}
	if n := origin.hits.Load(); n != 2 {
		t.Errorf("origin saw %d requests, want the two allowed ones", n)
	}
}

// TestHTTPDenyWithoutACAClosesTheHost: an allowed host with an HTTP deny cannot be tunnelled
// blind, because the deny could not be enforced inside the tunnel.
func TestHTTPDenyWithoutACAClosesTheHost(t *testing.T) {
	webCA := newAuthority(t)
	srv := newOrigin(t, webCA, "api.rules.test", &countingHandler{})
	p := newTestProxy(t, httpRulePolicy(t, policy.Deny, []string{"allow api.rules.test"},
		httpRule(t, policy.Deny, "api.rules.test", []string{"POST"}, ""),
	), nil)
	if _, err := p.client(pool(webCA)).Get(hostPort(t, srv.URL, "api.rules.test")); err == nil {
		t.Fatal("a host with an unenforceable HTTP deny was tunnelled blind")
	}
	if !hasReason(p.Server, "HTTP deny rule") {
		t.Error("the refusal does not name the HTTP deny rule")
	}
}

// TestDeferredFlowThatIsNotTLSIsClosed: a CONNECT let through to be read must not fall back to a
// blind splice when what arrives is not TLS for the host.
func TestDeferredFlowThatIsNotTLSIsClosed(t *testing.T) {
	origin := &countingHandler{}
	srv := httptest.NewServer(origin) // plaintext, reached through CONNECT
	defer srv.Close()

	p := newTestProxy(t, httpRulePolicy(t, policy.Deny, nil,
		httpRule(t, policy.Allow, "api.rules.test", []string{"GET"}, ""),
	), nil, func(c *Config) { c.CA = newAuthority(t) })

	conn := connectThrough(t, p, srv.URL, "api.rules.test")
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: api.rules.test\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, _ := io.ReadAll(conn)
	if strings.Contains(string(data), "origin:") {
		t.Fatalf("a plaintext request inside a deferred tunnel reached the origin: %q", data)
	}
	if origin.hits.Load() != 0 {
		t.Error("the origin was reached")
	}
	if !hasReason(p.Server, "closed rather than carried blind") {
		t.Error("the log does not record why the tunnel was closed")
	}
}
