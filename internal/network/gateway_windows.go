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
// What Boks lacks is one device in a VMM that is already being ported. libkrun has had a WHP
// backend in tree since mid-2026 — the vCPU loop, the instruction emulator, an IOCP-based
// epoll shim, and Windows ports of virtio-fs, -blk, -console, -balloon and -rng — targeting
// libkrun 2.0. **virtio-net is the single device that was not ported**, which is exactly and
// only the one this gateway needs. Upstream nerdbox already builds for windows/amd64 and loads
// the VMM as a DLL named krun.dll, so nothing above the VMM needs to change either.
//
// When that lands, the Windows link is nerdbox's `unixstream` mode rather than `unixgram`:
// libkrun frames each Ethernet frame with a 4-byte big-endian length, which is byte-for-byte
// gvisor-tap-vsock's `qemu` protocol. Boks already links that codec — see stack_unix.go, which
// passes types.VfkitProtocol to Switch.Accept; Windows would pass types.QemuProtocol over a
// stream conn and change nothing else.
//
// (Two things that are true and are *not* the reason, recorded so they are not rediscovered as
// blockers: the Host Compute Service, which drives Hyper-V containers and LCOW, really does
// expose no socket-backed NIC — its NetworkAdapter is an HNS endpoint id and nothing else. And
// Windows' AF_UNIX really does implement only SOCK_STREAM. Neither is load-bearing: a WHP VMM
// need not go near HCS, and SOCK_STREAM is precisely what `unixstream` wants.)
//
// None of this has been executed on Windows — no machine on this project has it. See
// docs/windows.md.
type gateway struct{}

func (g *gateway) start(context.Context, Plan, *policy.Engine, io.Writer) error {
	return errors.New("network: sandbox networking is not available on Windows; " +
		"libkrun's Windows Hypervisor Platform backend does not yet include virtio-net, " +
		"so nothing emits the guest's frames onto a host link this stack could terminate " +
		"(see docs/windows.md)")
}

func (g *gateway) listen(string) (net.Listener, error) { return nil, ErrNoNetwork }

func (g *gateway) dial(context.Context, string) (net.Conn, error) { return nil, ErrNoNetwork }

func (g *gateway) stop() error { return nil }
