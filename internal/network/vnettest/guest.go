// Package vnettest attaches a fake guest to a sandbox's virtual network link, so that the
// datapath can be driven from the guest's side on a machine with no hypervisor.
//
// # What it is, and what it is not
//
// The fake guest is a second gvisor stack on the far end of the same SOCK_DGRAM UNIX socket
// a VM would use, configured the way the container is configured by Boks' annotations: one
// address in the sandbox's subnet, and a default route through the gateway. Everything
// between the two stacks is real: Ethernet frames over the real link, ARP, a real TCP
// handshake, and real traffic to whatever the connection reaches.
//
// That makes two different things testable, and they are worth separating:
//
//   - what a **cooperating** guest does — HTTPClient sends every request through the proxy
//     inside the virtual network, exactly as a guest honouring HTTP_PROXY would;
//   - what an **uncooperating** guest does — Dial opens a TCP connection straight at a
//     destination, with no proxy anywhere in the path. That is the case that used to walk
//     past the policy entirely, and it is the one the host stack now judges.
//
// It proves nothing about the hypervisor. A real guest reaches this link through libkrun's
// virtio-net device and nerdbox's annotations, and neither is exercised here. Read a passing
// test as "the host side of the datapath works", never as "a sandbox is contained".
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
	"strconv"
	"strings"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// Config describes the guest to attach. It mirrors what Boks' container annotations give a
// real guest, and the values must be the same ones: the host stack routes to the address it
// handed out, and answers ARP for the gateway it said it was.
type Config struct {
	// Socket is the link socket the sandbox's stack is holding.
	Socket string
	// GuestIP is the address the container annotation gives the guest.
	GuestIP string
	// GatewayIP is the host-side stack's address, and the guest's default route.
	GatewayIP string
	// Subnet is the virtual network, as CIDR.
	Subnet string
	// MAC is the guest's link address. It must differ from the gateway's, which Boks
	// generates randomly per sandbox, so the default is safe.
	MAC string
	MTU int
}

// Guest is a fake guest attached to one sandbox link.
type Guest struct {
	stack  *stack.Stack
	sw     *tap.Switch
	conn   *net.UnixConn
	cancel context.CancelFunc
	socket string
}

// defaultMAC is a locally administered unicast address, as the guest's would be.
const defaultMAC = "5a:94:ef:e4:0c:ee"

// Attach connects a fake guest to the link socket of a started network.
func Attach(cfg Config) (*Guest, error) {
	if cfg.MAC == "" {
		cfg.MAC = defaultMAC
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1500
	}
	guestIP := net.ParseIP(cfg.GuestIP).To4()
	gatewayIP := net.ParseIP(cfg.GatewayIP).To4()
	if guestIP == nil || gatewayIP == nil {
		return nil, fmt.Errorf("vnettest: guest %q and gateway %q must both be IPv4", cfg.GuestIP, cfg.GatewayIP)
	}
	_, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("vnettest: subnet %q: %w", cfg.Subnet, err)
	}
	ones, _ := subnet.Mask.Size()

	// The guest end binds its own socket, exactly as the VMM does: the host learns where
	// to send replies from the source address of the first datagram it receives.
	guestSocket := filepath.Join(filepath.Dir(cfg.Socket), "guest.sock")
	_ = os.Remove(guestSocket)

	link, err := tap.NewLinkEndpoint(false, uint32(cfg.MTU), cfg.MAC, cfg.GuestIP, nil)
	if err != nil {
		return nil, fmt.Errorf("vnettest: building the guest link: %w", err)
	}
	sw := tap.NewSwitch(false)
	link.Connect(sw)
	sw.Connect(link)

	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		// A guest kernel speaks all three. UDP and ICMP are here so that a test can
		// send them and observe that the host stack drops them, which is a property
		// worth being able to check rather than assume.
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	if err := s.CreateNIC(1, link); err != nil {
		s.Close()
		return nil, fmt.Errorf("vnettest: creating the guest NIC: %s", err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFrom4Slice(guestIP),
			PrefixLen: ones,
		},
	}, stack.AddressProperties{}); err != nil {
		s.Close()
		return nil, fmt.Errorf("vnettest: assigning the guest address: %s", err)
	}

	onLink, err := tcpip.NewSubnet(tcpip.AddrFromSlice(subnet.IP), tcpip.MaskFromBytes(subnet.Mask))
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("vnettest: building the guest route table: %w", err)
	}
	// The default route is what makes this a guest rather than a peer: a connection to any
	// address outside the subnet is sent to the gateway's MAC, which is precisely how a
	// real guest's raw socket reaches — or fails to reach — the outside world.
	s.SetRouteTable([]tcpip.Route{
		{Destination: onLink, NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4Slice(gatewayIP), NIC: 1},
	})

	conn, err := net.DialUnix("unixgram",
		&net.UnixAddr{Name: guestSocket, Net: "unixgram"},
		&net.UnixAddr{Name: cfg.Socket, Net: "unixgram"})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("vnettest: connecting to the link socket %s: %w", cfg.Socket, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	g := &Guest{stack: s, sw: sw, conn: conn, cancel: cancel, socket: guestSocket}
	go func() {
		// Ends when the context is cancelled or either side closes the link.
		_ = sw.Accept(ctx, conn, types.VfkitProtocol)
	}()
	return g, nil
}

// Dial opens a TCP connection from the fake guest, with no proxy involved at all.
//
// The first connection also carries the link's ARP exchange, and the host side does not bind
// its return path until it has seen a datagram from us, so early attempts can fail while
// nothing is wrong. Retrying is part of the fixture rather than of every test.
//
// A refusal is *not* retried. When the host stack denies a destination it answers with a
// RST, and that is a final answer about a policy, not a symptom of a link that is still
// coming up — retrying it would turn a fast, exact refusal into a timeout.
func (g *Guest) Dial(ctx context.Context, addr string) (net.Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	var lastErr error
	for {
		conn, err := g.dialOnce(addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if Refused(err) || time.Now().After(deadline) {
			return nil, fmt.Errorf("vnettest: dialling %s from the fake guest: %w", addr, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (g *Guest) dialOnce(addr string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		// A guest with a raw socket has already resolved its destination; this fixture
		// deliberately has no resolver, so that no test can accidentally depend on the
		// machine's DNS.
		return nil, fmt.Errorf("vnettest: %q is not an IPv4 address", host)
	}
	return gonet.DialTCP(g.stack, tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4Slice(ip.To4()),
		Port: uint16(port),
	}, ipv4.ProtocolNumber)
}

// Refused reports whether a dial failed because the far end refused it — which is what a
// policy denial looks like from inside the guest, and what distinguishes it from a
// destination that simply never answered.
//
// The substring is "refused" rather than either full phrase because the two stacks spell it
// differently: the host's C library says "connection refused" and gvisor says "connection
// was refused". Matching the shorter form keeps the fixture honest about which one it is
// talking to, which is neither.
func Refused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "refused")
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

// RawHTTPClient returns a client with **no proxy configured at all**: every request is a
// direct TCP connection from the guest to the destination address. It is the fixture's
// version of a guest that unsets HTTP_PROXY and opens its own socket.
func (g *Guest) RawHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       func(ctx context.Context, _, addr string) (net.Conn, error) { return g.Dial(ctx, addr) },
			DisableKeepAlives: true,
		},
	}
}

// Close detaches the fake guest.
func (g *Guest) Close() error {
	g.cancel()
	err := g.conn.Close()
	g.stack.Close()
	_ = os.Remove(g.socket)
	return err
}
