// Package proxy is the host-side forward proxy that applies a network policy to a
// sandbox's HTTP and HTTPS traffic and attaches credentials the guest never sees.
//
// # This is a feature layered on the boundary, not the boundary
//
// A forward proxy only sees traffic a client chooses to send it, and **an environment
// variable is not a security boundary**. That has not changed and never will. What changed
// is what lies underneath: inside a sandbox this proxy listens in a virtual network whose
// host-side stack (internal/network) judges every TCP connection the guest opens, by address
// and port, before it dials anything. A guest that ignores HTTP_PROXY no longer walks past
// the policy — it is judged on addresses instead of names, and loses the things only a
// cooperating client can be given:
//
//   - hostname rules, and with them any rule about a destination whose address is not
//     known in advance;
//   - credential injection, which requires reading the request;
//   - a refusal that says why, in a body a human can read, instead of a refused socket.
//
// Run standalone under `boks proxy`, with no sandbox and no stack behind it, this package is
// exactly what it always was: a cooperating-client mechanism. The command says so.
//
// # Two kinds of flow, and the difference is user-visible
//
// Every connection through this proxy is one of two things, recorded on every entry in the
// decision log under the names Docker Sandboxes uses for the same distinction:
//
//   - **forward-bypass** — the default, and what happens to all but a handful of
//     destinations. The CONNECT is spliced byte-for-byte: the flow used the proxy but
//     bypassed inspection. TLS is end-to-end, the client validates the origin's own
//     certificate chain, and the proxy sees ciphertext. Filtering is on the two things
//     visible without decrypting: the CONNECT target and the ClientHello's server name.
//   - **forward** — the proxy handled the flow at the HTTP level and could read it. That
//     is either plaintext HTTP, where there was never anything to break, or HTTPS that
//     the proxy terminated and re-originated, presenting a leaf signed by the local Boks
//     CA (internal/ca) and verifying the origin's real certificate itself.
//
// (A third mode, **transparent**, belongs to flows judged at the network layer without using
// the proxy at all. Those decisions are taken and logged by internal/network, not here.)
//
// An HTTPS flow is terminated **only if the destination host has a credential rule
// configured for it** (internal/secret). There is no flag, preset or default that
// intercepts anything else, and adding one would be a mistake: interception is the price of
// credential injection over HTTPS, not a capability worth having on its own. Without a CA
// configured, nothing is ever terminated and HTTPS credential rules simply do not fire.
//
// What inspection costs, stated plainly here and in docs/security-model.md:
//
//   - For those hosts the guest no longer has end-to-end confidentiality with the origin.
//     Boks *can* read the traffic. It does not retain it: no body, header value or URL is
//     copied into a log, an error or a metric, and tests assert that.
//   - The guest validates a Boks certificate instead of the origin's, so certificate
//     pinning in the guest breaks — visibly, which is the correct failure.
//   - Boks becomes responsible for verifying the origin: it does full verification against
//     the host's trust store and refuses the flow if that fails.
//   - HTTP/2 is not carried inside an inspected flow. ALPN offers http/1.1 only, so
//     clients negotiate down rather than break; tunnelled flows are untouched and can use
//     whatever they like.
//
// What tunnelling costs, unchanged:
//
//   - SNI can be omitted, or can name a host the client never talks to. It is a
//     cross-check on the CONNECT target, not an independent guarantee. Encrypted Client
//     Hello removes it entirely.
//   - Hostname rules mean nothing for a raw socket. Only address and port rules apply
//     there — which is what the network stack applies, and why a policy written entirely
//     in hostnames denies raw flows rather than permitting them.
//   - DNS is a covert channel unless resolution is mediated too. Traffic through this
//     proxy carries names rather than resolving them in the guest, and inside a sandbox
//     the only resolver the guest can reach is the gateway's own: UDP to anything else is
//     dropped at the link. What that does not do is filter the names themselves, which is
//     still unbuilt.
package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// Config configures a Server. Only Engine is required.
type Config struct {
	// Engine decides destinations and records every decision.
	Engine *policy.Engine

	// Injector attaches credentials to permitted requests. Optional.
	//
	// It also decides, on its own, which HTTPS flows are inspected: a host with a
	// credential rule, and no other. See shouldInspect.
	Injector *secret.Injector

	// CA signs the certificates presented on inspected flows. Optional, and without it
	// no flow is ever inspected — an HTTPS credential rule then never fires, which is
	// reported rather than silently ignored.
	CA *ca.Authority

	// UpstreamRootCAs verifies origin certificates on inspected flows. Nil means the
	// host's own trust store, which is what production uses; tests set it so that a
	// throwaway origin can be verified as strictly as a real one.
	UpstreamRootCAs *x509.CertPool

	// Resolver turns a hostname into addresses. Defaults to the host resolver.
	// Overridable so tests need no DNS and no /etc/hosts.
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

	// DialAddr opens a TCP connection to an already-resolved address. Defaults to a
	// plain dialer. Resolution is separated from dialling on purpose: the address a
	// name resolved to is checked against the deny rules before any packet is sent.
	DialAddr func(ctx context.Context, addrPort netip.AddrPort) (net.Conn, error)

	// ErrorLog receives operational errors. Decisions go to the policy log, not here.
	// Nothing written here ever contains a credential.
	ErrorLog *log.Logger

	// ClientHelloTimeout bounds the wait for a TLS ClientHello inside a CONNECT tunnel
	// before the tunnel is spliced without an SNI check. Zero means the default.
	ClientHelloTimeout time.Duration

	// DialTimeout bounds a single upstream connection attempt. Zero means the default.
	DialTimeout time.Duration
}

const (
	defaultClientHelloTimeout = 5 * time.Second
	defaultDialTimeout        = 15 * time.Second
)

// Server is the forward proxy. It is an http.Handler, so it can be served on any
// listener, and it is safe for concurrent use.
type Server struct {
	cfg       Config
	http      *http.Server
	transport *http.Transport

	mu     sync.Mutex
	closed bool
}

// New builds a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Engine == nil {
		return nil, errors.New("proxy: no policy engine; the proxy must not run without a policy")
	}
	if cfg.Resolver == nil {
		cfg.Resolver = defaultResolve
	}
	if cfg.DialAddr == nil {
		cfg.DialAddr = defaultDial
	}
	if cfg.ClientHelloTimeout == 0 {
		cfg.ClientHelloTimeout = defaultClientHelloTimeout
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	s := &Server{cfg: cfg}
	s.transport = &http.Transport{
		Proxy: nil, // a forward proxy chaining to itself would be a loop
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			t, err := policy.ParseTarget(addr, 80)
			if err != nil {
				return nil, err
			}
			return s.dial(ctx, t, policy.ModeForward)
		},
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          32,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	s.http = &http.Server{
		Handler:  s,
		ErrorLog: cfg.ErrorLog,
		// A hostile client must not be able to pin connections open for free.
		ReadHeaderTimeout: 30 * time.Second,
	}
	return s, nil
}

// Serve accepts connections on l until Close is called.
func (s *Server) Serve(l net.Listener) error {
	err := s.http.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the proxy and releases idle upstream connections.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.transport.CloseIdleConnections()
	return s.http.Close()
}

// Engine exposes the policy engine, mostly so a caller can read the decision log.
func (s *Server) Engine() *policy.Engine { return s.cfg.Engine }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	s.handleHTTP(w, r)
}

// handleHTTP forwards a plaintext HTTP request.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w,
			"boks: this is a forward proxy, not an origin server.\n"+
				"Point HTTP_PROXY/HTTPS_PROXY at it, or send an absolute-URI request.\n",
			http.StatusBadRequest)
		return
	}
	target, err := policy.ParseTarget(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if err != nil {
		http.Error(w, "boks: cannot parse destination: "+err.Error()+"\n", http.StatusBadRequest)
		return
	}

	// Plaintext HTTP is readable by everything on the path, Boks included. Recording it
	// as a distinct flow keeps "we read this" true in the log without implying that a TLS
	// session was broken to do it.
	decision := s.cfg.Engine.CheckMode(policy.StageHTTP, target, policy.ModeForward)
	if !decision.Allowed {
		writeDenied(w, decision)
		return
	}

	outbound := r.Clone(r.Context())
	outbound.RequestURI = ""
	stripHopByHop(outbound.Header)
	// The guest's own proxy credentials are not ours to forward, and they are exactly
	// the kind of value that should not travel further than this process.
	outbound.Header.Del("Proxy-Authorization")
	outbound.Header.Del("Proxy-Connection")

	used, err := s.cfg.Injector.Apply(r.Context(), target, outbound.Header)
	if err != nil {
		// The error names secrets, never values; see internal/secret.
		http.Error(w, "boks: credential injection failed: "+err.Error()+"\n", http.StatusBadGateway)
		return
	}
	if len(used) > 0 {
		s.logf("injected credential %s for %s", strings.Join(used, ", "), target)
	}

	resp, err := s.transport.RoundTrip(outbound)
	if err != nil {
		s.logf("upstream %s: %v", target, err)
		http.Error(w, "boks: upstream request failed: "+err.Error()+"\n", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	header := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	stripHopByHop(header)
	header.Set("Boks-Policy", "allow")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.logf("copying response from %s: %v", target, err)
	}
}

// handleConnect establishes a tunnel, after checking the CONNECT target, the address it
// resolves to, and the server name in the client's ClientHello.
//
// The tunnel is spliced blind unless the destination has a credential rule, in which case
// it is handed to inspect() instead. That decision is taken here, from the CONNECT target
// alone, before the client has said anything: nothing a guest sends can widen it.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	target, err := policy.ParseTarget(r.Host, 443)
	if err != nil {
		http.Error(w, "boks: cannot parse CONNECT target: "+err.Error()+"\n", http.StatusBadRequest)
		return
	}

	mode := policy.ModeForwardBypass
	if s.shouldInspect(target) {
		mode = policy.ModeForward
	}

	decision := s.cfg.Engine.CheckMode(policy.StageConnect, target, mode)
	if !decision.Allowed {
		writeDenied(w, decision)
		return
	}
	if mode == policy.ModeForwardBypass && s.cfg.Injector.Handles(target) {
		// A credential rule that cannot fire is worse than no rule: the request goes out
		// unauthenticated and the guest's placeholder is what the origin sees. Say so
		// where the user is already looking.
		s.cfg.Engine.Note(policy.StageConnect, target, policy.ModeForwardBypass,
			"a credential rule names this host, but no certificate authority is configured, so the flow is carried blind and nothing is injected")
	}

	// Dial before answering, so that a refusal or a failure is reportable as HTTP
	// instead of a tunnel that dies for no visible reason.
	upstream, err := s.dial(r.Context(), target, mode)
	if err != nil {
		var denied *deniedError
		if errors.As(err, &denied) {
			writeDenied(w, denied.decision)
			return
		}
		http.Error(w, "boks: cannot reach "+target.String()+": "+err.Error()+"\n", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "boks: cannot establish a tunnel on this connection\n", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		s.logf("hijacking connection for %s: %v", target, err)
		return
	}
	defer client.Close()
	defer upstream.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\nBoks-Policy: allow\r\n\r\n")); err != nil {
		return
	}

	head, sni, err := s.readClientHello(client, buffered)
	if err != nil {
		s.logf("reading ClientHello for %s: %v", target, err)
		return
	}
	sniMatches := sni == "" || strings.EqualFold(sni, target.Host)
	if !sniMatches {
		// The client asked to tunnel to one host and then greeted another. Judge what
		// it actually greeted.
		sniTarget, err := policy.NewTarget(sni, target.Port)
		if err != nil {
			s.logf("CONNECT %s carried an unparseable server name: %v", target, err)
			return
		}
		if d := s.cfg.Engine.CheckMode(policy.StageSNI, sniTarget, mode); !d.Allowed {
			// The 200 is already on the wire, so the only refusal left is to drop the
			// tunnel. The client sees a broken handshake; the reason is in the
			// decision log, which is why the log is not optional.
			s.logf("closing tunnel to %s: %s", target, d.Reason)
			return
		}
	}

	// Inspection needs the flow to be TLS for the host that was judged. A ClientHello for
	// some other name, or bytes that are not TLS at all, fall back to a blind splice: the
	// destination was still checked, and terminating something we cannot name would be
	// interception nobody asked for.
	if mode == policy.ModeForward && sniMatches && looksLikeTLS(head) {
		s.inspect(r.Context(), target, client, buffered, head, upstream)
		return
	}
	if mode == policy.ModeForward {
		s.cfg.Engine.Note(policy.StageConnect, target, policy.ModeForwardBypass,
			"marked for inspection, but the tunnel is not TLS for this name; carried blind and no credential attached")
	}

	if len(head) > 0 {
		if _, err := upstream.Write(head); err != nil {
			return
		}
	}
	// Read the client through the hijacked buffer, not the raw connection: bytes the HTTP
	// server had already buffered are only reachable there, and skipping them would
	// silently truncate the tunnel's first request.
	s.splice(client, buffered, upstream)
}

// readClientHello reads enough of the client's first bytes to find the TLS server name,
// returning everything it consumed so the tunnel can replay it upstream.
//
// A tunnel carrying something other than TLS, or a client that says nothing, is not an
// error: the CONNECT target was already judged. Only the extra SNI cross-check is lost,
// and that is recorded by its absence from the decision log rather than by a failure.
func (s *Server) readClientHello(client net.Conn, buffered io.Reader) ([]byte, string, error) {
	deadline := time.Now().Add(s.cfg.ClientHelloTimeout)
	if err := client.SetReadDeadline(deadline); err != nil {
		return nil, "", err
	}
	defer client.SetReadDeadline(time.Time{})

	var head []byte
	tmp := make([]byte, 4096)
	for len(head) < maxClientHello {
		n, err := buffered.Read(tmp)
		if n > 0 {
			head = append(head, tmp[:n]...)
			name, needMore, perr := extractSNI(head)
			if perr == nil {
				return head, name, nil
			}
			if !needMore {
				return head, "", nil // not TLS, or TLS without SNI
			}
		}
		if err != nil {
			if isTimeout(err) || errors.Is(err, io.EOF) {
				return head, "", nil
			}
			return head, "", err
		}
	}
	return head, "", nil
}

// splice copies in both directions until either side finishes. clientReader is the
// hijacked connection's buffered reader; client itself is used only to close.
func (s *Server) splice(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// deniedError carries a policy decision out of the dial path.
type deniedError struct{ decision policy.Decision }

func (e *deniedError) Error() string { return e.decision.Reason }

// dial resolves a target and connects to it, checking each candidate address against the
// deny rules first.
//
// This second check is what stops a name-based allowlist from becoming a way into the
// host: `allowed.example A 127.0.0.1` passes a hostname allow and would otherwise reach
// whatever is listening on the host's loopback. Deny rules apply to the address that will
// actually be contacted, not to the name that was asked for.
func (s *Server) dial(ctx context.Context, t policy.Target, mode policy.Mode) (net.Conn, error) {
	var candidates []netip.Addr
	if t.IsIP() {
		candidates = []netip.Addr{t.Addr}
	} else {
		addrs, err := s.cfg.Resolver(ctx, t.Host)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", t.Host, err)
		}
		candidates = addrs
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s has no addresses", t.Host)
	}

	var (
		lastDenied *policy.Decision
		lastErr    error
	)
	for _, addr := range candidates {
		addr = addr.Unmap().WithZone("")
		resolved := policy.TargetFromAddr(netip.AddrPortFrom(addr, uint16(t.Port)))
		// An address literal was already judged by Check; judging it again would put a
		// duplicate in the log for every connection.
		if !t.IsIP() {
			if d := s.cfg.Engine.CheckResolved(resolved, mode); !d.Allowed {
				dc := d
				lastDenied = &dc
				continue
			}
		}
		dialCtx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
		conn, err := s.cfg.DialAddr(dialCtx, netip.AddrPortFrom(addr, uint16(t.Port)))
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil && lastDenied != nil {
		return nil, &deniedError{decision: *lastDenied}
	}
	if lastErr == nil {
		lastErr = errors.New("no usable address")
	}
	return nil, lastErr
}

func defaultResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func defaultDial(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", ap.String())
}

// writeDenied answers a blocked request with something a person can act on.
//
// A denial that looks like a network failure costs an hour of debugging the wrong thing,
// so it says what was blocked, why, which policy did it, and what to type next. The status
// is 403 and the body is plain text, which curl, git and every HTTP library surface.
func writeDenied(w http.ResponseWriter, d policy.Decision) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Boks-Policy", "deny")
	h.Set("Boks-Policy-Reason", d.Reason)
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprint(w, denialText(d))
}

// denialText is the body of a refusal, shared by the plain-HTTP path and the inspected
// path, which has no http.ResponseWriter to write through.
func denialText(d policy.Decision) string {
	return fmt.Sprintf(`boks: blocked by network policy

  destination: %s:%d
  stage:       %s
  policy:      %s
  reason:      %s

To permit it, add the destination when starting the sandbox:

  boks run --allow %s:%d ...

Recent decisions: boks policy log
`, d.Host, d.Port, d.Stage, d.Policy, d.Reason, d.Host, d.Port)
}

// hopByHop headers are per-connection and must not be forwarded. Connection lists further
// headers by name, and those go too.
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHop(h http.Header) {
	for _, name := range h.Values("Connection") {
		for _, token := range strings.Split(name, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

func defaultPortForScheme(scheme string) int {
	if strings.EqualFold(scheme, "https") {
		return 443
	}
	return 80
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.ErrorLog == nil {
		return
	}
	s.cfg.ErrorLog.Printf(format, args...)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// closeWrite half-closes a connection where the type allows it, so the peer sees EOF
// instead of waiting out a timeout.
func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
