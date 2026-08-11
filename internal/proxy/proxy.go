// Package proxy is the host-side forward proxy that applies a network policy to a
// sandbox's HTTP and HTTPS traffic and attaches credentials the guest never sees.
//
// # This is not an enforcement boundary yet
//
// A forward proxy only sees traffic a client chooses to send it. Boks' current VM runtime
// uses libkrun's TSI, which rewrites the guest's socket calls and performs the connection
// on the host, so a guest that ignores HTTP_PROXY and opens a raw socket bypasses every
// decision in this package. **An environment variable is not a security boundary.** Real
// enforcement needs the guest's virtio-net link terminated by a host-side network stack
// that can drop packets; until that exists, this proxy is a cooperating-client mechanism —
// useful for shaping and observing what well-behaved tooling does, worthless against
// tooling that does not want to be shaped.
//
// Nothing in this package is wired into `boks run`'s datapath. It runs standalone, under
// `boks proxy`, so the policy engine and credential path can be built and tested while the
// netstack question is settled.
//
// # No TLS interception
//
// The proxy never terminates TLS. There is no custom CA, the guest validates the real
// certificate chain of the real origin, and the proxy cannot read request or response
// bodies. HTTPS is filtered on two things it can see without decrypting: the CONNECT
// target, and the server name in the TLS ClientHello.
//
// The cost of that choice is stated plainly here and in docs/security-model.md:
//
//   - SNI can be omitted, or can name a host the client never talks to. It is a
//     cross-check on the CONNECT target, not an independent guarantee. Encrypted Client
//     Hello removes it entirely.
//   - Hostname rules mean nothing for a raw socket. Only address and port rules apply
//     there, and only once something can see raw sockets at all.
//   - DNS is a covert channel unless resolution is mediated too. Traffic through this
//     proxy carries names rather than resolving them in the guest, which helps; a guest
//     that can send its own UDP does not have to cooperate.
//   - Credentials can only be injected into requests the proxy can read, which today
//     means plaintext HTTP. See internal/secret.
package proxy

import (
	"context"
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

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// Config configures a Server. Only Engine is required.
type Config struct {
	// Engine decides destinations and records every decision.
	Engine *policy.Engine

	// Injector attaches credentials to permitted requests. Optional.
	Injector *secret.Injector

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
			return s.dial(ctx, t)
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

	decision := s.cfg.Engine.Check(policy.StageHTTP, target)
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
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	target, err := policy.ParseTarget(r.Host, 443)
	if err != nil {
		http.Error(w, "boks: cannot parse CONNECT target: "+err.Error()+"\n", http.StatusBadRequest)
		return
	}

	decision := s.cfg.Engine.Check(policy.StageConnect, target)
	if !decision.Allowed {
		writeDenied(w, decision)
		return
	}

	// Dial before answering, so that a refusal or a failure is reportable as HTTP
	// instead of a tunnel that dies for no visible reason.
	upstream, err := s.dial(r.Context(), target)
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
	if sni != "" && !strings.EqualFold(sni, target.Host) {
		// The client asked to tunnel to one host and then greeted another. Judge what
		// it actually greeted.
		sniTarget, err := policy.NewTarget(sni, target.Port)
		if err != nil {
			s.logf("CONNECT %s carried an unparseable server name: %v", target, err)
			return
		}
		if d := s.cfg.Engine.Check(policy.StageSNI, sniTarget); !d.Allowed {
			// The 200 is already on the wire, so the only refusal left is to drop the
			// tunnel. The client sees a broken handshake; the reason is in the
			// decision log, which is why the log is not optional.
			s.logf("closing tunnel to %s: %s", target, d.Reason)
			return
		}
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
func (s *Server) dial(ctx context.Context, t policy.Target) (net.Conn, error) {
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
			if d := s.cfg.Engine.CheckResolved(resolved); !d.Allowed {
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
	fmt.Fprintf(w, `boks: blocked by network policy

  destination: %s:%d
  stage:       %s
  policy:      %s
  reason:      %s

To permit it, add the destination when starting the sandbox:

  boks run -allow %s:%d ...

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
