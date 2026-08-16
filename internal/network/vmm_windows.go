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
// Everything below Boks worked there. So Boks tried.
//
// # The frame path, which this file said for a week had never carried a frame
//
// It has. That claim is retired, and the two runs that retired it are worth naming precisely,
// because a sentence this load-bearing should not be withdrawn on a vague impression:
//
//   - **2026-08-14.** `boks run --net nat shell <workspace> -- uname -a` on Windows 11
//     hardware, exit 0. The stack log recorded `network: the guest attached to the link
//     socket …\boks\net\shell-boks\net.sock` — first contact on the frame path — and the
//     policy engine judged real traffic rather than merely accepting an attach: an allowed
//     destination completed a TCP connection to a resolved GitHub address, a denied one was
//     refused at CONNECT, three `policy-log.jsonl` records with `stage:connect` and
//     `stage:dial`.
//   - **2026-08-15.** The same probe returned **HTTP 200 from github.com**, fetched by a
//     Linux container in a microVM on Windows through Boks' own gvisor stack, while the
//     denied host still failed at `CONNECT tunnel failed, response 403` — the policy refusing
//     it rather than the guest's clock. Every step ran from an unelevated shell.
//
// So Unexercised below returns nil here, as it does everywhere else. Nothing warns and nothing
// bounds a wait, because there is no longer a platform whose link has not been shown to carry
// frames. The mechanism is kept rather than deleted — see the doc comment on Unexercised for
// what it is still for.
//
// # The failure that looks like success, which is not retired
//
// A shim that ignores the network annotations does not leave the guest with no network: it
// leaves it on libkrun's TSI, where the guest's AF_INET calls are performed *on the host* and
// the guest's 127.0.0.1 is the host's. That is the opposite of containment, and from outside
// it looks like a sandbox that is working. Nothing here can detect it directly; the only
// signal Boks has is that nothing ever connected to the link socket. What has changed is that
// this is now a misconfiguration — the wrong shim build, or a krun.dll built without
// `--features blk,net` — rather than the expected outcome of the first attempt. `boks doctor`
// reports where the shim will find krun.dll, and internal/enforce's noPeerError names the
// three things to check.
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

// Unexercised reports whether anything on this platform has been observed putting a guest's
// frames on the link this package binds.
//
// It is nil here now, for the same reason it is nil on Unix: a real guest has been watched
// across this link on Windows, with the policy engine allowing one destination and refusing
// another, on 2026-08-14 and again on 2026-08-15 (docs/verification.md). Until 2026-08-14 this
// returned a sentence saying no Ethernet frame had ever crossed libkrun's virtio-net device
// here, and that sentence was printed as an unsuppressable WARNING before every `--net nat`
// run on Windows. Leaving it in place after the runs above would have been the same kind of
// error in the other direction.
//
// The function stays rather than being deleted, and so do its two consumers — the CLI, which
// warns before the sandbox is created, and the supervisor, which bounds its wait for a peer
// instead of idling forever (internal/enforce, linkWatchdog). Both take the answer as an
// argument rather than reading it here, so both remain tested against a non-nil answer that a
// test constructs. This is the one place that decides whether any platform is in that state,
// and today none is.
func Unexercised() error { return nil }
