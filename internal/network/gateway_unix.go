//go:build !windows

package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/dagsommer/boks/internal/policy"
)

// gateway holds one sandbox's link socket and the stack behind it.
//
// See gateway.go for why the stack is embedded rather than spawned as gvproxy, and
// stack_unix.go for why the stack is assembled by hand rather than taken whole from
// gvisor-tap-vsock's virtualnetwork.New — that constructor installs a TCP forwarder which
// dials whatever the guest asks for, with no policy in the path.
//
// The transport is a SOCK_DGRAM UNIX socket, which is why this file is Unix-only:
// gvisor-tap-vsock has no unixgram transport on Windows, and nerdbox does not support
// Windows either, so there is nothing to lose by saying so plainly.
type gateway struct {
	mu    sync.Mutex
	conn  *net.UnixConn
	stack *hostStack
}

func (g *gateway) start(ctx context.Context, plan Plan, engine *policy.Engine, logger io.Writer) error {
	hs, err := newHostStack(ctx, plan, engine, logger)
	if err != nil {
		return err
	}

	// The vfkit transport is a SOCK_DGRAM UNIX socket, which is what the VM's
	// `mode=unixgram` annotation matches. Bind before returning: the VM connects during
	// boot, and a socket that appears late is a boot failure, not a retry.
	conn, err := transport.ListenUnixgram("unixgram://" + plan.Socket)
	if err != nil {
		hs.stop()
		return fmt.Errorf("network: binding the link socket %s: %w", plan.Socket, err)
	}

	g.mu.Lock()
	g.conn = conn
	g.stack = hs
	g.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	go func() {
		// AcceptVfkit blocks until the VM sends its first datagram, then binds the
		// return path to that peer. One VM per stack, so there is no accept loop.
		vmConn, err := transport.AcceptVfkit(conn)
		if err != nil {
			logf(logger, "network: the VM never attached to %s: %v", plan.Socket, err)
			return
		}
		if err := hs.accept(ctx, vmConn); err != nil && ctx.Err() == nil {
			logf(logger, "network: link to the VM ended: %v", err)
		}
	}()
	return nil
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

// stop closes the link socket and tears the stack down, including any flow it was carrying.
//
// Both halves matter. Releasing the socket lets the next run of the same sandbox bind it;
// stopping the stack is what makes "this sandbox no longer has a network" true for a
// connection the guest opened a moment before teardown.
func (g *gateway) stop() error {
	g.mu.Lock()
	conn, hs := g.conn, g.stack
	g.mu.Unlock()

	var err error
	if conn != nil {
		if cerr := conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = fmt.Errorf("network: closing the link socket: %w", cerr)
		}
	}
	if hs != nil {
		hs.stop()
	}
	return err
}
