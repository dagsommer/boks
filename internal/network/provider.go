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

// Dial opens a connection from the host side into the sandbox's virtual network.
//
// It is the inverse of Listen, and it exists for two reasons: it is how a test drives the
// datapath without a hypervisor — dial the gateway, speak to whatever Boks put there — and
// it is what a future `boks ports` would forward an inbound connection through. It is
// host→guest only. Nothing here lets the guest reach the host; that direction is decided by
// the policy engine in front of the proxy.
func (n *Network) Dial(ctx context.Context, port int) (net.Conn, error) {
	if err := n.running(); err != nil {
		return nil, err
	}
	return n.provider.dial(ctx, net.JoinHostPort(n.plan.Gateway.String(), strconv.Itoa(port)))
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
	// Refuse early on a platform where nothing can attach a VM to this link, rather than
	// binding a socket and waiting for a peer that cannot exist. This is the only place
	// the platform is asked about: the link is a stream now, so the socket, the framing
	// and the stack are the same code everywhere.
	if err := vmmSupported(); err != nil {
		return err
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
//
// It does not frame what it reads, and it deliberately does not care: there is no stack
// behind this socket, so a length prefix is a byte like any other. That is the one place a
// stream link is simpler than a datagram one.
type blackhole struct {
	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	stopped  bool
	done     chan struct{}
}

func (b *blackhole) start(ctx context.Context, plan Plan, _ *policy.Engine, _ io.Writer) error {
	listener, err := net.Listen("unix", plan.Socket)
	if err != nil {
		return fmt.Errorf("network: binding the link socket %s: %w", plan.Socket, err)
	}
	b.listener = listener
	b.conns = map[net.Conn]struct{}{}
	b.done = make(chan struct{})

	go func() {
		defer close(b.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !b.track(conn) {
				_ = conn.Close()
				continue
			}
			go func() {
				defer b.untrack(conn)
				// Deliberately discarded. A frame arriving here means the
				// guest emitted something on an interface it was never wired
				// to, and reading it is only what keeps the VM's device from
				// blocking on a socket nobody drains.
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	go func() {
		<-ctx.Done()
		_ = b.stop()
	}()
	return nil
}

func (b *blackhole) track(conn net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return false
	}
	b.conns[conn] = struct{}{}
	return true
}

func (b *blackhole) untrack(conn net.Conn) {
	b.mu.Lock()
	delete(b.conns, conn)
	b.mu.Unlock()
	_ = conn.Close()
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

// stop closes the socket and every connection on it. It is called both from Network.Stop and
// from the context watcher, so it has to be safe twice and safe concurrently.
func (b *blackhole) stop() error {
	b.mu.Lock()
	if b.listener == nil || b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	conns := make([]net.Conn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.conns = map[net.Conn]struct{}{}
	b.mu.Unlock()

	err := b.listener.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	<-b.done
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("network: closing the link socket: %w", err)
	}
	return nil
}
