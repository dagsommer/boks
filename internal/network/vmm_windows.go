package network

// Windows is attempted now, and this file is the record of what that does and does not mean.
//
// It used to hold a refusal — vmmSupported() — that Network.Start consulted before it bound
// anything, and the reason for it was wrong twice before it was right. The history is worth
// keeping, because each of those reasons is a thing somebody could otherwise rediscover as a
// blocker:
//
//   - It was *not* "nerdbox has no Windows support", which implied a runtime gap. Windows can
//     do this and the reference product does it: Docker Sandboxes runs Linux microVMs on
//     Windows through the **Windows Hypervisor Platform**, a user-mode hypervisor API, with a
//     VMM that maps guest memory into its own process, emulates virtio-net in user space, and
//     terminates the guest's TCP/IP in a userspace stack. That is precisely Boks' model.
//   - It was not the transport either, although for a while it genuinely was. The link was an
//     AF_UNIX SOCK_DGRAM socket, Windows' AF_UNIX has only SOCK_STREAM, and that one
//     dependency compiled the whole host stack for Unix only. The link is a stream now —
//     libkrun's `unixstream` framing, which is gvisor-tap-vsock's `qemu` protocol — so the
//     stack, the switch, the policy forwarder and the socket are the same code everywhere.
//   - It was not a missing device by the end. virtio-net was the one device libkrun's WHP
//     backend had not ported, which was exactly and only the one this link needs. That
//     stopped being true on 2026-08-13: packaging/libkrun-windows/ carries a Winsock AF_UNIX
//     backend for it, krun.dll links with `--features blk,net` on a Windows CI runner, and
//     `krun_add_net_unixstream` is exported.
//
// What was left was the refusal itself, standing on "unexercised" — and a refusal is a poor
// way to hold that position, because it guarantees the thing stays unexercised. On 2026-08-14
// a container ran end to end in a microVM on Windows 11 through `ctr`: containerd, the nerdbox
// shim, krun.dll, WHP, a 6.12.44 guest kernel and an advancing clock (docs/verification.md).
// Everything below Boks works there. So Boks now tries, and the honest failure has moved from
// "we will not attempt this" to "we attempted it and here is exactly what did not happen".
//
// # What is still not true
//
// **No Ethernet frame has ever crossed libkrun's virtio-net device on Windows.** Not one. The
// device compiles, links and exports its entry point; nothing has been observed writing a
// frame into it or reading one out of it. Every boot on Windows so far has been either a
// direct C probe against krun.dll or a `ctr` run with no network attached at all. So `boks
// run` on Windows is an attempt, and Unexercised below says so to anything that wants to warn
// a user or bound a wait.
//
// This matters more than "a feature might not work", and the reason is the fallback. A shim
// that ignores the network annotations does not leave the guest with no network: it leaves it
// on libkrun's TSI, where the guest's AF_INET calls are performed *on the host* and the
// guest's 127.0.0.1 is the host's. That is the opposite of containment, and it looks from the
// outside like a sandbox that is working. Nothing here can detect it directly — the only
// signal Boks has is that nothing ever connected to the link socket — which is why the
// supervisor treats "no peer" as a failure loud enough to stop trusting the sandbox, rather
// than as a quiet wait (see internal/enforce, linkWatchdog).
//
// # What protects the link socket here, which is not what protects it on Unix
//
// On Unix the socket sits in a directory this package creates 0700, and that mode is enforced
// by the kernel. On Windows the mode argument is ignored: MkdirAll's permission bits do
// nothing, and what actually keeps another local user out is the ACL inherited from the state
// directory's parent — `%LocalAppData%\boks` by default, inside the user's profile, which
// Windows protects from other standard users. That is a real control but it is a *different*
// control, and it moves with BOKS_STATE_DIR. Pointing that variable at a shared location on
// Windows leaves the link socket reachable by any local user, who could claim the link before
// the VM does and be handed the sandbox's egress. The same reasoning is why the supervisor's
// control socket is not bound here at all — see internal/enforce/control_windows.go, where the
// second-opinion credential check cannot be answered either.
//
// (Two things that are true and are *not* blockers, recorded so they are not rediscovered as
// such: the Host Compute Service, which drives Hyper-V containers and LCOW, really does expose
// no socket-backed NIC — its NetworkAdapter is an HNS endpoint id and nothing else. And
// Windows' AF_UNIX really does implement only SOCK_STREAM. A WHP VMM need not go near HCS, and
// SOCK_STREAM is precisely what `unixstream` wants.)
//
// See docs/windows.md for the state of the port, and docs/verification.md for what has
// actually been observed.

// Unexercised reports that nothing has ever been seen putting a guest's frames on this link.
//
// It is not a refusal and nothing consults it before binding: Start behaves identically on
// every platform. It exists so that the two places which can do something useful with the fact
// — the CLI, which warns before the sandbox is created, and the supervisor, which bounds its
// wait for a peer instead of idling forever — can say the same true thing without duplicating
// it.
func Unexercised() error { return unexercisedOnWindows() }
