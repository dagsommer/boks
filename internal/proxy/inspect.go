package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

const (
	// inspectIdleTimeout bounds how long an inspected connection may sit between
	// requests. Long enough for a keep-alive client, short enough that a guest cannot
	// pin file descriptors open for free.
	inspectIdleTimeout = 2 * time.Minute
	// inspectResponseTimeout bounds the wait for an origin's response headers, matching
	// the plain-HTTP transport's own limit.
	inspectResponseTimeout = 60 * time.Second
)

// shouldInspect is the whole opt-in rule for TLS interception, in one place so that it can
// be read and audited in one place.
//
// A flow is terminated only when the destination has a credential rule *and* an authority
// exists to sign with. There is no other input: no flag, no preset, no default, nothing the
// guest sends. If this function ever grows a second reason to return true, that reason
// needs to survive the same argument this one did.
func (s *Server) shouldInspect(t policy.Target) bool {
	return s.cfg.CA != nil && s.cfg.Injector.Handles(t)
}

// looksLikeTLS reports whether the bytes a client sent into a tunnel begin a TLS handshake.
// 0x16 is the handshake content type, the first byte of every ClientHello.
func looksLikeTLS(head []byte) bool { return len(head) > 0 && head[0] == 0x16 }

// inspect terminates a TLS flow, re-originates it, and proxies HTTP/1.1 between the two
// halves so that a credential can be attached to the request.
//
// The order of the two handshakes is deliberate. The client's is completed first, so that
// a failure to verify the *origin* can be reported to the guest as a readable 502 instead
// of a handshake that dies for no stated reason. Nothing the client sends is forwarded
// before the origin has been verified, so the guest is never talking to an unverified
// server through us.
func (s *Server) inspect(ctx context.Context, target policy.Target, client net.Conn, clientReader io.Reader, head []byte, upstream net.Conn) {
	leaf, err := s.cfg.CA.LeafFor(target.Host)
	if err != nil {
		s.logf("cannot mint a certificate for %s: %v", target, err)
		return
	}

	// The ClientHello was already consumed to read the SNI, so the TLS server has to see
	// those bytes again ahead of the rest of the stream.
	replay := &replayConn{Conn: client, r: io.MultiReader(bytes.NewReader(head), clientReader)}
	tlsClient := tls.Server(replay, &tls.Config{
		MinVersion: tls.VersionTLS12,
		// http/1.1 only: the inspected path speaks HTTP/1.1, and ALPN is the honest way
		// to say so. Tunnelled flows are untouched and may negotiate whatever they like.
		NextProtos: []string{"http/1.1"},
		// The certificate is chosen from the policy target, never from the ClientHello.
		// A guest must not be able to pick what the authority signs.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return leaf, nil },
	})
	if err := s.handshake(ctx, tlsClient); err != nil {
		s.logf("TLS handshake with the guest for %s failed: %v", target, err)
		return
	}

	tlsUpstream := tls.Client(upstream, &tls.Config{
		ServerName: target.Host,
		RootCAs:    s.cfg.UpstreamRootCAs,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := s.handshake(ctx, tlsUpstream); err != nil {
		// Boks verifies the origin on the guest's behalf now, so a verification failure
		// is Boks' to report. Saying so is important: the guest cannot tell a rejected
		// origin from a broken proxy by itself.
		s.logf("verifying the certificate of %s failed: %v", target, err)
		writeStatus(tlsClient, http.StatusBadGateway, fmt.Sprintf(
			"boks: refused to connect to %s\n\n"+
				"This host has a credential rule, so boks terminates TLS for it and verifies\n"+
				"the origin's certificate itself. That verification failed, so nothing was sent.\n\n"+
				"  reason: %v\n", target, err))
		return
	}

	s.serveInspected(ctx, target, tlsClient, tlsUpstream)
}

// serveInspected proxies HTTP/1.1 requests over an already-terminated flow.
//
// Bodies stream through in both directions and are never buffered, decoded or examined.
// The proxy reads request headers because it has to add one; it has no reason to look at
// anything else, and does not.
func (s *Server) serveInspected(ctx context.Context, target policy.Target, client, upstream *tls.Conn) {
	clientReader := bufio.NewReader(client)
	upstreamReader := bufio.NewReader(upstream)

	for {
		_ = client.SetReadDeadline(time.Now().Add(inspectIdleTimeout))
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if !isDisconnect(err) {
				// Deliberately not %v: a parse error from net/http quotes the bytes it
				// choked on, which is the request line — and a request line can carry a
				// token in a query string. Nothing derived from traffic is logged.
				s.logf("inspected flow to %s: the guest sent something that is not an HTTP/1.1 request", target)
			}
			return
		}
		_ = client.SetReadDeadline(time.Time{})

		if req.Method == http.MethodConnect {
			writeStatus(client, http.StatusBadRequest, "boks: CONNECT is not accepted inside an inspected flow\n")
			return
		}

		// A request inside the flow may name a different host than the tunnel did. Being
		// able to see and judge that is the one thing interception buys beyond injection,
		// so it is checked — and only checked. The credential still follows the host that
		// was actually connected to and whose certificate was verified, never the Host
		// header, or a guest could redirect one host's secret to another's origin.
		if host := requestHost(req); host != "" && !strings.EqualFold(host, target.Host) {
			t, err := policy.NewTarget(host, target.Port)
			if err != nil {
				writeStatus(client, http.StatusBadRequest, "boks: the request's Host header is not a usable destination\n")
				return
			}
			if d := s.cfg.Engine.CheckMode(policy.StageRequest, t, policy.ModeForward); !d.Allowed {
				writeStatus(client, http.StatusForbidden, denialText(d))
				return
			}
		}

		stripHopByHop(req.Header)
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		// net/http invents a User-Agent when writing a request that has none. A present
		// but empty header suppresses that, so the origin sees what the guest sent rather
		// than what Boks would have sent.
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "")
		}

		// An OAuth token request goes down one of two paths, and which one is decided by the
		// stored credential rather than by anything in the request. A credential that has no
		// token yet is being *acquired*: the request is relayed and the response kept. Every
		// other one is a refresh: the request is answered here and never forwarded. See
		// relayTokenRequest and answerTokenRequest, and internal/secret/acquire.go for why
		// they are opposites.
		if credential, ok := s.cfg.Injector.TokenEndpointFor(target, req.URL.Path); ok && req.Method == http.MethodPost {
			if s.cfg.Injector.NeedsAcquisition(ctx, credential) {
				if !s.relayTokenRequest(ctx, target, credential, req, client, upstream, upstreamReader) {
					return
				}
				continue
			}
			if !s.answerTokenRequest(ctx, target, credential, req, client) {
				return
			}
			continue
		}

		used, err := s.cfg.Injector.Apply(ctx, target, req.Header, secret.FlowTLS)
		if err != nil {
			// The error names secrets, never values; see internal/secret.
			writeStatus(client, http.StatusBadGateway, "boks: credential injection failed: "+err.Error()+"\n")
			return
		}
		if len(used) > 0 {
			s.logf("injected credential %s for %s (inspected flow)", strings.Join(used, ", "), target)
		}

		if err := req.Write(upstream); err != nil {
			s.logf("forwarding a request to %s: %v", target, err)
			return
		}

		_ = upstream.SetReadDeadline(time.Now().Add(inspectResponseTimeout))
		resp, err := http.ReadResponse(upstreamReader, req)
		_ = upstream.SetReadDeadline(time.Time{})
		if err != nil {
			// Same reasoning as above: a malformed-response error quotes the origin's
			// bytes, and those are traffic.
			s.logf("inspected flow to %s: the origin's response could not be read", target)
			writeStatus(client, http.StatusBadGateway, "boks: the origin's response could not be read\n")
			return
		}

		writeErr := resp.Write(client)
		resp.Body.Close()
		if writeErr != nil {
			return
		}

		if resp.StatusCode == http.StatusSwitchingProtocols {
			// After an upgrade the connection stops being HTTP. Boks cannot read it and
			// says so rather than pretending the rest of the flow was inspected.
			s.cfg.Engine.Note(policy.StageRequest, target, policy.ModeForwardBypass,
				"protocol upgrade inside an inspected flow; the remainder is carried opaquely")
			s.splice(client, clientReader, upstream)
			return
		}
		if req.Close || resp.Close {
			return
		}
	}
}

// answerTokenRequest answers a guest's OAuth token request from the host, without
// forwarding it, and reports whether the connection may carry another request.
//
// This is the refresh decision made concrete. The guest's bytes stop here: they are drained
// and discarded, never parsed for parameters and never relayed, so no request a guest can
// compose reaches the token endpoint carrying the real refresh token. What goes back is a
// response Boks composed — the same sentinels the guest already holds, and a lifetime that
// says the expiry is not its concern. An agent that persists what a refresh returned
// therefore writes back exactly what it had, which is the property that makes this survive a
// guest rewriting its own credential file.
//
// The real exchange, when one is needed, happens on the host inside ExchangeToken: a
// separate HTTPS request from this process, composed here, that no guest input touches.
func (s *Server) answerTokenRequest(ctx context.Context, target policy.Target, credential secret.Credential,
	req *http.Request, client net.Conn) bool {

	// The body has to be consumed for the connection to stay framed, and it is discarded
	// unread: a guest's refresh parameters are not input to anything Boks does.
	_, drainErr := io.Copy(io.Discard, io.LimitReader(req.Body, maxTokenRequestBody))
	req.Body.Close()

	exchange, err := s.cfg.Injector.ExchangeToken(ctx, credential)
	if err != nil {
		// Errors here name the service and never a token; see internal/secret.
		s.logf("oauth token request for %s on %s: %v", credential.Service, target, err)
		writeStatus(client, http.StatusBadGateway, "boks: "+err.Error()+"\n")
		return false
	}
	s.cfg.Engine.Note(policy.StageRequest, target, policy.ModeForward,
		"oauth token request for "+credential.Service+" answered on the host; the guest keeps its sentinels")
	s.logf("answered an oauth token request for %s on %s (host-side refresh: %v)",
		credential.Service, target, exchange.Refreshed)

	writeJSON(client, exchange.Status, exchange.Body)
	// A body larger than the cap means the framing is no longer trustworthy, so this
	// connection ends after the answer rather than risking the next request being read out
	// of the middle of this one.
	return drainErr == nil
}

// maxTokenRequestBody bounds what is drained from a token request. A real one is a few
// hundred bytes; anything beyond this is not a refresh.
const maxTokenRequestBody = 64 << 10

// relayTokenRequest forwards a guest's token request and keeps the tokens that come back,
// giving the guest sentinels in their place. It reports whether the connection may carry
// another request.
//
// This is the one place in Boks where a guest-composed request reaches a token endpoint, and
// the one place an origin's response body is buffered rather than streamed. Both are
// exceptions to rules the inspected path otherwise keeps, and both exist for the same reason:
// the credential is being *acquired*, so the thing that unlocks it — an authorization code
// and a PKCE verifier from a redirect the guest received — exists only inside the guest, and
// the thing that comes back is exactly what must not.
//
// The exceptions are bounded by the stored record, not by the request. The Injector permits
// this path only for a credential with no token at all, and the first token it acquires closes
// it: a guest cannot ask to be relayed, and cannot re-open the path once it has been used.
//
// Order of operations, which is not an implementation detail. The request is relayed
// unchanged but for an Accept-Encoding that lets the answer be read; the whole response is
// read, up to a cap, and anything larger or still encoded is refused; the tokens are taken
// out, stored, and masked out of the body. Only then is the guest answered. Answering first
// would mean a store failure left the guest with a sentinel, Boks with nothing, and an
// authorization code that cannot be spent twice.
func (s *Server) relayTokenRequest(ctx context.Context, target policy.Target, credential secret.Credential,
	req *http.Request, client net.Conn, upstream *tls.Conn, upstreamReader *bufio.Reader) bool {

	secret.PrepareRelay(req.Header)
	err := req.Write(upstream)
	req.Body.Close()
	if err != nil {
		s.logf("relaying an oauth token request for %s to %s failed", credential.Service, target)
		writeStatus(client, http.StatusBadGateway, "boks: the login exchange could not be sent\n")
		return false
	}

	_ = upstream.SetReadDeadline(time.Now().Add(inspectResponseTimeout))
	resp, err := http.ReadResponse(upstreamReader, req)
	_ = upstream.SetReadDeadline(time.Time{})
	if err != nil {
		// Same reasoning as the ordinary path: a malformed-response error quotes the
		// origin's bytes, and on this endpoint those bytes are the token. Nothing derived
		// from the response is logged here or below.
		s.logf("oauth token request for %s on %s: the origin's response could not be read", credential.Service, target)
		writeStatus(client, http.StatusBadGateway, "boks: the login exchange got no readable answer\n")
		return false
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, secret.MaxTokenResponseBody+1))
	resp.Body.Close()
	if readErr != nil || len(body) > secret.MaxTokenResponseBody {
		// Fail closed. A body Boks could not hold entire is a body it could not mask, and
		// forwarding it would be handing over whatever is inside unread.
		s.logf("oauth token request for %s on %s: the origin's response was too large to inspect", credential.Service, target)
		writeStatus(client, http.StatusBadGateway, "boks: the login exchange returned more than boks will read\n")
		return false
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		// Also fail closed, and for the same reason: the inspected path decodes nothing, so
		// an encoded body cannot be masked. The request asked for identity; an origin that
		// ignored that is not one whose answer can be forwarded.
		s.logf("oauth token request for %s on %s: the origin answered with a content encoding boks cannot inspect", credential.Service, target)
		writeStatus(client, http.StatusBadGateway, "boks: the login exchange returned an encoded body boks will not forward unread\n")
		return false
	}

	acquired, err := s.cfg.Injector.AcquireToken(ctx, credential, resp.StatusCode, body)
	if err != nil {
		// Errors from internal/secret name the service and the kind of failure, never a
		// value; the masking assertion in particular is written to say which token survived
		// without saying what it was.
		s.logf("acquiring the oauth credential %s on %s: %v", credential.Service, target, err)
		writeStatus(client, http.StatusBadGateway, "boks: "+err.Error()+"\n")
		return false
	}

	if acquired.Acquired {
		s.cfg.Engine.Note(policy.StageRequest, target, policy.ModeForward,
			"oauth login for "+credential.Service+" completed inside the sandbox; the tokens were kept on the host and the guest received sentinels")
		s.logf("acquired the oauth credential %s from a login on %s (the guest received sentinels)",
			credential.Service, target)
	} else {
		s.logf("an oauth login exchange for %s on %s returned no token (status %d); the origin's answer was passed through",
			credential.Service, target, acquired.Status)
	}

	// The origin's own headers are not reused. They describe a body that no longer exists —
	// its length, and possibly its encoding — and the two fields that matter are set here.
	//
	// The connection is closed with the answer. The agent's very next request is a refresh
	// with the sentinel it has just been given, which the answering path serves on a fresh
	// connection just as well; ending here means nothing downstream has to reason about a
	// relayed response and a composed one sharing one keep-alive.
	writeJSONClosing(client, acquired.Status, acquired.Body)
	return false
}

// writeJSON sends a JSON response Boks composed itself on a connection with no
// http.ResponseWriter. The body is built in internal/secret from sentinels, never from a
// token and never from anything read off the wire.
func writeJSON(w io.Writer, status int, body []byte) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n"+
		"Content-Type: application/json\r\n"+
		"Cache-Control: no-store\r\n"+
		"Content-Length: %d\r\n"+
		"Boks-Policy: allow\r\n\r\n",
		status, http.StatusText(status), len(body))
	_, _ = w.Write(body)
}

// writeJSONClosing is writeJSON for an answer after which the connection ends. Saying so is
// the difference between a client that opens a fresh connection and one that writes its next
// request into a socket that is already going away.
func writeJSONClosing(w io.Writer, status int, body []byte) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n"+
		"Content-Type: application/json\r\n"+
		"Cache-Control: no-store\r\n"+
		"Content-Length: %d\r\n"+
		"Boks-Policy: allow\r\n"+
		"Connection: close\r\n\r\n",
		status, http.StatusText(status), len(body))
	_, _ = w.Write(body)
}

// handshake runs a TLS handshake under the dial timeout.
func (s *Server) handshake(ctx context.Context, c *tls.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()
	return c.HandshakeContext(ctx)
}

// requestHost is the host a request claims to be for, without its port.
func requestHost(r *http.Request) string {
	host := r.Host
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// writeStatus sends a plain-text HTTP/1.1 response on a connection that has no
// http.ResponseWriter, which is every response Boks originates inside an inspected flow.
//
// Only text Boks wrote itself is sent; nothing read from traffic is echoed back.
func writeStatus(w io.Writer, status int, body string) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Boks-Policy: %s\r\n"+
		"Connection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), boksPolicyHeader(status), body)
}

func boksPolicyHeader(status int) string {
	if status == http.StatusForbidden {
		return "deny"
	}
	return "error"
}

// isDisconnect reports whether an error is just the other end going away, which is how
// every keep-alive connection ends and is not worth a log line.
func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		isTimeout(err)
}

// replayConn is a connection whose reads come from somewhere else — the already-consumed
// ClientHello followed by the rest of the client's stream — while writes and the connection
// interface itself stay with the real socket.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }
