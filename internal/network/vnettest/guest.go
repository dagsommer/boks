// Package vnettest attaches a fake guest to a sandbox's virtual network link, so that the
// datapath can be driven from the guest's side on a machine with no hypervisor.
//
// # What it is, and what it is not
//
// The fake guest is a second gvisor-tap-vsock stack on the far end of the same SOCK_DGRAM
// UNIX socket a VM would use. Everything between the two stacks is real: Ethernet frames
// over the real link, ARP, a real TCP handshake, and a real HTTP client speaking to
// whatever Boks put inside the virtual network. A test using it therefore proves that the
// proxy answers inside the sandbox's network, that an allowed destination succeeds and a
// denied one is refused, and that teardown closes everything.
//
// It proves nothing about the hypervisor. A real guest reaches this link through libkrun's
// virtio-net device and nerdbox's annotations, and neither is exercised here. Read a
// passing test as "the host side of the datapath works", never as "a sandbox is contained".
//
// It lives in a package of its own rather than in a _test.go file because both the network
// package and the packages that build on it need it. Nothing outside tests imports it, so
// it is not part of the boks binary.
package vnettest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// Guest is a fake guest attached to one sandbox link.
type Guest struct {
	vn     *virtualnetwork.VirtualNetwork
	conn   *net.UnixConn
	cancel context.CancelFunc
	socket string
	addr   string
}

// Attach connects a fake guest to the link socket of a started network.
//
// guestIP is the address the fake guest answers on; it must be the address the sandbox's
// container annotation hands the real guest, because the host stack routes to that.
func Attach(socket, guestIP string, mtu int) (*Guest, error) {
	// The guest end binds its own socket, exactly as the VMM does: the host learns where
	// to send replies from the source address of the first datagram it receives.
	guestSocket := filepath.Join(filepath.Dir(socket), "guest.sock")
	_ = os.Remove(guestSocket)

	vn, err := virtualnetwork.New(&types.Configuration{
		MTU:               mtu,
		Subnet:            "192.168.127.0/24",
		GatewayIP:         guestIP,
		GatewayMacAddress: "5a:94:ef:e4:0c:ee",
	})
	if err != nil {
		return nil, fmt.Errorf("vnettest: building the fake guest stack: %w", err)
	}
	conn, err := net.DialUnix("unixgram",
		&net.UnixAddr{Name: guestSocket, Net: "unixgram"},
		&net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("vnettest: connecting to the link socket %s: %w", socket, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	g := &Guest{vn: vn, conn: conn, cancel: cancel, socket: guestSocket, addr: guestIP}
	go func() {
		// Ends when the context is cancelled or either side closes the link.
		_ = vn.AcceptVfkit(ctx, conn)
	}()
	return g, nil
}

// Dial opens a TCP connection from the fake guest.
//
// The first connection also carries the link's ARP exchange, and the host side does not
// bind its return path until it has seen a datagram from us, so early attempts can fail
// while nothing is wrong. Retrying is part of the fixture rather than of every test.
func (g *Guest) Dial(ctx context.Context, addr string) (net.Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	var lastErr error
	for {
		conn, err := g.vn.Dial("tcp", addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("vnettest: dialling %s from the fake guest: %w", addr, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// HTTPClient returns a client whose every request goes through proxyURL, reached from
// inside the virtual network. This is what a cooperating guest does with HTTP_PROXY.
func (g *Guest) HTTPClient(proxyURL string) (*http.Client, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("vnettest: proxy URL %q: %w", proxyURL, err)
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:       http.ProxyURL(u),
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return g.Dial(ctx, addr) },
			// The proxy is inside the virtual network and serves one guest; keeping
			// connections idle across tests only makes teardown noisier.
			DisableKeepAlives: true,
		},
	}, nil
}

// Close detaches the fake guest.
func (g *Guest) Close() error {
	g.cancel()
	err := g.conn.Close()
	_ = os.Remove(g.socket)
	return err
}
