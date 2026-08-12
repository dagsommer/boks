//go:build !windows

package network

import (
	"context"
	"io"
	"net/netip"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// ipv4Frame builds the packet the switch hands the link endpoint: an IPv4 packet with the
// Ethernet header already stripped, exactly as tap.Switch delivers it.
func ipv4Frame(t *testing.T, proto tcpip.TransportProtocolNumber, dst string, dstPort uint16) *stack.PacketBuffer {
	t.Helper()
	const transportSize = header.TCPMinimumSize // the larger of the two headers
	buf := make([]byte, header.IPv4MinimumSize+transportSize)

	switch proto {
	case header.UDPProtocolNumber:
		header.UDP(buf[header.IPv4MinimumSize:]).Encode(&header.UDPFields{
			SrcPort: 40000,
			DstPort: dstPort,
			Length:  header.UDPMinimumSize,
		})
	case header.TCPProtocolNumber:
		header.TCP(buf[header.IPv4MinimumSize:]).Encode(&header.TCPFields{
			SrcPort:    40000,
			DstPort:    dstPort,
			DataOffset: header.TCPMinimumSize,
			Flags:      header.TCPFlagSyn,
		})
	}

	addr, err := netip.ParseAddr(dst)
	if err != nil {
		t.Fatalf("bad destination %q: %v", dst, err)
	}
	ip := header.IPv4(buf)
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(buf)),
		TTL:         64,
		Protocol:    uint8(proto),
		SrcAddr:     tcpip.AddrFrom4([4]byte{192, 168, 127, 2}),
		DstAddr:     tcpip.AddrFrom4(addr.As4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	return stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(buf)})
}

// TestTheLinkCarriesOnlyWhatCanBeJudged pins the protocol whitelist.
//
// Everything the guest emits arrives at this filter first, and what it lets past decides what
// the stack can be asked to do. TCP is carried because the forwarder judges it; ARP because a
// guest that cannot find the gateway has no network; UDP only to the gateway's own services.
// ICMP, IPv6 and every other IP protocol are dropped: none of them carries a connection to
// judge, and DNS to a server of the guest's choosing is a channel around every hostname rule.
func TestTheLinkCarriesOnlyWhatCanBeJudged(t *testing.T) {
	link := &filteredLink{gateway: netip.MustParseAddr(DefaultGatewayIP)}

	tests := []struct {
		name  string
		proto tcpip.NetworkProtocolNumber
		pkt   *stack.PacketBuffer
		drop  bool
	}{
		{"arp, so the guest can find the gateway", header.ARPProtocolNumber, nil, false},
		{"tcp to the internet, judged by the forwarder", header.IPv4ProtocolNumber,
			ipv4Frame(t, header.TCPProtocolNumber, "203.0.113.7", 443), false},
		{"dns to the gateway's own resolver", header.IPv4ProtocolNumber,
			ipv4Frame(t, header.UDPProtocolNumber, DefaultGatewayIP, 53), false},
		{"dhcp, broadcast before the guest has an address", header.IPv4ProtocolNumber,
			ipv4Frame(t, header.UDPProtocolNumber, "255.255.255.255", 67), false},
		{"dns to a resolver of the guest's choosing", header.IPv4ProtocolNumber,
			ipv4Frame(t, header.UDPProtocolNumber, "8.8.8.8", 53), true},
		{"quic, or any other udp", header.IPv4ProtocolNumber,
			ipv4Frame(t, header.UDPProtocolNumber, "203.0.113.7", 443), true},
		{"udp to the gateway on another port", header.IPv4ProtocolNumber,
			ipv4Frame(t, header.UDPProtocolNumber, DefaultGatewayIP, 4711), true},
		{"icmp", header.IPv4ProtocolNumber,
			ipv4Frame(t, header.ICMPv4ProtocolNumber, "203.0.113.7", 0), true},
		{"ipv6, which nothing here routes", header.IPv6ProtocolNumber, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pkt := tc.pkt
			if pkt == nil {
				pkt = stack.NewPacketBuffer(stack.PacketBufferOptions{})
			}
			defer pkt.DecRef()
			why, drop := link.classify(tc.proto, pkt)
			if drop != tc.drop {
				t.Fatalf("drop = %v, want %v (%s)", drop, tc.drop, why)
			}
			if drop && why == "" {
				t.Error("a dropped frame must say what it was, or nobody can debug the silence")
			}
		})
	}
}

// TestATruncatedPacketIsDropped: the guest writes these bytes, so a header that is not there
// must not be read as though it were.
func TestATruncatedPacketIsDropped(t *testing.T) {
	link := &filteredLink{gateway: netip.MustParseAddr(DefaultGatewayIP)}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(make([]byte, header.IPv4MinimumSize-1)),
	})
	defer pkt.DecRef()
	if _, drop := link.classify(header.IPv4ProtocolNumber, pkt); !drop {
		t.Error("a truncated IPv4 packet was carried")
	}
}

// TestAStackWithNoPolicyRefusesEverything is the fail-closed property. A stack whose judge is
// missing is a misconfiguration, and the safe reading of a missing rule set is that nothing
// is permitted — never that everything is.
func TestAStackWithNoPolicyRefusesEverything(t *testing.T) {
	h := &hostStack{conns: map[io.Closer]struct{}{}, noticed: map[string]struct{}{}}
	target, err := policy.NewTarget("203.0.113.7", 443)
	if err != nil {
		t.Fatal(err)
	}
	if h.judge(target) {
		t.Error("a stack with no policy engine permitted a destination")
	}
}

// TestEveryRawDecisionIsLoggedAsTransparent covers the gap the finding was about: the flows
// that mattered — the ones that ignored the proxy — were the ones the decision log could not
// show. Both outcomes have to be recorded, and both have to say they were judged on an
// address rather than read.
func TestEveryRawDecisionIsLoggedAsTransparent(t *testing.T) {
	res, err := (policy.Request{Preset: policy.PresetLocked, Allow: []string{"203.0.113.8:443"}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	log := policy.NewLog(16)
	h := &hostStack{
		engine:  policy.NewEngine(pol, log).WithSandbox("boks-test"),
		conns:   map[io.Closer]struct{}{},
		noticed: map[string]struct{}{},
	}

	allowed, _ := policy.NewTarget("203.0.113.8", 443)
	denied, _ := policy.NewTarget("203.0.113.7", 443)
	if !h.judge(allowed) {
		t.Error("an allowed address was refused")
	}
	if h.judge(denied) {
		t.Error("a denied address was permitted")
	}

	decisions := log.Recent(0)
	if len(decisions) != 2 {
		t.Fatalf("%d decisions recorded, want 2: %+v", len(decisions), decisions)
	}
	for _, d := range decisions {
		if d.Mode != policy.ModeTransparent {
			t.Errorf("mode = %q, want transparent: this flow never used the proxy", d.Mode)
		}
		if d.Stage != policy.StageNetwork {
			t.Errorf("stage = %q, want network", d.Stage)
		}
		if !strings.HasPrefix(d.Resource, "net:ip:") {
			t.Errorf("resource = %q; a raw flow carries no hostname", d.Resource)
		}
		if d.Sandbox != "boks-test" {
			t.Errorf("decision %+v is not attributed to a sandbox", d)
		}
	}
	if decisions[0].Allowed == decisions[1].Allowed {
		t.Errorf("both decisions have the same outcome: %+v", decisions)
	}
}

// TestTheStackStopsCleanly: stop is called from a defer next to every other piece of sandbox
// cleanup, so it has to be safe twice and safe on a stack nothing ever attached to.
func TestTheStackStopsCleanly(t *testing.T) {
	plan, err := NewPlan(testConfig(t, ModeNAT))
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	h, err := newHostStack(context.Background(), plan, nil, nil)
	if err != nil {
		t.Fatalf("newHostStack: %v", err)
	}
	h.stop()
	h.stop()
}
