package network

import "errors"

// vmmSupported refuses on Windows, and the reason has been wrong twice, so it is worth
// stating precisely.
//
// It is *not* "nerdbox has no Windows support", which implied a runtime gap. Nor is it that
// Windows cannot do this: the spike behind docs/windows.md established that it can, and that
// the reference product does. Docker Sandboxes runs its Linux microVMs on Windows through the
// **Windows Hypervisor Platform** — a user-mode hypervisor API — with a VMM that maps guest
// memory into its own process, emulates virtio-net in user space, and terminates the guest's
// TCP/IP in a userspace stack. That is precisely Boks' model, working, on Windows.
//
// And it is no longer the transport. It used to be: the link was an AF_UNIX SOCK_DGRAM
// socket, Windows' AF_UNIX has only SOCK_STREAM, and that one dependency was what compiled
// the whole host stack for Unix only. The link is a stream now — libkrun's `unixstream`
// framing, which is gvisor-tap-vsock's `qemu` protocol — so the stack, the switch, the
// policy forwarder and the socket all build and run here. Nothing above this function is
// Unix-specific any more.
//
// What Boks lacks is one device in a VMM that is already being ported. libkrun has had a WHP
// backend in tree since mid-2026 — the vCPU loop, the instruction emulator, an IOCP-based
// epoll shim, and Windows ports of virtio-fs, -blk, -console, -balloon and -rng — targeting
// libkrun 2.0. **virtio-net is the single device that was not ported**, which is exactly and
// only the one this link needs. Upstream nerdbox already builds for windows/amd64 and loads
// the VMM as a DLL named krun.dll, so nothing above the VMM needs to change either.
//
// So a Windows sandbox would bind this socket and wait forever for a peer that cannot exist.
// Refusing here says that once, early, instead of leaving a user with a booting VM and no
// network.
//
// (Two things that are true and are *not* the reason, recorded so they are not rediscovered
// as blockers: the Host Compute Service, which drives Hyper-V containers and LCOW, really
// does expose no socket-backed NIC — its NetworkAdapter is an HNS endpoint id and nothing
// else. And Windows' AF_UNIX really does implement only SOCK_STREAM. Neither is
// load-bearing: a WHP VMM need not go near HCS, and SOCK_STREAM is precisely what
// `unixstream` wants.)
//
// None of this has been executed on Windows — no machine on this project has it. See
// docs/windows.md.
func vmmSupported() error {
	return errors.New("network: sandbox networking is not available on Windows; " +
		"libkrun's Windows Hypervisor Platform backend does not yet include virtio-net, " +
		"so nothing emits the guest's frames onto a host link this stack could terminate " +
		"(see docs/windows.md)")
}
