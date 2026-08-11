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
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// gateway is a gvisor-tap-vsock userspace network stack, running in this process.
// See gateway.go for why the stack is embedded rather than spawned as gvproxy.
//
// The transport is a SOCK_DGRAM UNIX socket, which is why this file is Unix-only:
// gvisor-tap-vsock has no unixgram transport on Windows, and nerdbox does not support
// Windows either, so there is nothing to lose by saying so plainly.
type gateway struct {
	mu   sync.Mutex
	conn *net.UnixConn
	vn   *virtualnetwork.VirtualNetwork
}

func (g *gateway) start(ctx context.Context, plan Plan, logger io.Writer) error {
	cfg := gatewayConfig(plan)
	vn, err := virtualnetwork.New(cfg)
	if err != nil {
		return fmt.Errorf("network: starting the host network stack: %w", err)
	}

	// The vfkit transport is a SOCK_DGRAM UNIX socket, which is what the VM's
	// `mode=unixgram` annotation matches. Bind before returning: the VM connects during
	// boot, and a socket that appears late is a boot failure, not a retry.
	conn, err := transport.ListenUnixgram("unixgram://" + plan.Socket)
	if err != nil {
		return fmt.Errorf("network: binding the link socket %s: %w", plan.Socket, err)
	}

	g.mu.Lock()
	g.conn = conn
	g.vn = vn
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
		if err := vn.AcceptVfkit(ctx, vmConn); err != nil && ctx.Err() == nil {
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
	vn := g.vn
	g.mu.Unlock()
	if vn == nil {
		return nil, fmt.Errorf("%w: the stack is not running", ErrNoNetwork)
	}
	l, err := vn.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("network: listening on %s inside the sandbox network: %w", addr, err)
	}
	return l, nil
}

func (g *gateway) dial(ctx context.Context, addr string) (net.Conn, error) {
	g.mu.Lock()
	vn := g.vn
	g.mu.Unlock()
	if vn == nil {
		return nil, fmt.Errorf("%w: the stack is not running", ErrNoNetwork)
	}
	conn, err := vn.DialContextTCP(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("network: dialling %s inside the sandbox network: %w", addr, err)
	}
	return conn, nil
}

func (g *gateway) stop() error {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return nil
	}
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("network: closing the link socket: %w", err)
	}
	return nil
}
