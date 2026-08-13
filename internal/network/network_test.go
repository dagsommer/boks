package network

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/network/vnettest"
)

func testConfig(t *testing.T, mode Mode) Config {
	t.Helper()
	// A short directory: the socket path has to fit in sockaddr_un, and t.TempDir() on
	// macOS is already long enough to make that interesting.
	dir, err := os.MkdirTemp("", "bn")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return Config{Mode: mode, Sandbox: "boks-test", RuntimeDir: dir}
}

func TestPlanAddressing(t *testing.T) {
	plan, err := NewPlan(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if plan.GuestAddr.String() != "192.168.127.2/24" {
		t.Errorf("guest address = %s", plan.GuestAddr)
	}
	if plan.Gateway.String() != "192.168.127.1" {
		t.Errorf("gateway = %s", plan.Gateway)
	}
	if plan.MTU != DefaultMTU {
		t.Errorf("MTU = %d", plan.MTU)
	}
}

// TestPlanMACIsUnicastAndLocal pins the two properties nerdbox's parser enforces. It
// rejects a MAC with the multicast bit set, so generating one would fail at task start
// with an error far from its cause.
func TestPlanMACIsUnicastAndLocal(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		plan, err := NewPlan(testConfig(t, ModeNAT))
		if err != nil {
			t.Fatalf("NewPlan: %v", err)
		}
		mac, err := net.ParseMAC(plan.VMMAC)
		if err != nil {
			t.Fatalf("generated MAC %q does not parse: %v", plan.VMMAC, err)
		}
		if mac[0]&0x01 != 0 {
			t.Fatalf("MAC %s has the multicast bit set; nerdbox rejects it", mac)
		}
		if mac[0]&0x02 == 0 {
			t.Fatalf("MAC %s is not locally administered", mac)
		}
		seen[plan.VMMAC] = true
	}
	if len(seen) < 60 {
		t.Errorf("only %d distinct MACs out of 64; two sandboxes must not collide", len(seen))
	}
}

func TestAnnotationsForNAT(t *testing.T) {
	plan, err := NewPlan(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	a := plan.Annotations()

	vm := a["io.containerd.nerdbox.network.0"]
	for _, want := range []string{"socket=" + plan.Socket, "mode=unixstream", "mac=" + plan.VMMAC} {
		if !strings.Contains(vm, want) {
			t.Errorf("VM annotation %q lacks %q", vm, want)
		}
	}
	// The two flags that would change the framing on the wire. vfkit=true makes libkrun
	// send a magic sequence before the first frame, and vnet_hdr=true puts a virtio-net
	// header in front of every frame; either one turns the stream the host stack reads
	// into something else, so neither may appear by accident.
	for _, never := range []string{"vfkit=", "vnet_hdr=", "features="} {
		if strings.Contains(vm, never) {
			t.Errorf("VM annotation %q sets %s, which changes the link's framing", vm, never)
		}
	}

	ctr, ok := a["io.containerd.nerdbox.ctr.network.0"]
	if !ok {
		t.Fatal("NAT mode did not wire the container to the NIC")
	}
	if !strings.Contains(ctr, "vmmac="+plan.VMMAC) {
		t.Errorf("container annotation %q does not reference the VM MAC", ctr)
	}
	// The parser calls netip.ParsePrefix on addr, whatever nerdbox's documentation
	// shows: a bare IP is rejected.
	if !strings.Contains(ctr, "addr=192.168.127.2/24") {
		t.Errorf("container annotation %q does not carry a CIDR address", ctr)
	}
	if !strings.Contains(ctr, "gw=192.168.127.1") || !strings.Contains(ctr, "ifname=eth0") {
		t.Errorf("container annotation %q is missing the gateway or interface name", ctr)
	}

	// DNS must point at the gateway, not at a copy of the host's resolver.
	if got := a["io.containerd.nerdbox.ctr.dns"]; got != "nameserver=192.168.127.1" {
		t.Errorf("DNS annotation = %q", got)
	}
}

// TestAnnotationsForNoneOmitTheContainerWiring is the whole of ModeNone: the NIC exists on
// the VM (which disables TSI) and the container is never attached to it.
func TestAnnotationsForNoneOmitTheContainerWiring(t *testing.T) {
	plan, err := NewPlan(testConfig(t, ModeNone))
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	a := plan.Annotations()
	if _, ok := a["io.containerd.nerdbox.network.0"]; !ok {
		t.Error("no VM NIC: without it the runtime falls back to TSI, which reaches host loopback")
	}
	for _, key := range []string{"io.containerd.nerdbox.ctr.network.0", "io.containerd.nerdbox.ctr.dns"} {
		if v, ok := a[key]; ok {
			t.Errorf("mode none emitted %s=%q; the container must not be wired to the NIC", key, v)
		}
	}
}

// TestAnnotationsParseTheWayNerdboxParsesThem re-implements the shim's validation rules
// from its source, so a change here that nerdbox would reject fails in our tests instead of
// at VM boot.
func TestAnnotationsParseTheWayNerdboxParsesThem(t *testing.T) {
	plan, err := NewPlan(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	for key, value := range plan.Annotations() {
		if key == "io.containerd.nerdbox.ctr.dns" {
			continue
		}
		for _, field := range strings.Split(value, ",") {
			k, v, found := strings.Cut(field, "=")
			if !found {
				t.Errorf("%s: field %q is not key=value", key, field)
				continue
			}
			switch k {
			case "mac", "vmmac":
				mac, err := net.ParseMAC(v)
				if err != nil {
					t.Errorf("%s: %s=%q does not parse: %v", key, k, v, err)
				} else if mac[0]&0x01 != 0 {
					t.Errorf("%s: %s=%q is multicast", key, k, v)
				}
			case "addr":
				if _, err := netip.ParsePrefix(v); err != nil {
					t.Errorf("%s: addr=%q is not CIDR: %v", key, v, err)
				}
			case "gw":
				if _, err := netip.ParseAddr(v); err != nil {
					t.Errorf("%s: gw=%q does not parse: %v", key, v, err)
				}
			case "ifname":
				if len(v) >= 16 {
					t.Errorf("%s: ifname=%q is at or over IFNAMSIZ", key, v)
				}
			case "mode":
				if v != "unixgram" && v != "unixstream" {
					t.Errorf("%s: mode=%q is not a mode nerdbox accepts", key, v)
				}
			case "socket":
				if v == "" {
					t.Errorf("%s: empty socket path", key)
				}
			default:
				t.Errorf("%s: unknown field %q; nerdbox rejects unknown fields", key, k)
			}
		}
	}
}

// TestNoIPv6IsHandedToTheGuest records a decision that has to be deliberate.
//
// TSI had no IPv6 at all. A real NIC does: a guest brings up link-local v6 by itself, and a
// spike saw exactly that (MLD reports on the wire). Boks assigns no routable v6 address and
// no v6 gateway, so the guest's v6 is confined to the link and the host-side stack — which
// is IPv4-only — drops it. If a v6 address is ever handed out here, the policy layer's v6
// rules and the proxy both have to be revisited in the same change.
func TestNoIPv6IsHandedToTheGuest(t *testing.T) {
	plan, err := NewPlan(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if plan.GuestAddr.Addr().Is6() && !plan.GuestAddr.Addr().Is4In6() {
		t.Errorf("guest address %s is IPv6", plan.GuestAddr)
	}
	if plan.Gateway.Is6() && !plan.Gateway.Is4In6() {
		t.Errorf("gateway %s is IPv6", plan.Gateway)
	}
	for key, value := range plan.Annotations() {
		for _, field := range strings.Split(value, ",") {
			k, v, _ := strings.Cut(field, "=")
			if k != "addr" && k != "gw" && k != "nameserver" {
				continue
			}
			if strings.Contains(v, ":") {
				t.Errorf("%s carries an IPv6 value %q; nothing routes it and no rule was written for it", key, v)
			}
		}
	}
}

func TestPlanRejectsBadConfigs(t *testing.T) {
	base := testConfig(t, ModeNAT)

	tests := []struct {
		name  string
		mutum func(*Config)
	}{
		{"no sandbox name", func(c *Config) { c.Sandbox = "" }},
		{"no runtime dir", func(c *Config) { c.RuntimeDir = "" }},
		{"unknown mode", func(c *Config) { c.Mode = "bridge" }},
		{"bad subnet", func(c *Config) { c.Subnet = "192.168.127.0" }},
		{"gateway outside subnet", func(c *Config) { c.GatewayIP = "10.0.0.1" }},
		{"guest outside subnet", func(c *Config) { c.GuestIP = "10.0.0.2" }},
		{"guest equals gateway", func(c *Config) { c.GuestIP = DefaultGatewayIP }},
		{"runtime dir too long", func(c *Config) { c.RuntimeDir = "/" + strings.Repeat("x", 120) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutum(&cfg)
			if plan, err := NewPlan(cfg); err == nil {
				t.Errorf("NewPlan accepted it: %+v", plan)
			}
		})
	}
}

// TestGatewayConfigIsClosed is the assertion the coordinator's spike could not make: the
// host being unreachable was observed, not guaranteed. These four fields are the ways
// gvisor-tap-vsock can be told to expose the host, and all four must be off.
func TestGatewayConfigIsClosed(t *testing.T) {
	plan, err := NewPlan(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	cfg := gatewayConfig(plan)

	if len(cfg.NAT) != 0 {
		t.Errorf("NAT = %v; any entry here maps an address in the guest's view onto the host", cfg.NAT)
	}
	if len(cfg.Forwards) != 0 {
		t.Errorf("Forwards = %v; port forwarding into the guest must be explicit, never default", cfg.Forwards)
	}
	if len(cfg.GatewayVirtualIPs) != 0 {
		t.Errorf("GatewayVirtualIPs = %v; extra gateway addresses widen what the guest can address", cfg.GatewayVirtualIPs)
	}
	if cfg.Ec2MetadataAccess {
		t.Error("Ec2MetadataAccess is on; that is a proxy to 169.254.169.254, the best credential source on a hosted machine")
	}
	if cfg.CaptureFile != "" {
		t.Errorf("CaptureFile = %q; capturing guest traffic to disk must be opt-in", cfg.CaptureFile)
	}
	if cfg.Subnet != plan.Subnet.String() || cfg.GatewayIP != plan.Gateway.String() {
		t.Errorf("the stack's addressing (%s, %s) disagrees with the container's", cfg.Subnet, cfg.GatewayIP)
	}
}

// TestStartStopLifecycle covers what a crashed or interrupted run leaves behind: nothing.
func TestStartStopLifecycle(t *testing.T) {
	for _, mode := range Modes() {
		t.Run(string(mode), func(t *testing.T) {
			n, err := New(testConfig(t, mode))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			socket := n.Plan().Socket

			if err := n.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := os.Stat(socket); err != nil {
				t.Fatalf("the link socket does not exist after Start: %v", err)
			}
			// The VM connects to this socket during boot; it must be accepting
			// immediately, not after a race.
			conn, err := net.Dial("unix", socket)
			if err != nil {
				t.Fatalf("dialling the link socket: %v", err)
			}
			if _, err := conn.Write(framed(broadcastFrame())); err != nil {
				t.Errorf("writing to the link socket: %v", err)
			}
			conn.Close()

			if err := n.Start(context.Background()); err == nil {
				t.Error("Start twice was accepted; one stack serves exactly one VM")
			}

			if err := n.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if _, err := os.Stat(filepath.Dir(socket)); !os.IsNotExist(err) {
				t.Errorf("the socket directory survived Stop: %v", err)
			}
			if err := n.Stop(); err != nil {
				t.Errorf("Stop twice returned %v; cleanup must be idempotent", err)
			}
		})
	}
}

// TestContextCancellationClosesTheSocket: SIGINT cancels the context, and nothing must be
// left holding the socket afterwards.
//
// A stream link makes this sharper than the datagram one could: a connect either reaches
// something that is accepting or it does not, where an unconnected datagram send could
// succeed against a socket nobody was reading.
func TestContextCancellationClosesTheSocket(t *testing.T) {
	n, err := New(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	socket := n.Plan().Socket
	if conn, err := net.Dial("unix", socket); err != nil {
		t.Fatalf("the link socket is not accepting before cancellation: %v", err)
	} else {
		conn.Close()
	}
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			break // the socket is gone, which is the point
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the link socket still accepts connections after the context was cancelled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := n.Stop(); err != nil {
		t.Errorf("Stop after cancellation: %v", err)
	}
}

// echoInside puts an echo server inside the sandbox's virtual network, at the address the
// proxy would listen on. It is the far end for the round trips below: a guest that reaches it
// has a working link in both directions, which is the only thing these tests are about.
func echoInside(t *testing.T, n *Network, port int) {
	t.Helper()
	listener, err := n.Listen(port)
	if err != nil {
		t.Fatalf("listening inside the virtual network: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
}

// roundTrip sends a line through the link and reads it back, which fails unless frames are
// crossing the link intact in both directions.
func roundTrip(t *testing.T, guest *vnettest.Guest, addr, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := guest.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("the guest could not reach %s: %v", addr, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(message)); err != nil {
		t.Fatalf("writing through the link: %v", err)
	}
	buf := make([]byte, len(message))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading through the link: %v", err)
	}
	if string(buf) != message {
		t.Fatalf("the link returned %q, want %q", buf, message)
	}
}

// linkHeld reports whether a connection currently holds the sandbox's link. It reaches into
// the gateway because the accept loop's state has no reason to be public: nothing in Boks
// asks, and only a test that is about reconnection needs to know.
func linkHeld(t *testing.T, n *Network) bool {
	t.Helper()
	g, ok := n.provider.(*gateway)
	if !ok {
		t.Fatalf("provider is %T, not a gateway", n.provider)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conn != nil
}

// TestTheLinkTakesAVMThatConnectsLateAndOneThatReconnects covers what the stream transport
// changed about attaching.
//
// On the datagram socket there was nothing to attach: the host bound the socket, and the
// first datagram to arrive both identified the peer and was its first frame. A stream has a
// connection, and two states come with it. The VM connects *late* — it is still booting when
// Start returns — so the link has to sit and wait rather than fail. And a VMM that restarts
// reconnects; if the host only ever served one connection, a restart would leave the sandbox
// with a socket nobody is reading for the rest of its life.
func TestTheLinkTakesAVMThatConnectsLateAndOneThatReconnects(t *testing.T) {
	n, err := New(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer n.Stop()

	const port = 9099
	echoInside(t, n, port)
	addr := net.JoinHostPort(n.Plan().Gateway.String(), strconv.Itoa(port))

	// Nothing has connected yet, and the stack has been up for a while: exactly the
	// position a VM's boot leaves it in.
	if linkHeld(t, n) {
		t.Fatal("something is holding the link before any peer connected")
	}
	time.Sleep(50 * time.Millisecond)

	for attempt := 1; attempt <= 2; attempt++ {
		guest, err := vnettest.Attach(vnettest.Config{
			Socket:    n.Plan().Socket,
			GuestIP:   n.Plan().GuestAddr.Addr().String(),
			GatewayIP: n.Plan().Gateway.String(),
			Subnet:    n.Plan().Subnet.String(),
			MTU:       n.Plan().MTU,
		})
		if err != nil {
			t.Fatalf("attempt %d: attaching a guest: %v", attempt, err)
		}
		roundTrip(t, guest, addr, "attempt")
		guest.Close()

		// The host has to notice the peer is gone and go back to accepting, or the
		// next connection is refused as a second VM.
		deadline := time.Now().Add(5 * time.Second)
		for linkHeld(t, n) {
			if time.Now().After(deadline) {
				t.Fatalf("attempt %d: the link was still held five seconds after the peer disconnected",
					attempt)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestASecondPeerIsRefusedWhileOneHoldsTheLink is the property a listening socket needs that
// a datagram socket got for free.
//
// One stack serves exactly one VM: it hands out one address, and the switch it sits behind is
// an Ethernet fabric. A second connection accepted alongside the first would be a second,
// unaccounted device on that fabric — able to inject frames the sandbox's own guest would
// receive, and to receive the broadcasts meant for it. The link socket lives in a mode-0700
// directory, so this is not a boundary between users; it is the difference between a
// sandbox's network having one occupant and having any number.
func TestASecondPeerIsRefusedWhileOneHoldsTheLink(t *testing.T) {
	n, err := New(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log := &syncBuffer{}
	n.SetLogger(log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer n.Stop()

	const port = 9098
	echoInside(t, n, port)
	addr := net.JoinHostPort(n.Plan().Gateway.String(), strconv.Itoa(port))

	guest, err := vnettest.Attach(vnettest.Config{
		Socket:    n.Plan().Socket,
		GuestIP:   n.Plan().GuestAddr.Addr().String(),
		GatewayIP: n.Plan().Gateway.String(),
		Subnet:    n.Plan().Subnet.String(),
		MTU:       n.Plan().MTU,
	})
	if err != nil {
		t.Fatalf("attaching the guest: %v", err)
	}
	defer guest.Close()
	roundTrip(t, guest, addr, "the first peer")

	intruder, err := net.Dial("unix", n.Plan().Socket)
	if err != nil {
		t.Fatalf("dialling the link socket a second time: %v", err)
	}
	defer intruder.Close()
	if err := intruder.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := intruder.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the second connection read %v, want EOF: it must be closed, not served", err)
	}

	// And the peer that had the link still has it.
	roundTrip(t, guest, addr, "still the first peer")

	// Whoever can reach the socket chooses how often it connects, and the stack log is a
	// file: a line per refusal would make a connect loop a disk-usage primitive. The cap is
	// the same idea as the one on dropped destinations in stack.go.
	for i := 0; i < 4*maxLinkNotices; i++ {
		conn, err := net.Dial("unix", n.Plan().Socket)
		if err != nil {
			t.Fatalf("dialling the link socket: %v", err)
		}
		conn.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "further messages about this link's connections are suppressed") {
		if time.Now().After(deadline) {
			t.Fatalf("%d refusals never reached the cap:\n%s", 4*maxLinkNotices, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Count(log.String(), "refused a second connection"); got > maxLinkNotices {
		t.Errorf("%d refusal lines were written for %d connections; the cap is %d",
			got, 4*maxLinkNotices, maxLinkNotices)
	}
}

// syncBuffer collects the stack's operational log from the goroutines that write it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStaleSocketIsReplaced(t *testing.T) {
	cfg := testConfig(t, ModeNone)
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	socket := n.Plan().Socket
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Simulate a crashed run: the socket file is there, nothing is listening.
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := n.Start(context.Background()); err != nil {
		t.Fatalf("Start over a stale socket: %v", err)
	}
	if err := n.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestParseMode(t *testing.T) {
	for _, in := range []string{"none", "NONE", " none "} {
		if m, err := ParseMode(in); err != nil || m != ModeNone {
			t.Errorf("ParseMode(%q) = %v, %v", in, m, err)
		}
	}
	if m, err := ParseMode(""); err != nil || m != DefaultMode {
		t.Errorf("ParseMode(\"\") = %v, %v", m, err)
	}
	if _, err := ParseMode("host"); err == nil {
		t.Error("ParseMode accepted an unknown mode")
	}
}
