package network

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/dagsommer/boks/internal/policy"
)

// gateway on Windows exists only so the package compiles there, and it refuses.
//
// The earlier version of this file blamed the runtime ("nerdbox has no Windows support"),
// which implied that a Windows-capable VM runtime would be enough. It would not, and the
// distinction matters enough to record here rather than only in the docs.
//
// Boks enforces by owning the far end of the guest's NIC: the VMM writes the guest's Ethernet
// frames to a host socket, and a userspace netstack in this process terminates and judges
// them. That shape needs a hypervisor which can back a virtual NIC with a host socket.
// Hyper-V cannot. The Host Compute Service's device schema has exactly one networking device,
// `NetworkAdapter`, whose only meaningful field is an HNS/HCN `EndpointId` — there is no
// socket path, no file descriptor and no external provider. The guest's frames land on the
// Windows vSwitch, where the only supported interposition is a kernel-mode NDIS filter or WFP
// callout driver, not a userspace process.
//
// Hyper-V sockets do not close the gap. gvisor-tap-vsock does implement an hvsock transport
// (pkg/transport/listen_windows.go), but hvsock is a SOCK_STREAM channel between two
// processes, not an L2 link: it carries frames only because a cooperating agent inside the
// guest — gvforwarder — owns a tap device and shuttles them. That is how podman machine works
// on Hyper-V, and it is a different security posture from a VMM-backed NIC, because the
// datapath depends on a component living inside the boundary Boks is trying to enforce.
//
// The SOCK_DGRAM link Boks uses today is separately unavailable: Windows' AF_UNIX implements
// only SOCK_STREAM, so `unixgram` has no implementation to have.
//
// None of this has been executed on Windows — no machine on this project has Hyper-V. It is
// read from hcsshim's, containerd's and gvisor-tap-vsock's source. See docs/windows.md.
type gateway struct{}

func (g *gateway) start(context.Context, Plan, *policy.Engine, io.Writer) error {
	return errors.New("network: sandbox networking is not available on Windows; " +
		"Hyper-V attaches a VM NIC as an HNS endpoint and offers no socket-backed NIC, " +
		"so the host cannot terminate and judge the guest's traffic (see docs/windows.md)")
}

func (g *gateway) listen(string) (net.Listener, error) { return nil, ErrNoNetwork }

func (g *gateway) dial(context.Context, string) (net.Conn, error) { return nil, ErrNoNetwork }

func (g *gateway) stop() error { return nil }
