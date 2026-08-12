//go:build !windows

package network

// The host-side network stack, assembled here rather than taken whole from
// gvisor-tap-vsock's virtualnetwork.New.
//
// # Why Boks builds the stack itself
//
// virtualnetwork.New installs gvisor-tap-vsock's own TCP forwarder, and that forwarder's
// handler is, in full:
//
//	outbound, err := net.Dial("tcp", net.JoinHostPort(localAddress.String(), fmt.Sprint(r.ID().LocalPort)))
//
// A bare dial to whatever address the guest put in the SYN, with no policy consulted and no
// hook in the public API to add one. A guest that ignored HTTP_PROXY therefore reached the
// internet unfiltered and unlogged — measured, not theorised: a guest under `-policy locked`
// completed a TLS handshake to a denied address and `boks policy log` showed nothing,
// because the packets never reached the policy engine at all.
//
// Everything virtualnetwork.New does is exported, so this file does the same assembly and
// installs a forwarder that asks the policy engine first. The link layer, the switch and the
// gvisor stack are all still the library's; only the handler is ours. No fork, no vendored
// patch.
//
// # What is enforced here, and how
//
//   - **TCP**: every connection is judged before it is dialled, from the address and port in
//     the SYN. Denied connections are refused with a RST, so the guest sees "connection
//     refused" rather than a hang. Every decision — allow and deny alike — is recorded with
//     mode "transparent", which is what makes `boks policy log` show the flows that did not
//     use the proxy.
//   - **UDP**: dropped at the link, except to the gateway's own resolver and DHCP server.
//     There is deliberately no UDP forwarder: forwarding UDP would hand the guest a data
//     path that carries no connection to judge and, through DNS to a server of its choosing,
//     a covert channel around any hostname rule.
//   - **ICMP**: dropped at the link. Nothing is forwarded and nothing is answered, so a ping
//     from the guest times out rather than being satisfied by a reply the host stack made up.
//   - **Anything else** — IPv6, other IP protocols — is dropped. ARP is kept, because the
//     guest has to be able to find the gateway.
//
// The policy engine is the only thing that decides. If a stack is started without one, it
// denies everything: a network whose judge is missing must not be an open network.
//
// # What this is not
//
// It is not a demonstration that a real guest is contained. The datapath here is driven in
// tests by a second gvisor stack on the far end of the real link socket
// (internal/network/vnettest), which proves that the *stack* refuses a flow the policy
// denies. A real VM reaches this stack through libkrun's virtio-net device, which no machine
// in this project has been able to run. See docs/verification.md.

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/dagsommer/boks/internal/policy"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// nicID is the only NIC. One stack serves one sandbox, whose one link is this NIC.
	nicID = 1

	// dialTimeout bounds an outbound connection attempt. Without one, a guest could park
	// connections to a black hole and hold a forwarder slot until the TCP stack gave up on
	// its own schedule.
	dialTimeout = 30 * time.Second

	// maxInFlight is how many unanswered SYNs the forwarder will work on at once. Each one
	// is a goroutine holding an in-progress dial, so this is the guest's budget for making
	// the host open sockets. gvisor-tap-vsock uses 10; a sandbox running a package install
	// opens more than that in a burst, and 64 is still a bounded, small number.
	maxInFlight = 64

	dnsPort  = 53
	dhcpPort = 67
)

// hostStack is the userspace network stack that terminates one sandbox's virtual NIC.
type hostStack struct {
	plan   Plan
	engine *policy.Engine
	logger io.Writer
	ctx    context.Context
	// metadataAccess mirrors the configuration's Ec2MetadataAccess. It is false, and the
	// field exists so that the forwarder enforces the same closed posture the
	// configuration declares rather than the two drifting apart.
	metadataAccess bool

	stack  *stack.Stack
	sw     *tap.Switch
	dnsUDP *gonet.UDPConn
	dnsTCP *gonet.TCPListener

	mu     sync.Mutex
	closed bool
	// conns holds every connection the forwarder is currently splicing, so that Close
	// tears them down instead of leaving a guest's flow alive after Boks says the sandbox
	// has no network.
	conns map[io.Closer]struct{}
	// noticed remembers which dropped destinations have already been mentioned in the
	// operational log. The guest chooses the volume of dropped datagrams, so a line per
	// packet would be a log-flooding primitive; a line per destination, capped, is a
	// diagnostic.
	noticed map[string]struct{}
}

// newHostStack assembles the stack. It binds no socket: the caller attaches the link.
func newHostStack(ctx context.Context, plan Plan, engine *policy.Engine, logger io.Writer) (*hostStack, error) {
	cfg := gatewayConfig(plan)

	_, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("network: subnet %q: %w", cfg.Subnet, err)
	}
	gatewayIP := net.ParseIP(cfg.GatewayIP).To4()
	if gatewayIP == nil {
		return nil, fmt.Errorf("network: the gateway address %q is not IPv4", cfg.GatewayIP)
	}
	// NewPlan defaults this, but a Plan can also arrive decoded from another process, so
	// the range is checked where it is used: the link endpoint takes a uint32 and the DHCP
	// option a uint16, and a value that wraps either produces a stack that misbehaves a
	// long way from its cause.
	if plan.MTU <= 0 || plan.MTU > math.MaxUint16 {
		return nil, fmt.Errorf("network: MTU %d is out of range (1-%d)", plan.MTU, math.MaxUint16)
	}

	h := &hostStack{
		plan:           plan,
		engine:         engine,
		logger:         logger,
		ctx:            ctx,
		metadataAccess: cfg.Ec2MetadataAccess,
		conns:          map[io.Closer]struct{}{},
		noticed:        map[string]struct{}{},
	}
	if engine == nil {
		logf(logger, "network: no policy engine was attached to this stack; every destination will be refused")
	}

	// The gateway's own address is reserved so the DHCP server can never lease it to a
	// guest that asks for one.
	pool := tap.NewIPPool(subnet)
	pool.Reserve(gatewayIP, plan.GatewayMAC)

	link, err := tap.NewLinkEndpoint(cfg.Debug, uint32(plan.MTU), plan.GatewayMAC, plan.Gateway.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("network: building the link endpoint: %w", err)
	}
	filtered := &filteredLink{LinkEndpoint: link, gateway: plan.Gateway, onDrop: h.noteLinkDrop}

	// The switch is the library's Ethernet fabric between the link socket and the stack.
	// Both directions have to be connected: the endpoint writes to the switch, and the
	// switch delivers to the endpoint — here, to the filtering wrapper around it, which is
	// the point at which UDP and ICMP stop.
	sw := tap.NewSwitch(cfg.Debug)
	filtered.Connect(sw)
	sw.Connect(filtered)

	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			arp.NewProtocol,
		},
		// TCP because it is the only thing forwarded, UDP because the gateway's own
		// resolver and DHCP server bind UDP sockets in this stack. ICMP is absent: no
		// ICMP socket is ever opened here, and its packets never reach the stack.
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
		},
	})

	if err := s.CreateNIC(nicID, filtered); err != nil {
		s.Close()
		return nil, fmt.Errorf("network: creating the NIC: %s", err)
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4Slice(gatewayIP).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		s.Close()
		return nil, fmt.Errorf("network: assigning the gateway address: %s", err)
	}

	// Spoofing lets the stack answer as an address it does not hold, and promiscuous mode
	// lets it accept a frame addressed to one. Both are what make a *forwarding* stack
	// possible at all: the guest addresses the destination directly, not the gateway, so
	// every packet worth judging is one the NIC would otherwise ignore.
	s.SetSpoofing(nicID, true)
	s.SetPromiscuousMode(nicID, true)

	route, err := tcpip.NewSubnet(tcpip.AddrFromSlice(subnet.IP), tcpip.MaskFromBytes(subnet.Mask))
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("network: building the route table: %w", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: route, NIC: nicID}})

	// The hook the library gives no way to reach through virtualnetwork.New: the handler
	// for TCP segments that match no bound endpoint. Everything the guest opens to the
	// outside world arrives here, and is judged before anything is dialled.
	forwarder := tcp.NewForwarder(s, 0, maxInFlight, h.forwardTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, forwarder.HandlePacket)
	// No UDP handler is installed, deliberately. Datagrams to the gateway's own services
	// reach their bound endpoints without one; anything else has nowhere to go, and the
	// link filter has already dropped it.

	h.stack = s
	h.sw = sw

	if err := h.startDNS(cfg); err != nil {
		s.Close()
		return nil, err
	}
	if err := h.startDHCP(cfg, pool); err != nil {
		s.Close()
		return nil, err
	}
	return h, nil
}

// startDNS binds the gateway's resolver, which is where the container's resolv.conf points.
//
// Mediating DNS matters for two reasons. It is the only resolver the guest can reach — the
// link filter drops UDP to anything else — so a guest cannot use DNS to a server of its
// choosing as a covert channel. And it is where a policy on *names* would attach when one
// exists: today the resolver answers whatever it is asked, through the host's own resolver,
// which is worth stating plainly rather than implying that name filtering is already here.
func (h *hostStack) startDNS(cfg *types.Configuration) error {
	addr := tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4())
	udpConn, err := gonet.DialUDP(h.stack, &tcpip.FullAddress{NIC: nicID, Addr: addr, Port: dnsPort}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("network: binding the gateway resolver: %w", err)
	}
	tcpLn, err := gonet.ListenTCP(h.stack, tcpip.FullAddress{NIC: nicID, Addr: addr, Port: dnsPort}, ipv4.ProtocolNumber)
	if err != nil {
		udpConn.Close()
		return fmt.Errorf("network: binding the gateway resolver over TCP: %w", err)
	}
	server, err := dns.New(udpConn, tcpLn, cfg.DNS)
	if err != nil {
		udpConn.Close()
		_ = tcpLn.Close()
		return fmt.Errorf("network: starting the gateway resolver: %w", err)
	}
	h.dnsUDP = udpConn
	h.dnsTCP = tcpLn

	go func() {
		if err := server.Serve(); err != nil && !h.stopping() {
			logf(h.logger, "network: the gateway resolver stopped: %v", err)
		}
	}()
	go func() {
		if err := server.ServeTCP(); err != nil && !h.stopping() {
			logf(h.logger, "network: the gateway resolver stopped answering over TCP: %v", err)
		}
	}()
	return nil
}

// startDHCP runs the address server for a guest that asks for one.
//
// Boks hands the container a static address in its annotations, so in the normal case
// nothing ever sends a DHCP request. It is kept because a guest whose interface is brought
// up by a DHCP client would otherwise have no network at all, and because it costs nothing
// worth having: the server binds a UDP port on this stack and hands out an address inside
// the sandbox's own subnet. It is not a forwarding path.
func (h *hostStack) startDHCP(cfg *types.Configuration, pool *tap.IPPool) error {
	server, err := dhcp.New(cfg, h.stack, pool)
	if err != nil {
		return fmt.Errorf("network: starting the address server: %w", err)
	}
	go func() {
		if err := server.Serve(); err != nil && !h.stopping() {
			logf(h.logger, "network: the address server stopped: %v", err)
		}
	}()
	return nil
}

// accept serves the link until the context is cancelled or the VM disconnects.
func (h *hostStack) accept(ctx context.Context, conn net.Conn) error {
	return h.sw.Accept(ctx, conn, types.VfkitProtocol)
}

// forwardTCP is the enforcement point: one call per connection the guest opens to something
// that is not a service inside its own virtual network.
//
// gvisor calls this in a goroutine of its own, one per in-flight request, which is what
// makes it safe to dial and then splice here rather than handing off.
func (h *hostStack) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	// "Local" is the stack's word for the address the segment was addressed *to*, which
	// from the guest's point of view is the remote destination. This is the address the
	// guest chose, unfiltered and untranslated: exactly what the policy has to judge.
	dst, ok := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	if !ok {
		r.Complete(true)
		return
	}
	dst = dst.Unmap()
	port := int(id.LocalPort)

	// The sandbox's own subnet is not a destination to dial: the only thing in it is this
	// gateway, whose real services are bound endpoints and never reach a forwarder. Dialling
	// it would send the guest's connection to whatever holds that address on the *host's*
	// network, which is not the same machine the guest was addressing.
	if h.plan.Subnet.Contains(dst) {
		h.noteDrop(fmt.Sprintf("tcp to %s:%d inside the sandbox's own network, where nothing answers", dst, port))
		r.Complete(true)
		return
	}

	// Link-local, which contains 169.254.169.254 — the cloud instance metadata endpoint,
	// and the best credential source on a hosted machine. gvisor-tap-vsock's own forwarder
	// refused this whenever its Ec2MetadataAccess flag was off, and gatewayConfig asserts
	// that flag off; refusing it here keeps the assertion true now that Boks owns the
	// forwarder. It is deliberately not a policy question: every preset denies link-local,
	// and this makes an explicit `-allow 169.254.169.254` fail too.
	if !h.metadataAccess && dst.IsLinkLocalUnicast() {
		h.noteDrop(fmt.Sprintf("tcp to link-local %s:%d, which includes the instance metadata endpoint", dst, port))
		r.Complete(true)
		return
	}

	target := policy.TargetFromAddr(netip.AddrPortFrom(dst, uint16(port)))
	if !h.judge(target) {
		// A RST rather than silence: a denied destination should fail the way a closed
		// port fails, immediately and legibly, instead of hanging until something times
		// out and leaving the user to guess whether it was policy or the network.
		r.Complete(true)
		return
	}

	dialCtx, cancel := context.WithTimeout(h.ctx, dialTimeout)
	defer cancel()
	outbound, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(dst.String(), strconv.Itoa(port)))
	if err != nil {
		// The policy permitted it and the destination did not answer. That is not a
		// policy event, so it stays out of the decision log and in the operational one.
		h.noteDrop(fmt.Sprintf("tcp to %s: %v", target, err))
		r.Complete(true)
		return
	}

	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	r.Complete(false)
	if tcpErr != nil {
		_ = outbound.Close()
		logf(h.logger, "network: accepting the guest's connection to %s: %s", target, tcpErr)
		return
	}
	h.splice(gonet.NewTCPConn(&wq, ep), outbound)
}

// judge asks the policy engine, and records the answer with mode "transparent".
//
// Both outcomes are logged, which is the point: before this existed, the connections that
// mattered most — the ones that ignored the proxy — were the ones `boks policy log` could
// not show. A raw flow has no hostname, so the destination is logged as the address it was,
// and the resource takes its IP form.
func (h *hostStack) judge(t policy.Target) bool {
	if h.engine == nil {
		// Fail closed. A stack with no policy attached is a misconfiguration, and the
		// safe reading of a missing rule set is "nothing is permitted".
		h.noteDrop(fmt.Sprintf("tcp to %s with no policy attached to this stack", t))
		return false
	}
	return h.engine.CheckMode(policy.StageNetwork, t, policy.ModeTransparent).Allowed
}

// splice carries a permitted connection in both directions until either end finishes.
//
// Half-close is honoured rather than tearing the whole connection down on the first EOF:
// protocols that shut down one direction and keep reading — an HTTP request without
// keep-alive, anything shaped like `cmd | ssh host` — break in ways that look like network
// faults if the other direction dies with them.
func (h *hostStack) splice(guest *gonet.TCPConn, outbound net.Conn) {
	if !h.track(guest, outbound) {
		_ = guest.Close()
		_ = outbound.Close()
		return
	}
	defer h.untrack(guest, outbound)

	var wg sync.WaitGroup
	wg.Add(2)
	go copyHalf(outbound, guest, &wg)
	go copyHalf(guest, outbound, &wg)
	wg.Wait()

	_ = guest.Close()
	_ = outbound.Close()
}

// copyHalf carries one direction and then shuts that direction of dst down.
func copyHalf(dst, src net.Conn, wg *sync.WaitGroup) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = dst.Close()
}

func (h *hostStack) track(cs ...io.Closer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	for _, c := range cs {
		h.conns[c] = struct{}{}
	}
	return true
}

func (h *hostStack) untrack(cs ...io.Closer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range cs {
		delete(h.conns, c)
	}
}

func (h *hostStack) stopping() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// maxNoticedDestinations caps how many distinct dropped destinations are ever mentioned in
// the operational log for one sandbox. The guest controls this input.
const maxNoticedDestinations = 64

// noteDrop records, at most once per destination, that something was refused for a reason
// that is not a policy decision. Policy decisions go to the decision log; these are
// diagnostics for the "why does nothing work" question, and a guest must not be able to
// turn them into unbounded disk usage.
func (h *hostStack) noteDrop(what string) {
	h.mu.Lock()
	if _, seen := h.noticed[what]; seen || len(h.noticed) >= maxNoticedDestinations {
		h.mu.Unlock()
		return
	}
	h.noticed[what] = struct{}{}
	h.mu.Unlock()
	logf(h.logger, "network: dropped %s", what)
}

// noteDroppedFlow records a link-level drop in the decision log as well as the operational
// one, so `boks policy log` shows it.
//
// UDP and ICMP are refused categorically rather than by a rule, so there is no policy check
// to record — Note exists for exactly that, carrying the reason instead of a verdict. Without
// this a guest could probe UDP or ICMP all day and leave no trace anywhere the user looks,
// which is the difference between containment and containment you can see.
//
// Rate limiting is the same as noteDrop's: once per distinct destination, capped, because the
// guest chooses how many packets to send and must not choose how large the log gets.
func (h *hostStack) noteDroppedFlow(target policy.Target, reason, operational string) {
	h.mu.Lock()
	_, seen := h.noticed[operational]
	full := len(h.noticed) >= maxNoticedDestinations
	if !seen && !full {
		h.noticed[operational] = struct{}{}
	}
	h.mu.Unlock()
	if seen || full {
		return
	}

	logf(h.logger, "network: dropped %s", operational)
	if h.engine != nil {
		h.engine.NoteRefused(policy.StageNetwork, target, policy.ModeTransparent, reason)
	}
}

// listen returns a listener inside the virtual network.
func (h *hostStack) listen(addr string) (net.Listener, error) {
	ip, port, err := splitIPPort(addr)
	if err != nil {
		return nil, err
	}
	l, err := gonet.ListenTCP(h.stack, tcpip.FullAddress{
		NIC:  nicID,
		Addr: tcpip.AddrFrom4Slice(ip.To4()),
		Port: port,
	}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("network: listening on %s inside the sandbox network: %w", addr, err)
	}
	return l, nil
}

// dial opens a connection from the host side into the virtual network.
func (h *hostStack) dial(ctx context.Context, addr string) (net.Conn, error) {
	ip, port, err := splitIPPort(addr)
	if err != nil {
		return nil, err
	}
	conn, err := gonet.DialContextTCP(ctx, h.stack, tcpip.FullAddress{
		NIC:  nicID,
		Addr: tcpip.AddrFrom4Slice(ip.To4()),
		Port: port,
	}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("network: dialling %s inside the sandbox network: %w", addr, err)
	}
	return conn, nil
}

// stop closes every flow the stack is carrying and then the stack itself.
//
// Closing the spliced connections first is what makes "the sandbox has no network any more"
// true rather than aspirational: a flow the guest opened before teardown must not outlive
// the stack that judged it. Stack.Wait is deliberately not called — it waits on endpoints
// the guest still holds and would turn a teardown into an indefinite block.
func (h *hostStack) stop() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	conns := make([]io.Closer, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.conns = map[io.Closer]struct{}{}
	h.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	if h.dnsUDP != nil {
		h.dnsUDP.Close()
	}
	if h.dnsTCP != nil {
		_ = h.dnsTCP.Close()
	}
	h.stack.Close()
}

// splitIPPort parses "ip:port". Only literals: nothing inside a sandbox's virtual network
// has a name, and resolving one here would mean asking the host's resolver what an address
// in a private stack means.
func splitIPPort(addr string) (net.IP, uint16, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, 0, fmt.Errorf("network: address %q: %w", addr, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, 0, fmt.Errorf("network: port in %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return nil, 0, fmt.Errorf("network: %q is not an IPv4 address", host)
	}
	return ip, uint16(port), nil
}

// filteredLink is the link endpoint with an inbound filter in front of it.
//
// It wraps gvisor-tap-vsock's endpoint rather than replacing it: the Ethernet handling,
// ARP behaviour and write path are the library's, and only what the stack is allowed to
// *see* is ours. Filtering here rather than in the stack means a dropped datagram costs one
// header parse and never allocates an endpoint, a route or a reply.
type filteredLink struct {
	*tap.LinkEndpoint
	gateway netip.Addr
	onDrop  func(dropReason)
}

// DeliverNetworkPacket is the guest's entire inbound path. Everything the sandbox emits
// arrives here first.
func (e *filteredLink) DeliverNetworkPacket(protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	if why, drop := e.classify(protocol, pkt); drop {
		if e.onDrop != nil && why.operational != "" {
			e.onDrop(why)
		}
		return
	}
	e.LinkEndpoint.DeliverNetworkPacket(protocol, pkt)
}

// dropReason describes a frame the link refused, in the two forms the two logs want.
//
// A frame with no addressable destination — a truncated packet, a non-IPv4 ethertype — has
// no target, and `addressed` says so. Those still reach the operational log, because
// something the guest sent was thrown away and an operator may need to know, but they cannot
// become a policy decision about a destination that was never legible.
type dropReason struct {
	operational string
	reason      string
	target      policy.Target
	addressed   bool
}

// classify decides whether a frame from the guest may reach the stack at all.
//
// This is a whitelist, and the shape of it is the point: TCP is passed because the forwarder
// judges it; ARP is passed because a guest that cannot find the gateway has no network; UDP
// is passed only to the two services the gateway itself runs; everything else — ICMP, IPv6,
// any other IP protocol — is dropped without a reply.
//
// The reference product documents UDP and ICMP as blocked at the network layer and not
// re-enablable by policy. This matches that, and for the same reason: neither carries a
// connection that could be judged, and DNS to an arbitrary server is a channel around every
// hostname rule.
func (e *filteredLink) classify(protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) (why dropReason, drop bool) {
	switch protocol {
	case header.ARPProtocolNumber:
		return dropReason{}, false
	case header.IPv4ProtocolNumber:
	default:
		// IPv6 above all: a guest brings up link-local v6 by itself, and nothing here
		// routes it. Dropping it is what keeps "this stack is IPv4-only" true.
		return dropReason{operational: fmt.Sprintf(
			"a frame of ethertype 0x%04x, which this stack does not carry", uint16(protocol))}, true
	}

	hdr, ok := pkt.Data().PullUp(header.IPv4MinimumSize)
	if !ok {
		return dropReason{operational: "a truncated IPv4 packet"}, true
	}
	ip := header.IPv4(hdr)
	dstAddr := ip.DestinationAddress()
	dst, _ := netip.AddrFromSlice(dstAddr.AsSlice())
	dst = dst.Unmap()

	switch ip.TransportProtocol() {
	case header.TCPProtocolNumber:
		// Passed on to the forwarder, which is where the policy decision is taken and
		// logged. Nothing is decided here.
		return dropReason{}, false
	case header.UDPProtocolNumber:
		port, ok := udpDestinationPort(ip, pkt)
		if !ok {
			return dropReason{operational: fmt.Sprintf(
				"a truncated or fragmented UDP datagram to %s", dst)}, true
		}
		// The resolver, on the gateway's own address only: DNS anywhere else is the
		// covert channel this is here to close.
		if port == dnsPort && dst == e.gateway {
			return dropReason{}, false
		}
		// DHCP is broadcast before the guest has an address, so its destination cannot
		// be checked. The server binds inside this stack and hands out an address in the
		// sandbox's own subnet; there is nothing to forward and nowhere to reach.
		if port == dhcpPort {
			return dropReason{}, false
		}
		return dropReason{
			operational: fmt.Sprintf("udp to %s:%d (only DNS to the gateway is carried)", dst, port),
			reason:      "udp is not carried; only DNS to the sandbox's own resolver is",
			target:      policy.TargetFromAddr(netip.AddrPortFrom(dst, port)),
			addressed:   true,
		}, true
	case header.ICMPv4ProtocolNumber:
		return dropReason{
			operational: fmt.Sprintf("icmp to %s (icmp is not carried)", dst),
			reason:      "icmp is not carried, and no rule can permit it",
			target:      policy.TargetFromAddr(netip.AddrPortFrom(dst, 0)),
			addressed:   true,
		}, true
	default:
		proto := ip.TransportProtocol()
		return dropReason{
			operational: fmt.Sprintf("ip protocol %d to %s (only tcp is carried)", proto, dst),
			reason:      fmt.Sprintf("ip protocol %d is not carried; only tcp is", proto),
			target:      policy.TargetFromAddr(netip.AddrPortFrom(dst, 0)),
			addressed:   true,
		}, true
	}
}

// udpDestinationPort reads the destination port out of a UDP datagram.
//
// A fragment after the first carries no UDP header, so its ports cannot be known; it is
// reported as unreadable and therefore dropped, rather than guessed at.
func udpDestinationPort(ip header.IPv4, pkt *stack.PacketBuffer) (uint16, bool) {
	if ip.FragmentOffset() != 0 {
		return 0, false
	}
	ihl := int(ip.HeaderLength())
	if ihl < header.IPv4MinimumSize {
		return 0, false
	}
	b, ok := pkt.Data().PullUp(ihl + header.UDPMinimumSize)
	if !ok {
		return 0, false
	}
	return header.UDP(b[ihl:]).DestinationPort(), true
}

// noteLinkDrop records a frame the link refused. A drop with a legible destination becomes a
// decision as well as an operational line; one without stays operational only.
func (h *hostStack) noteLinkDrop(why dropReason) {
	if !why.addressed {
		h.noteDrop(why.operational)
		return
	}
	h.noteDroppedFlow(why.target, why.reason, why.operational)
}
