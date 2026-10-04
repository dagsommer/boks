package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// hostService stands in for something on the host's loopback — llama-server, in the case
// this was built for. It reports the Authorization it received, so injection is visible.
func hostService(t *testing.T) (port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host saw %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ = strconv.Atoi(u.Port())
	return port
}

func hostURL(port int, path string) string {
	return fmt.Sprintf("http://%s:%d%s", HostName, port, path)
}

// An opened port is reached as http://host.boks.internal:PORT through the proxy, and the
// request carries the credential a rule names for it.
func TestHostPortReachedThroughTheProxyWithItsCredential(t *testing.T) {
	port := hostService(t)
	inj := mustInjector(t, secret.MapProvider{"llama": "local-key"}, "llama@"+HostName+"=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny), inj, func(c *Config) { c.HostPorts = []int{port} })

	resp, err := p.client(nil).Get(hostURL(port, "/v1/models"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `auth="Bearer local-key"`) {
		t.Fatalf("status %d, body %q; want the host service, with the injected key", resp.StatusCode, body)
	}
}

// The host is never reachable by policy alone: an allow-everything policy — which is what
// `--policy open` is — opens no port on the host, because the host is not a policy question.
func TestHostPortNotOpenedIsRefusedEvenUnderAnAllowEverythingPolicy(t *testing.T) {
	port := hostService(t)
	p := newTestProxy(t, mustPolicy(t, policy.Allow, "allow *"), nil)

	resp, err := p.client(nil).Get(hostURL(port, "/"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, body %q; a port that was not opened must be refused", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "--allow-host-port "+strconv.Itoa(port)) {
		t.Errorf("the refusal does not say how to open the port:\n%s", body)
	}
}

// connectTo opens a CONNECT tunnel through the proxy and returns the connection after the
// proxy's answer, with the status line it gave.
func connectTo(t *testing.T, p *testProxy, target string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", p.url.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the CONNECT answer: %v", err)
	}
	return conn, r, resp.Status
}

// Node's proxy support tunnels even plain http://, so a CONNECT to the host is accepted — and
// read as HTTP, request by request, with the credential attached, never spliced blind. Two
// requests on one tunnel prove it is read per request, not just the first.
func TestHostTunnelIsReadAsHTTP(t *testing.T) {
	port := hostService(t)
	inj := mustInjector(t, secret.MapProvider{"llama": "local-key"}, "llama@"+HostName+"=bearer")
	p := newTestProxy(t, mustPolicy(t, policy.Deny), inj, func(c *Config) { c.HostPorts = []int{port} })

	target := fmt.Sprintf("%s:%d", HostName, port)
	conn, r, status := connectTo(t, p, target)
	if !strings.HasPrefix(status, "200") {
		t.Fatalf("CONNECT answered %q, want 200", status)
	}
	for _, path := range []string{"/first", "/second"} {
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer placeholder\r\n\r\n", path, target)
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("%s: reading the answer: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if want := "host saw GET " + path + ` auth="Bearer local-key"`; string(body) != want {
			t.Errorf("%s: body %q, want %q", path, body, want)
		}
	}
}

// TLS inside a tunnel to the host would be unreadable, which is the one thing a path to the
// host must not be: the tunnel is closed.
func TestHostTunnelRefusesTLS(t *testing.T) {
	port := hostService(t)
	p := newTestProxy(t, mustPolicy(t, policy.Deny), nil, func(c *Config) { c.HostPorts = []int{port} })

	conn, r, status := connectTo(t, p, fmt.Sprintf("%s:%d", HostName, port))
	if !strings.HasPrefix(status, "200") {
		t.Fatalf("CONNECT answered %q", status)
	}
	// The first bytes of a TLS ClientHello: a handshake record.
	if _, err := conn.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(make([]byte, 64)); err == nil {
		t.Fatalf("the tunnel answered %d bytes to TLS; it must be closed", n)
	}
}

// A CONNECT to a port that was not opened is refused before any tunnel exists.
func TestHostTunnelToAClosedPortIsRefused(t *testing.T) {
	port := hostService(t)
	p := newTestProxy(t, mustPolicy(t, policy.Allow, "allow *"), nil)

	_, _, status := connectTo(t, p, fmt.Sprintf("%s:%d", HostName, port))
	if !strings.HasPrefix(status, "403") {
		t.Fatalf("CONNECT to an unopened host port answered %q, want 403", status)
	}
}
