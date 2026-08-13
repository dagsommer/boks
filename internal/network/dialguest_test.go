//go:build !windows

package network

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/network/vnettest"
	"github.com/dagsommer/boks/internal/policy"
)

// TestDialGuestReachesAServiceInsideTheSandbox is the half of port publishing that lives in
// this package: the host side of the link opening a connection *to the container's address*,
// across the real link socket, and reaching a listener the guest bound.
//
// It is the datapath `boks ports` splices onto a host listener. Driving it here, with a
// second gvisor stack on the far end of the real socket, is as close as a machine with no
// hypervisor can get: everything between the two stacks — ARP, the TCP handshake, the data —
// is real, and only the VMM is missing.
func TestDialGuestReachesAServiceInsideTheSandbox(t *testing.T) {
	n, guest := startWithGuest(t)

	ln, err := guest.Listen(3000)
	if err != nil {
		t.Fatalf("listening inside the fake guest: %v", err)
	}
	defer ln.Close()
	go echoLine(ln)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The host end of a SOCK_DGRAM link learns its peer from the first datagram it
	// receives, so nothing the host initiates can be delivered until the guest has spoken
	// once. A real VM does that while it brings its interface up; a fake one is asked.
	if err := guest.Announce(ctx); err != nil {
		t.Fatalf("announcing the fake guest: %v", err)
	}

	conn, err := dialGuestWithRetry(ctx, n, 3000)
	if err != nil {
		t.Fatalf("dialling the guest's service: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "hello from the host\n"); err != nil {
		t.Fatalf("writing to the guest: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the guest's reply: %v", err)
	}
	if got != "hello from the host\n" {
		t.Errorf("guest replied %q", got)
	}
}

// TestDialGuestFailsWhenNothingListensOnTheExternalInterface pins the observable half of the
// constraint sbx documents and users hit first: only the guest's *external* interface is
// addressable across the link, so a port with nothing bound there fails to connect rather
// than half-working. A service bound to the VM's own 127.0.0.1 presents exactly as this does
// — the guest's loopback is a separate stack that no frame on this link ever reaches.
//
// The failure is what the forwarder turns into the advice to bind 0.0.0.0 in the guest.
func TestDialGuestFailsWhenNothingListensOnTheExternalInterface(t *testing.T) {
	n, guest := startWithGuest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := guest.Announce(ctx); err != nil {
		t.Fatalf("announcing the fake guest: %v", err)
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, 3*time.Second)
	defer cancelDial()
	conn, err := n.DialGuest(dialCtx, 3001)
	if err == nil {
		conn.Close()
		t.Fatal("a port nothing in the guest is listening on accepted a connection")
	}
}

func startWithGuest(t *testing.T) (*Network, *vnettest.Guest) {
	t.Helper()
	n, err := New(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, err := policy.Preset(policy.PresetOpen)
	if err != nil {
		t.Fatalf("Preset: %v", err)
	}
	n.SetPolicy(policy.NewEngine(p, policy.NewLog(16)))
	if err := n.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop() })

	plan := n.Plan()
	guest, err := vnettest.Attach(vnettest.Config{
		Socket:    plan.Socket,
		GuestIP:   plan.GuestAddr.Addr().String(),
		GatewayIP: plan.Gateway.String(),
		Subnet:    plan.Subnet.String(),
		MTU:       plan.MTU,
	})
	if err != nil {
		t.Fatalf("attaching the fake guest: %v", err)
	}
	t.Cleanup(func() { _ = guest.Close() })
	return n, guest
}

// dialGuestWithRetry covers the link coming up. The host side does not know where to send
// frames until the guest's first datagram has arrived, so the first attempt can fail while
// nothing is wrong — the same retry the fixture's own Dial does in the other direction.
func dialGuestWithRetry(ctx context.Context, n *Network, port int) (net.Conn, error) {
	var last error
	for {
		conn, err := n.DialGuest(ctx, port)
		if err == nil {
			return conn, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func echoLine(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, line)
		}()
	}
}
