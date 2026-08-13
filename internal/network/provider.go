package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/dagsommer/boks/internal/policy"
)

// Network is a started, or startable, host-side network for one sandbox.
//
// One instance serves exactly one sandbox. That is not a simplification: pointing a second
// VM at the same host stack hands out a duplicate address, and a third fails to attach at
// all. The lifetime of a Network is therefore the lifetime of its sandbox — create it,
// Start it before the task starts, and Stop it in the same defer that tears the sandbox
// down.
type Network struct {
	plan     Plan
	provider provider
	logger   io.Writer
	engine   *policy.Engine

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
}

// provider is the host-side end of the link.
type provider interface {
	// start binds the link socket and begins serving. It must return only once the
	// socket exists, because the VM will connect to it as soon as the task starts.
	//
	// The engine is the policy every flow the guest opens is judged against, in the
	// stack, before it is dialled. A provider that is given none must refuse everything:
	// a network whose judge is missing is not an open network.
	start(ctx context.Context, plan Plan, engine *policy.Engine, logger io.Writer) error
	// listen returns a listener *inside* the virtual network, at addr. See
	// (*Network).Listen for why that is the only place Boks' proxy should listen.
	listen(addr string) (net.Listener, error)
	// dial opens a connection into the virtual network from the host side.
	dial(ctx context.Context, addr string) (net.Conn, error)
	stop() error
}

// ErrNoNetwork is returned by Listen and Dial when the sandbox has no virtual network to
// listen in — ModeNone, or a stack that was never started.
var ErrNoNetwork = errors.New("network: this sandbox has no virtual network")

// New builds a Network from a Config. Nothing is started and no socket exists yet.
func New(cfg Config) (*Network, error) {
	plan, err := NewPlan(cfg)
	if err != nil {
		return nil, err
	}
	return NewFromPlan(plan)
}

// NewFromPlan builds a Network for an already-computed Plan.
//
// It exists because the process that computes a sandbox's annotations and the process that
// serves the other end of its link are not always the same one. Passing the plan across,
// rather than recomputing it, is what makes it impossible for the two to disagree about the
// socket path or the addressing — a disagreement that would show up as a VM that boots with
// a NIC connected to nothing.
func NewFromPlan(plan Plan) (*Network, error) {
	var p provider
	switch plan.Mode {
	case ModeNone:
		p = &blackhole{}
	case ModeNAT:
		p = &gateway{}
	default:
		return nil, fmt.Errorf("network: unsupported mode %q", plan.Mode)
	}
	return &Network{plan: plan, provider: p}, nil
}

// Plan returns the computed plan.
func (n *Network) Plan() Plan { return n.plan }

// Listen returns a TCP listener **inside** the sandbox's virtual network, on the gateway
// address at port.
//
// This is where Boks' filtering proxy listens, and the reason is worth stating: a listener
// obtained this way exists only in one sandbox's virtual network. Nothing is bound on the
// host, so no other process, no other sandbox and nothing on the LAN can reach it, and two
// sandboxes cannot collide on a port. Binding a host port and telling the guest to use it
// would have all three problems.
//
// ModeNone has no virtual network to listen in and returns ErrNoNetwork: a sandbox with no
// network must not acquire one through the back door of a proxy.
func (n *Network) Listen(port int) (net.Listener, error) {
	if err := n.running(); err != nil {
		return nil, err
	}
	return n.provider.listen(net.JoinHostPort(n.plan.Gateway.String(), strconv.Itoa(port)))
}

// Dial opens a connection from the host side to the *gateway* inside the sandbox's virtual
// network — that is, to something Boks itself bound there, such as the proxy.
//
// It is the inverse of Listen, and it is how a test drives the datapath without a hypervisor:
// dial the gateway, speak to whatever Boks put there. It is host→guest only. Nothing here
// lets the guest reach the host; that direction is decided by the policy engine in front of
// the proxy.
func (n *Network) Dial(ctx context.Context, port int) (net.Conn, error) {
	if err := n.running(); err != nil {
		return nil, err
	}
	return n.provider.dial(ctx, net.JoinHostPort(n.plan.Gateway.String(), strconv.Itoa(port)))
}

// DialGuest opens a connection from the host side to a port on the *container's* address
// inside the virtual network. It is the second half of port publishing.
//
// The distinction from Dial is the whole feature. Dial reaches the gateway, which is Boks'
// own end of the link; DialGuest reaches the sandbox, across the same virtio-net link a real
// VM is attached to. A published host port is exactly this: accept on the host's loopback,
// dial here, splice the two.
//
// Two properties are worth stating, because they are what keeps this from being a general
// opening of the sandbox. It reaches the guest's *external* interface, so a service bound only
// to the guest's own 127.0.0.1 is not reachable — the same constraint Docker Sandboxes
// documents. And it is strictly host→guest: such a connection exists only because something
// on the host opened it, and nothing in this direction gives the guest a way to reach the
// host.
func (n *Network) DialGuest(ctx context.Context, port int) (net.Conn, error) {
	if err := n.running(); err != nil {
		return nil, err
	}
	return n.provider.dial(ctx, net.JoinHostPort(n.plan.GuestAddr.Addr().String(), strconv.Itoa(port)))
}

func (n *Network) running() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.started || n.stopped {
		return fmt.Errorf("%w: it has not been started", ErrNoNetwork)
	}
	return nil
}

// Annotations returns the OCI annotations the sandbox must be created with.
func (n *Network) Annotations() map[string]string { return n.plan.Annotations() }

// SetLogger routes the provider's diagnostics somewhere. Nil discards them.
func (n *Network) SetLogger(w io.Writer) { n.logger = w }

// SetPolicy attaches the engine the stack judges the guest's connections with. It must be
// called before Start; afterwards it has no effect, because a policy that could change under
// a flow already in progress would make the decision log a description of nothing.
//
// This is the enforcement point, not a hint to it. Every TCP connection the guest opens is
// checked against this engine from the address and port in its SYN, whether or not the guest
// used the proxy, and both outcomes are recorded with mode "transparent". A Network started
// without one refuses every destination.
func (n *Network) SetPolicy(e *policy.Engine) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return
	}
	n.engine = e
}

// Start creates the socket directory and starts the host-side stack.
//
// It must be called before the container task starts: libkrun connects to the socket
// during boot, and a socket that does not exist yet is a boot failure rather than a retry.
func (n *Network) Start(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return errors.New("network: already started")
	}
	if n.stopped {
		return errors.New("network: already stopped")
	}

	dir := filepath.Dir(n.plan.Socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("network: creating %s: %w", dir, err)
	}
	// A socket left behind by a crashed run would make bind fail with "address already
	// in use" even though nothing is listening.
	if err := os.Remove(n.plan.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("network: removing a stale link socket: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	if err := n.provider.start(runCtx, n.plan, n.engine, n.logger); err != nil {
		cancel()
		_ = os.RemoveAll(dir)
		return err
	}
	n.started = true
	return nil
}

// Stop shuts the stack down and removes the socket and its directory.
//
// Stop is safe to call without Start, and safe to call twice, so it can sit in a defer
// next to every other piece of sandbox cleanup without a guard.
func (n *Network) Stop() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return nil
	}
	n.stopped = true
	if n.cancel != nil {
		n.cancel()
	}
	var err error
	if n.started {
		err = n.provider.stop()
	}
	// Remove the directory even if the provider complained: leaving a socket behind is
	// how the next run of the same sandbox name fails for a confusing reason.
	if rmErr := os.RemoveAll(filepath.Dir(n.plan.Socket)); rmErr != nil && err == nil {
		err = fmt.Errorf("network: removing the link socket directory: %w", rmErr)
	}
	return err
}

// blackhole binds the link socket and discards everything the VM sends.
//
// ModeNone still attaches a NIC to the VM — that is what turns TSI off — so something has
// to be at the other end of the link or the VM's device has nowhere to write. Nothing is
// wired to the NIC inside the container, so in practice almost nothing arrives; discarding
// what does is both correct and the smallest possible amount of host code.
type blackhole struct {
	conn *net.UnixConn
	done chan struct{}
}

func (b *blackhole) start(ctx context.Context, plan Plan, _ *policy.Engine, _ io.Writer) error {
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: plan.Socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("network: binding the link socket %s: %w", plan.Socket, err)
	}
	b.conn = conn
	b.done = make(chan struct{})

	go func() {
		defer close(b.done)
		buf := make([]byte, plan.MTU+64)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
			// Deliberately dropped. A frame arriving here means the guest emitted
			// something on an interface it was never wired to.
		}
	}()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return nil
}

// listen and dial refuse: ModeNone has a link socket, so the VM's NIC has somewhere to
// write, but there is no network stack behind it and no address to reach. A proxy in a
// sandbox with no network would be a network.
func (b *blackhole) listen(string) (net.Listener, error) {
	return nil, fmt.Errorf("%w: -net none attaches no stack, so there is nothing to listen in", ErrNoNetwork)
}

func (b *blackhole) dial(context.Context, string) (net.Conn, error) {
	return nil, fmt.Errorf("%w: -net none attaches no stack, so there is nothing to dial", ErrNoNetwork)
}

func (b *blackhole) stop() error {
	if b.conn == nil {
		return nil
	}
	err := b.conn.Close()
	<-b.done
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("network: closing the link socket: %w", err)
	}
	return nil
}
