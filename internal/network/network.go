// Package network gives a sandbox its network: it computes the nerdbox annotations that
// attach a virtual NIC to the VM and wire the container to it, and it runs the host-side
// stack that terminates the other end of that NIC.
//
// # Why this exists
//
// nerdbox's default is libkrun's TSI, which has no NIC at all: the guest's AF_INET socket
// calls are rewritten and performed *on the host*, so the guest reaches the host's own
// 127.0.0.1 and Boks has no point at which it can see or drop a flow. An external network
// provider replaces that with a real virtio-net link whose far end is a userspace network
// stack in this process — which is the property enforcement needs, because a guest that
// opens a raw socket still has its packets terminated by something Boks controls.
//
// *(Verified on 2026-08-11 on a macOS host with a working hypervisor, by a separate spike:
// with the annotations below, the guest gains eth0, and a probe of a host service on
// 127.0.0.1 that TSI happily answered now returns "connection refused" — the call is being
// handled by the guest's own loopback stack instead of being impersonated on the host.
// Three further signals agreed: a ninth virtio device (VIRTIO_ID_NET) appears, the host
// stack logs frames from the VM's MAC, and the guest's resolv.conf switches from a copy of
// the host's to the gateway this package configures.)*
//
// **The link is a stream now** — `mode=unixstream` rather than `mode=unixgram`,
// gvisor-tap-vsock's qemu framing rather than vfkit's — and the check was re-run against a
// real guest on 2026-08-13 over that transport, on macOS/Apple silicon. Denied destinations
// are refused before anything is dialled, two explicitly allowed addresses reach the origin
// carrying Cloudflare's own certificate, host loopback is unreachable, and every refusal
// appears in the log as `transparent`. The stream parser moved 5.8 MB byte-identical to a
// host reference and served 30 concurrent flows without interleaving. See link.go for why the
// change was made, and docs/verification.md for the full procedure and its limits — chiefly
// that this is one OS on one architecture, and that no VM has ever booted on Windows.
//
// # What this package now enforces
//
// The stack it runs is assembled here rather than taken whole from gvisor-tap-vsock, and the
// reason is the whole point of the package: the library's own TCP forwarder dials whatever
// address the guest puts in a SYN, with no policy consulted. Boks installs its own, which
// asks the policy engine first, refuses what is denied, and records both outcomes. UDP and
// ICMP are dropped at the link, apart from DNS to the gateway's own resolver. See
// stack.go for the assembly and the reasoning.
//
// The enforcement is verified against a simulated guest on the real link socket
// (internal/network/vnettest), speaking the real framing over the real socket. "The stack
// refuses it" is proven that way. "A real VM was refused over *this* link" is not: the run
// that saw a real guest refused used the datagram transport this package no longer asks for.
// Do not describe it as such.
//
// # The two annotations
//
// Both are needed, and each does half the job. This is confirmed against nerdbox's source
// (internal/shim/task/networking.go and ctrnetworking.go), not only its documentation,
// because the documentation is wrong in at least one place — it shows `addr=192.168.127.2`
// while the parser calls netip.ParsePrefix and rejects anything that is not CIDR.
//
//	io.containerd.nerdbox.network.N       attaches a NIC to the VM
//	    socket= (required)  host UNIX socket carrying the link
//	    mode=   (required)  unixgram (SOCK_DGRAM, one frame per datagram) or
//	                        unixstream (SOCK_STREAM, each frame length-prefixed).
//	                        Boks asks for unixstream; see link.go for why.
//	    mac=    (required)  unicast MAC; the multicast bit must be clear
//	    addr=   (optional)  CIDR, at most once per address family
//	    vfkit=, vnet_hdr=, features=  (optional) — all deliberately unset, see
//	                        Plan.Annotations
//
//	io.containerd.nerdbox.ctr.network.N   wires the container to that NIC
//	    vmmac=  (required)  identifies which VM NIC this is
//	    addr=   (optional)  CIDR — a bare IP is rejected
//	    gw=, ifname=, mac=  (optional)
//
//	io.containerd.nerdbox.ctr.dns         writes the container's /etc/resolv.conf
//	    each key=value becomes a "key value" line
//
// With only the VM-level annotation, the VM has the NIC but the container is never wired to
// it: the container sees `lo` only and TSI is off. That is ModeNone — the strongest
// containment Boks can offer today, and it costs nothing.
//
// The shim deletes these annotations from the spec after parsing, so they do not reach the
// guest. No OCI spec change and no CAP_NET_ADMIN are needed, despite what nerdbox's README
// example suggests.
package network

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
)

// Mode selects how a sandbox is connected.
type Mode string

const (
	// ModeNone gives the sandbox no network. A NIC is attached to the VM — which is
	// what turns TSI off — but the container is never wired to it, so it has loopback
	// and nothing else. Use this whenever the workload does not need the network: its
	// containment is an absence of wiring rather than a decision anything takes at
	// runtime, and it is the only posture that has been confirmed against a real guest.
	ModeNone Mode = "none"

	// ModeNAT connects the sandbox to a userspace network stack running on the host,
	// which carries permitted flows out through the host's real network and refuses the
	// rest. This is the mode a network policy is enforced in: the stack judges every TCP
	// connection the guest opens before it dials it, and drops UDP and ICMP outright.
	ModeNAT Mode = "nat"
)

// DefaultMode is what a sandbox gets when nothing is said.
//
// It is ModeNAT rather than ModeNone because a sandbox with no network at all would
// surprise every user who expects `go build` to work — but note that ModeNAT's containment
// is only as good as the enforcement built on top of it, which has never been exercised
// against a real guest. ModeNone is the honest choice when in doubt.
const DefaultMode = ModeNAT

// Modes lists the modes in decreasing order of containment.
func Modes() []Mode { return []Mode{ModeNone, ModeNAT} }

// ParseMode parses a mode name.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeNone:
		return ModeNone, nil
	case ModeNAT, "":
		return ModeNAT, nil
	}
	return "", fmt.Errorf("unknown network mode %q; use one of: none, nat", s)
}

// Default addressing. These follow gvisor-tap-vsock's own conventions so that its DHCP and
// DNS internals agree with what Boks writes into the container's configuration. One stack
// serves exactly one sandbox, so there is no address space to share and no reason to make
// these configurable yet.
const (
	DefaultSubnet     = "192.168.127.0/24"
	DefaultGatewayIP  = "192.168.127.1"
	DefaultGuestIP    = "192.168.127.2"
	DefaultMTU        = 1500
	defaultInterface  = "eth0"
	annotationVMNet   = "io.containerd.nerdbox.network"
	annotationCtrNet  = "io.containerd.nerdbox.ctr.network"
	annotationCtrDNS  = "io.containerd.nerdbox.ctr.dns"
	unixPathMaxDarwin = 104 // sun_path on macOS/BSD; Linux allows 108
)

// ModeFromAnnotations reads back the mode a container was created with, and whether Boks
// wired its network at all.
//
// A sandbox's mode is fixed when it is created, because it is expressed in annotations the
// runtime reads at boot. A later command that has to bring the sandbox up — `boks exec` on a
// stopped sandbox, `boks start` — must serve the network the container was wired for rather
// than the one its own flags default to, or it would attach a stack to a container that is
// not connected to it, or none to a container that is.
//
// The second return value matters more than it looks. A container with no VM NIC annotation
// was not merely given "no network": it falls back to the runtime's own transport, which is
// libkrun's TSI, where the guest's 127.0.0.1 is *the host's*. That is the opposite of
// containment, and it is what every sandbox created before this wiring existed looks like,
// so it has to be distinguishable from ModeNone rather than folded into it.
func ModeFromAnnotations(annotations map[string]string) (mode Mode, wired bool) {
	if _, ok := annotations[annotationVMNet+".0"]; !ok {
		return ModeNone, false
	}
	if _, ok := annotations[annotationCtrNet+".0"]; ok {
		return ModeNAT, true
	}
	return ModeNone, true
}

// Config describes the network a sandbox should get.
type Config struct {
	Mode Mode
	// Sandbox names the sandbox, and is used to build a socket path a human can
	// recognise in `lsof` output.
	Sandbox string
	// RuntimeDir is where the link socket is created. One directory per sandbox is
	// created inside it so that a crashed run leaves an obvious, removable remnant.
	RuntimeDir string
	// MTU, Subnet, GatewayIP and GuestIP override the defaults above.
	MTU       int
	Subnet    string
	GatewayIP string
	GuestIP   string
}

// Plan is the computed, side-effect-free result of a Config: everything the shim needs,
// and nothing started yet.
type Plan struct {
	Mode Mode
	// Socket is the host UNIX socket carrying the virtio-net link.
	Socket string
	// VMMAC is the MAC of the VM's NIC. It appears in both annotations, which is how
	// nerdbox ties the container's interface to the VM's device.
	VMMAC string
	// GatewayMAC is the MAC the host-side stack answers ARP with. It is generated
	// rather than fixed so that two sandboxes never share one.
	GatewayMAC string
	// GuestAddr is the address the container's interface gets, as CIDR.
	GuestAddr netip.Prefix
	// Gateway is the host-side stack's address inside the virtual network, and the
	// resolver the container is pointed at.
	Gateway netip.Addr
	// Subnet is the virtual network.
	Subnet netip.Prefix
	MTU    int
}

// NewPlan computes a plan. It performs no I/O beyond reading randomness, so it can be
// tested exhaustively and inspected before anything is started.
func NewPlan(cfg Config) (Plan, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = DefaultMode
	}
	if _, err := ParseMode(string(mode)); err != nil {
		return Plan{}, err
	}
	if cfg.Sandbox == "" {
		return Plan{}, fmt.Errorf("network: no sandbox name")
	}
	if cfg.RuntimeDir == "" {
		return Plan{}, fmt.Errorf("network: no runtime directory for the link socket")
	}

	subnet, err := netip.ParsePrefix(orDefault(cfg.Subnet, DefaultSubnet))
	if err != nil {
		return Plan{}, fmt.Errorf("network: subnet %q: %w", cfg.Subnet, err)
	}
	gateway, err := netip.ParseAddr(orDefault(cfg.GatewayIP, DefaultGatewayIP))
	if err != nil {
		return Plan{}, fmt.Errorf("network: gateway %q: %w", cfg.GatewayIP, err)
	}
	guest, err := netip.ParseAddr(orDefault(cfg.GuestIP, DefaultGuestIP))
	if err != nil {
		return Plan{}, fmt.Errorf("network: guest address %q: %w", cfg.GuestIP, err)
	}
	if !subnet.Contains(gateway) {
		return Plan{}, fmt.Errorf("network: gateway %s is outside subnet %s", gateway, subnet)
	}
	if !subnet.Contains(guest) {
		return Plan{}, fmt.Errorf("network: guest address %s is outside subnet %s", guest, subnet)
	}
	if guest == gateway {
		return Plan{}, fmt.Errorf("network: the guest and the gateway cannot share address %s", guest)
	}

	mac, err := randomMAC()
	if err != nil {
		return Plan{}, err
	}
	gwMAC, err := randomMAC()
	if err != nil {
		return Plan{}, err
	}

	socket := filepath.Join(cfg.RuntimeDir, sanitize(cfg.Sandbox), "net.sock")
	if err := checkSocketPath(socket); err != nil {
		return Plan{}, err
	}

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = DefaultMTU
	}

	return Plan{
		Mode:       mode,
		Socket:     socket,
		VMMAC:      mac,
		GatewayMAC: gwMAC,
		GuestAddr:  netip.PrefixFrom(guest, subnet.Bits()),
		Gateway:    gateway,
		Subnet:     subnet,
		MTU:        mtu,
	}, nil
}

// Annotations returns the OCI annotations to set on the container.
//
// ModeNone deliberately emits only the VM-level annotation. That single omission is the
// difference between "a sandbox on a controlled network" and "a sandbox with no network",
// and it is worth being explicit about rather than hiding behind a boolean.
func (p Plan) Annotations() map[string]string {
	vm := []string{
		"socket=" + p.Socket,
		// unixstream is libkrun's stream backend: an AF_UNIX SOCK_STREAM socket it
		// connects to, with each Ethernet frame prefixed by a 4-byte big-endian
		// length. That framing is gvisor-tap-vsock's qemu protocol, which is what the
		// host stack reads. The two other flags nerdbox accepts here are deliberately
		// absent: `vfkit=true` would make libkrun send a magic sequence this protocol
		// does not expect, and `vnet_hdr=true` would put a virtio-net header in front
		// of every frame, which the switch would parse as Ethernet.
		"mode=unixstream",
		"mac=" + p.VMMAC,
	}
	out := map[string]string{
		annotationVMNet + ".0": strings.Join(vm, ","),
	}
	if p.Mode == ModeNone {
		return out
	}

	ctr := []string{
		"vmmac=" + p.VMMAC,
		// CIDR, not a bare address: nerdbox parses this with netip.ParsePrefix and
		// rejects anything else, whatever its documentation shows.
		"addr=" + p.GuestAddr.String(),
		"gw=" + p.Gateway.String(),
		"ifname=" + defaultInterface,
	}
	out[annotationCtrNet+".0"] = strings.Join(ctr, ",")

	// Point the container's resolver at the host-side gateway, rather than letting it
	// inherit a copy of the host's resolv.conf. This is the DNS mediation point: with
	// it, name resolution is answered by a stack Boks controls, so DNS is not a free
	// covert channel past a hostname allowlist. It does not *close* that channel — the
	// gateway still resolves whatever it is asked, and query names leak to the upstream
	// resolver — but it is the hook a future policy on names will attach to.
	out[annotationCtrDNS] = "nameserver=" + p.Gateway.String()
	return out
}

// randomMAC generates a locally administered unicast MAC.
//
// nerdbox rejects a MAC with the multicast bit set, and a globally unique address is not
// ours to invent, so bit 1 (locally administered) is set and bit 0 (multicast) cleared.
func randomMAC() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("network: generating a MAC address: %w", err)
	}
	buf[0] = (buf[0] | 0x02) &^ 0x01
	return net.HardwareAddr(buf[:]).String(), nil
}

// checkSocketPath rejects a path that will not fit in sockaddr_un.
//
// A UNIX socket path is a fixed-size field, and overflowing it fails at bind time with an
// error that says nothing useful about which path was too long. macOS is the tighter of the
// three limits, so it is the one enforced everywhere: a sandbox that works on Linux and
// fails on macOS for this reason would be a miserable bug to chase. Windows' AF_UNIX has the
// same fixed field and a comparable limit, so the same check applies there.
//
// This used to refuse on Windows outright, because the link was a SOCK_DGRAM socket and
// Windows' AF_UNIX has never had one. The link is a stream now, so a socket path is just a
// socket path here; what Windows still lacks is a VMM to connect to it, which is refused
// where it is true — see vmm_windows.go.
func checkSocketPath(path string) error {
	if len(path) >= unixPathMaxDarwin {
		return fmt.Errorf("network: the link socket path is %d characters, over the %d-byte limit "+
			"for UNIX sockets: %s\nUse a shorter sandbox name or set a shorter runtime directory",
			len(path), unixPathMaxDarwin, path)
	}
	return nil
}

// sanitize keeps a sandbox name usable as a directory component.
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
