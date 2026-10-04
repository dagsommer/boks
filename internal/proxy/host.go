package proxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// HostName is the name a sandbox reaches a service on the host's loopback by, through this
// proxy and nowhere else.
//
// # Why only through the proxy
//
// Every preset denies the host's loopback, and a deny cannot be overridden by an allow —
// rightly: under `--policy open` an allow-everything rule would otherwise hand the guest every
// port on the host's localhost. Host access is therefore its own opt-in, port by port
// (`--allow-host-port`), and it exists in exactly one place: this proxy, which reads the
// request as HTTP, records it, and can attach a credential to it. The sandbox's resolver does
// not know the name and there is no address that reaches the host, so a client that ignores
// HTTP_PROXY fails to resolve it rather than finding a raw path around the proxy.
//
// A CONNECT to it is accepted but not spliced: Node's proxy support (undici) tunnels even a
// plain http:// request, so refusing CONNECT locked out every Node client — `pi` among them.
// What arrives inside such a tunnel is read as HTTP, request by request, exactly as a proxied
// request is; a client that starts TLS inside it is closed. See handleHostTunnel.
//
// Built 2026-10-04 for a model served on the host by llama-server, which the agent in the
// sandbox talks to over plain HTTP.
const HostName = "host.boks.internal"

// isHost reports whether t addresses the host through HostName.
func isHost(t policy.Target) bool {
	return !t.IsIP() && strings.EqualFold(strings.TrimSuffix(t.Host, "."), HostName)
}

// hostPortOpen reports whether port was opened on the host for this sandbox.
func (s *Server) hostPortOpen(port int) bool {
	for _, p := range s.cfg.HostPorts {
		if p == port {
			return true
		}
	}
	return false
}

// hostDecision decides a plain-HTTP request to HostName and records it. It replaces the
// policy check for that one name: no preset, profile or allow rule can open the host, and
// none is consulted.
func (s *Server) hostDecision(stage policy.Stage, t policy.Target) policy.Decision {
	if !s.hostPortOpen(t.Port) {
		return s.cfg.Engine.NoteRefused(stage, t, policy.ModeForward, fmt.Sprintf(
			"port %d on the host is not open to this sandbox; start it with --allow-host-port %d", t.Port, t.Port))
	}
	return s.cfg.Engine.Note(stage, t, policy.ModeForward, fmt.Sprintf(
		"host port %d, opened with --allow-host-port; forwarded to 127.0.0.1:%d", t.Port, t.Port))
}

// dialHost connects to an opened port on the host's loopback. It re-checks the port, so no
// path can reach it that the request-level decision did not allow.
func (s *Server) dialHost(ctx context.Context, t policy.Target) (net.Conn, error) {
	if !s.hostPortOpen(t.Port) {
		return nil, fmt.Errorf("port %d on the host is not open to this sandbox", t.Port)
	}
	dialCtx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()
	return s.cfg.DialAddr(dialCtx, netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(t.Port)))
}

// handleHostTunnel serves a CONNECT to HostName by reading the tunnel as plain HTTP rather than
// splicing it, so every request in it is judged, recorded and given its credential like any
// other proxied request. TLS inside the tunnel is refused: it would be unreadable, which is
// the one thing a path to the host must not be.
func (s *Server) handleHostTunnel(w http.ResponseWriter, r *http.Request, target policy.Target) {
	if d := s.hostDecision(policy.StageConnect, target); !d.Allowed {
		writeDenied(w, d)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "boks: cannot establish a tunnel on this connection\n", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		s.logf("hijacking connection for %s: %v", target, err)
		return
	}
	defer client.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\nBoks-Policy: allow\r\n\r\n")); err != nil {
		return
	}

	reader := buffered.Reader
	if first, err := reader.Peek(1); err != nil || (len(first) == 1 && first[0] == 0x16) {
		if err == nil {
			s.cfg.Engine.NoteRefused(policy.StageConnect, target, policy.ModeForwardBypass,
				"TLS inside a tunnel to the host; the host is reachable as plain HTTP only, which the proxy can read")
		}
		return
	}
	s.serveHostRequests(r.Context(), target, client, reader)
}

// serveHostRequests proxies plain-HTTP requests read from an established tunnel to the host.
func (s *Server) serveHostRequests(ctx context.Context, target policy.Target, client net.Conn, reader *bufio.Reader) {
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if d := s.hostDecision(policy.StageHTTP, target); !d.Allowed {
			req.Body.Close()
			writeStatus(client, http.StatusForbidden, denialText(d))
			return
		}
		req = req.WithContext(ctx)
		req.URL.Scheme = "http"
		req.URL.Host = target.String()
		req.RequestURI = ""
		stripHopByHop(req.Header)
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		if _, err := s.cfg.Injector.Apply(ctx, target, req.Header, secret.FlowPlaintext); err != nil {
			req.Body.Close()
			writeStatus(client, http.StatusBadGateway, "boks: credential injection failed: "+err.Error()+"\n")
			return
		}
		resp, err := s.transport.RoundTrip(req)
		if err != nil {
			s.logf("host %s: %v", target, err)
			writeStatus(client, http.StatusBadGateway, "boks: the host service did not answer: "+err.Error()+"\n")
			return
		}
		stripHopByHop(resp.Header)
		resp.Header.Set("Boks-Policy", "allow")
		writeErr := resp.Write(client)
		resp.Body.Close()
		if writeErr != nil || req.Close || resp.Close {
			return
		}
	}
}
