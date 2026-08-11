package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
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

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
}

// provider is the host-side end of the link.
type provider interface {
	// start binds the link socket and begins serving. It must return only once the
	// socket exists, because the VM will connect to it as soon as the task starts.
	start(ctx context.Context, plan Plan, logger io.Writer) error
	stop() error
}

// New builds a Network from a Config. Nothing is started and no socket exists yet.
func New(cfg Config) (*Network, error) {
	plan, err := NewPlan(cfg)
	if err != nil {
		return nil, err
	}
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

// Annotations returns the OCI annotations the sandbox must be created with.
func (n *Network) Annotations() map[string]string { return n.plan.Annotations() }

// SetLogger routes the provider's diagnostics somewhere. Nil discards them.
func (n *Network) SetLogger(w io.Writer) { n.logger = w }

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
	if err := n.provider.start(runCtx, n.plan, n.logger); err != nil {
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

func (b *blackhole) start(ctx context.Context, plan Plan, logger io.Writer) error {
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
