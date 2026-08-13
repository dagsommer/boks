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
// It also is no longer a missing device, and this comment used to say it was. virtio-net was
// the one device libkrun's WHP backend had not ported — which was exactly and only the one
// this link needs — so a Windows sandbox would have bound this socket and waited forever for
// a peer that could not exist. That stopped being true on 2026-08-13: packaging/libkrun-windows/
// carries a Winsock AF_UNIX backend for it, krun.dll links with `--features blk,net` on a
// Windows CI runner, and `krun_add_net_unixstream` is exported.
//
// What is missing now is evidence, not capability. Not one Ethernet frame has crossed that
// device. The guest boots and its clock runs, but every boot so far has been a direct C probe
// against krun.dll — containerd and the nerdbox shim have only just been made to start on
// Windows, and neither has yet carried a container. Until a frame is observed arriving here
// from a real guest, this refusal stands on "unexercised", which is a different and weaker
// claim than the one it used to make.
//
// What would lift it: a sandbox started through containerd and the shim, with the policy
// engine judging a flow from that guest. Nothing below this line needs to change for that to
// work — the refusal is the only thing in the way on this side.
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
	return errors.New("network: sandbox networking is not available on Windows yet; " +
		"the pieces exist — libkrun's Windows backend now carries virtio-net and krun.dll " +
		"links with it — but no frame has ever crossed that device, and no sandbox has been " +
		"started through containerd and the shim on Windows, so this is unexercised rather " +
		"than impossible (see docs/windows.md)")
}
