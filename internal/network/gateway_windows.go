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
// The reason has been wrong twice, so it is worth stating precisely.
//
// It is *not* "nerdbox has no Windows support", which implied a runtime gap. Nor is it that
// Windows cannot do this: the spike behind docs/windows.md established that it can, and that
// the reference product does. Docker Sandboxes runs its Linux microVMs on Windows through the
// **Windows Hypervisor Platform** — a user-mode hypervisor API — with a VMM that maps guest
// memory into its own process, emulates virtio-net in user space, and terminates the guest's
// TCP/IP in a userspace stack. That is precisely Boks' model, working, on Windows.
//
// What Boks lacks is a VMM that speaks WHP. libkrun targets KVM and Hypervisor.framework;
// Docker substituted a VMM of their own, which is not open source. So the link this gateway
// would own — a host socket carrying the guest's Ethernet frames — has nothing to attach to,
// not because the platform forbids it but because the component that would emit those frames
// does not exist in Boks' stack yet.
//
// (Two things that are true and are *not* the reason, recorded so they are not rediscovered
// as blockers: the Host Compute Service, which drives Hyper-V containers and LCOW, really does
// expose no socket-backed NIC — its NetworkAdapter is an HNS endpoint id and nothing else. And
// Windows' AF_UNIX really does implement only SOCK_STREAM, so the `unixgram` link Boks uses
// today has no Windows equivalent and a different link type would be needed. Neither is
// load-bearing, because a WHP VMM need not use HCS and need not use a datagram socket.)
//
// None of this has been executed on Windows — no machine on this project has it. See
// docs/windows.md.
type gateway struct{}

func (g *gateway) start(context.Context, Plan, *policy.Engine, io.Writer) error {
	return errors.New("network: sandbox networking is not available on Windows; " +
		"Boks has no VMM that speaks the Windows Hypervisor Platform, so nothing emits " +
		"the guest's frames onto a host link this stack could terminate (see docs/windows.md)")
}

func (g *gateway) listen(string) (net.Listener, error) { return nil, ErrNoNetwork }

func (g *gateway) dial(context.Context, string) (net.Conn, error) { return nil, ErrNoNetwork }

func (g *gateway) stop() error { return nil }
