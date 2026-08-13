package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/dagsommer/boks/internal/policy"
)

// Why the host-side stack is embedded rather than spawned as gvproxy.
//
// The alternative is to exec the gvproxy binary once per sandbox. Running gvisor-tap-vsock
// as a library was measured to be workable — it builds and cross-compiles for darwin/arm64
// and windows/amd64, and adds 115 modules to the graph — and it is better on the two axes
// that matter here:
//
//   - Lifetime. A child process outlives a crashed parent; a goroutine cannot. There is no
//     PID to track, no socket-path race on restart, no orphan to reap after a SIGKILL, and
//     no "is gvproxy installed, and which version" question for `boks doctor` to answer.
//     One stack serves exactly one VM — a second VM on the same stack gets a duplicate
//     address and a third fails to attach — so its lifetime has to be exactly a sandbox's,
//     which is far easier to guarantee inside the process that owns the sandbox.
//   - Control. The closed posture in gatewayConfig is asserted in a typed configuration
//     that a test can read back. Through the binary it would be the *absence* of
//     command-line flags, which no test can assert and any later refactor can silently
//     undo.
//
// The cost is a large dependency — gvisor's netstack — in the Boks binary. That is real and
// accepted: it is the component doing the security-relevant work, and having it pinned in
// go.mod is preferable to depending on whichever gvproxy happens to be on PATH.

// gatewayConfig builds the stack's configuration.
//
// **Every field that could expose the host is set explicitly, to zero.** The spike that
// confirmed this transport observed that the host was unreachable from the guest — but that
// was a property of how the stack happened to be configured, not a guarantee of the
// library. gvisor-tap-vsock can be told to translate an address to the host's loopback, to
// forward host ports inward, to answer on extra gateway addresses, and to proxy the EC2
// metadata service. Boks asserts all four closed here rather than trusting a default, so
// that a version bump that changes a default cannot quietly open the host, and so that a
// test can read the intent back.
//
// Since Boks assembles the stack itself (stack.go) this value no longer reaches a
// library constructor that could act on those four fields: nothing in Boks implements NAT,
// port forwarding, virtual gateway addresses or a metadata proxy at all, so they are closed
// by construction as well as by declaration. What the configuration is still *used* for is
// the addressing, and the two services the gateway runs inside the stack — the resolver and
// the address server. It is kept in this shape because it remains the one place a reader can
// see the whole posture, and because the assertion is cheap to keep and expensive to
// rediscover.
func gatewayConfig(plan Plan) *types.Configuration {
	return &types.Configuration{
		MTU:               plan.MTU,
		Subnet:            plan.Subnet.String(),
		GatewayIP:         plan.Gateway.String(),
		GatewayMacAddress: plan.GatewayMAC,

		// Closed posture, asserted rather than assumed:
		NAT:               map[string]string{}, // no address translated to the host
		Forwards:          map[string]string{}, // no host port forwarded into the guest
		GatewayVirtualIPs: nil,                 // the gateway answers on one address only
		Ec2MetadataAccess: false,               // no proxy to 169.254.169.254

		// The gateway's own resolver answers the guest, because the container's
		// resolv.conf points at it (see Plan.Annotations). Leaving DNS empty means it
		// resolves through the host's resolver rather than serving invented records.
		DNS:              nil,
		DNSSearchDomains: nil,

		Debug:       false,
		CaptureFile: "", // packet capture writes guest traffic to disk; opt-in only
	}
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}

// gateway holds one sandbox's link socket and the stack behind it.
//
// See above for why the stack is embedded rather than spawned as gvproxy, stack.go for why
// it is assembled by hand rather than taken whole from gvisor-tap-vsock's
// virtualnetwork.New, and link.go for the framing.
//
// Boks listens and the VMM connects, which is the direction libkrun's unixstream backend
// works in: nerdbox passes the socket *path* to krun_add_net_unixstream, and its `vfkit`
// flag is documented as a magic sequence libkrun sends "after connecting to the socket".
// The socket therefore has to exist before the task starts — a socket that appears late is
// a boot failure, not a retry — which is why start binds before it returns, exactly as the
// datagram transport did. What protects it is unchanged too: it lives in a directory this
// package creates 0700, so nothing another user runs can reach the sandbox's link.
type gateway struct {
	mu       sync.Mutex
	listener net.Listener
	// conn is the connection currently holding the link, if any. One VM per stack, so
	// there is at most one, and stop closes it: a write blocked on a peer that stopped
	// reading is released by closing the socket and by nothing else.
	conn    net.Conn
	stack   *hostStack
	stopped bool
	// notices counts the link-lifecycle lines written so far. See notice.
	notices int
}

// maxLinkNotices caps how many lines about connecting and disconnecting one gateway will
// ever write.
//
// Whoever is on the other end of the socket chooses how often it connects, and the stack log
// is a file on disk: without a cap, a connect/disconnect loop is a disk-usage primitive. This
// is the same reasoning as maxNoticedDestinations in stack.go, where the guest chooses the
// volume of dropped packets, and the same shape of answer — enough lines to diagnose the
// problem, not enough to be a problem.
const maxLinkNotices = 16

// notice writes a bounded operational line about the link's connection lifecycle.
func (g *gateway) notice(logger io.Writer, format string, args ...any) {
	g.mu.Lock()
	g.notices++
	count := g.notices
	g.mu.Unlock()
	switch {
	case count < maxLinkNotices:
		logf(logger, format, args...)
	case count == maxLinkNotices:
		logf(logger, format, args...)
		logf(logger, "network: further messages about this link's connections are suppressed")
	}
}

func (g *gateway) start(ctx context.Context, plan Plan, engine *policy.Engine, logger io.Writer) error {
	hs, err := newHostStack(ctx, plan, engine, logger)
	if err != nil {
		return err
	}

	listener, err := net.Listen("unix", plan.Socket)
	if err != nil {
		hs.stop()
		return fmt.Errorf("network: binding the link socket %s: %w", plan.Socket, err)
	}

	g.mu.Lock()
	g.listener = listener
	g.stack = hs
	g.mu.Unlock()

	go func() {
		<-ctx.Done()
		// Closing the listener ends the accept loop; closing the live connection ends
		// the switch's receive loop and releases any blocked write.
		g.closeLink()
	}()
	go g.serve(ctx, listener, plan, hs, logger)
	return nil
}

// serve carries one connection at a time, for as long as the link socket is open.
//
// A stream link has two states a datagram socket never had, and both are handled here.
//
// The VM connects *late* — during boot, some time after this returns — so there is nothing
// to wait for and nothing to retry; the accept loop simply blocks until it arrives. And a
// VMM that restarts reconnects, which must not leave the sandbox with a dead network for the
// rest of its life; the loop goes back to accepting when a connection ends. What it does not
// do is carry two at once: one stack serves one VM, and a second peer on the same socket
// would put an Ethernet device on the sandbox's fabric that nothing accounted for. The first
// connection holds the link and later ones are closed.
func (g *gateway) serve(ctx context.Context, listener net.Listener, plan Plan, hs *hostStack, logger io.Writer) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !g.stopping() {
				logf(logger, "network: the link socket %s stopped accepting connections: %v", plan.Socket, err)
			}
			return
		}
		if !g.claim(conn) {
			g.notice(logger, "network: refused a second connection to the link socket %s; one VM per sandbox", plan.Socket)
			_ = conn.Close()
			continue
		}
		go func() {
			defer g.release(conn)
			if err := hs.accept(ctx, conn); err != nil && ctx.Err() == nil && !g.stopping() {
				g.notice(logger, "network: link to the VM ended: %v", err)
			}
		}()
	}
}

// claim gives a connection the link, or refuses it because something else holds it or the
// gateway is being torn down.
func (g *gateway) claim(conn net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.conn != nil {
		return false
	}
	g.conn = conn
	return true
}

func (g *gateway) release(conn net.Conn) {
	g.mu.Lock()
	if g.conn == conn {
		g.conn = nil
	}
	g.mu.Unlock()
	_ = conn.Close()
}

// closeLink drops the listener and whatever is attached, without tearing down the stack.
// Context cancellation goes through here; stop does the rest.
func (g *gateway) closeLink() {
	g.mu.Lock()
	listener, conn := g.listener, g.conn
	g.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// listen returns a listener inside the virtual network. gvisor's stack is the only thing
// that can answer on the gateway address, so a listener from here is unreachable from the
// host's own network stack — which is exactly the property the proxy wants.
func (g *gateway) listen(addr string) (net.Listener, error) {
	g.mu.Lock()
	hs := g.stack
	g.mu.Unlock()
	if hs == nil {
		return nil, fmt.Errorf("%w: the stack is not running", ErrNoNetwork)
	}
	return hs.listen(addr)
}

func (g *gateway) dial(ctx context.Context, addr string) (net.Conn, error) {
	g.mu.Lock()
	hs := g.stack
	g.mu.Unlock()
	if hs == nil {
		return nil, fmt.Errorf("%w: the stack is not running", ErrNoNetwork)
	}
	return hs.dial(ctx, addr)
}

func (g *gateway) stopping() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopped
}

// stop closes the link socket and tears the stack down, including any flow it was carrying.
//
// Both halves matter. Releasing the socket lets the next run of the same sandbox bind it;
// stopping the stack is what makes "this sandbox no longer has a network" true for a
// connection the guest opened a moment before teardown.
func (g *gateway) stop() error {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return nil
	}
	g.stopped = true
	listener, conn, hs := g.listener, g.conn, g.stack
	g.conn = nil
	g.mu.Unlock()

	var err error
	if listener != nil {
		if cerr := listener.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = fmt.Errorf("network: closing the link socket: %w", cerr)
		}
	}
	if conn != nil {
		_ = conn.Close()
	}
	if hs != nil {
		hs.stop()
	}
	return err
}
