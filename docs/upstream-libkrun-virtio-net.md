# Upstream: virtio-net on Windows (libkrun)

Boks enforces network policy at the virtio-net boundary. Upstream libkrun cannot provide
that boundary on Windows, which is why `boks run` had nothing to attach to there.

> **Later.** `packaging/libkrun-windows/` now carries the backend described here, `krun.dll`
> links with `--features blk,net`, and `boks run` no longer refuses on Windows — it binds the
> link socket and attempts the sandbox, with a bounded wait that fails legibly if nothing
> connects (`internal/network/vmm_windows.go`, `internal/enforce/link.go`). **No frame has
> crossed the device on Windows even so.** The refusal was removed because refusing guarantees
> the question stays unanswered, not because it was answered.

Not because it is impossible: a shipping product drives a virtio NIC on Windows through
libkrun's own ABI today (below). Because upstream has no virtio-net backend for it.

This document records the upstream change that closes the libkrun half of that gap, and
what Boks had to do to meet it. The Boks half is done — the link is a stream on every
platform now — and untested against any VMM.

## Status

**Written, not submitted, never run.** A branch exists against
`github.com/containers/libkrun` main; nothing has been proposed to the maintainers and no
part of it has been executed on Windows.

"Never run" narrowed but did not change on 2026-08-14. The equivalent backend is carried in
this repository's own series ([`packaging/libkrun-windows`](../packaging/libkrun-windows/),
patches 0019–0025), and the `krun.dll` that booted a guest and ran a container that day was
built with all of it — so it compiles, links and loads on Windows. That run added no network
device, because nerdbox adds one only when asked and nothing asked. Not a frame, not a
`krun_add_net_unixstream`, has gone through this code on Windows.

## Why this was the blocker

libkrun's Windows Hypervisor Platform backend was merged upstream in May–June 2026 (PRs
#660, #665, #691, #692). virtio-fs, blk, console, balloon and rng were ported with it.
virtio-net was not, and virtio-net is the device Boks needs: TSI has no policy decision
point we control, so the NIC is the boundary.

## Evidence that the target is reachable

A Windows 11 machine running Docker Sandboxes was inspected read-only on 2026-08-12. What
it shows is that every layer below us already works on Windows:

- **virtio-net is running in production.** The guest's `/sys/bus/virtio/devices/*/modalias`
  entries decode to virtio device-id `1` (net), plus block ×4, virtio-fs ×4, balloon,
  console and vsock. A userspace VMM on Windows is emulating a virtio NIC today.
- **Through libkrun's ABI, with the shim we consume.** The install ships `sailor.dll` (a VMM
  exposing libkrun's ABI), **`containerd-shim-nerdbox-v1.exe`**, `mkfs.erofs.exe` and
  `mkfs.ext4.exe`. A working Windows build of the nerdbox shim exists and loads a VMM
  through the `krun.dll` ABI.
- **No kernel driver, no elevation.** No `.sys` file, no registered driver or service, and
  `sbx diagnose` passes eight user-level checks without admin rights.
- **No Hyper-V worker process** — no `vmwp.exe`, no `vmmem`; the shim process holds the VM.
  Consistent with WHP rather than Hyper-V managing the machine. *Inferred* — confirming the
  API calls would need ETW or a debugger, which was not done.

This is about feasibility, not about our code. It says the platform, the ABI and the shim
all support this shape of VM; it says nothing about whether the branch below works.

## The change

Four commits on a `windows-virtio-net` branch:

1. `utils/windows: mark the ntdll extern block unsafe` — the crate is on edition 2024, so
   the merged Windows code does not compile with a current toolchain at all.
2. `devices: enable the vm-memory rawfd feature only on Unix` — `rawfd` is a
   `compile_error!` on Windows targets; declaring it unconditionally made the devices crate
   unbuildable there.
3. `virtio/net: move the unixstream socket calls behind a platform module` — a no-op
   refactor on Unix that separates the frame protocol from the socket calls.
4. `virtio/net: add a Windows backend for unixstream` — the port itself.

The load-bearing constraint: **Windows has `AF_UNIX` only for `SOCK_STREAM`**, since
Windows 10 1803. There has never been a `SOCK_DGRAM` equivalent — the same reason
gvisor-tap-vsock's Windows stub is `unixgram_windows.go`. So `unixstream` is the backend
that can exist on Windows, and `unixgram` stays Unix-only.

Two Windows specifics shape the implementation: a `SOCKET` is not a waitable object, so
`WSAEventSelect` binds it to an event object that the libkrun poller can watch (and that
event must be acknowledged with `WSAEnumNetworkEvents` before it re-signals); and
`WSAEventSelect` forces the socket non-blocking, so the `MSG_WAITALL` receive that completes
a partial frame on Unix is emulated with `WSAPoll`.

## What Boks had to change — done, on the Linux path

**The protocol switch is the known one: vfkit/unixgram → qemu/unixstream.** It has been
made, on every platform rather than only on Windows, for the reason noted at the bottom of
this section: a transport that only Windows uses is a transport nothing tests.

- `internal/network/gateway.go` (was `gateway_unix.go`) listens on an `AF_UNIX`
  `SOCK_STREAM` socket instead of binding a datagram one, and libkrun connects to it — which
  is the direction `krun_add_net_unixstream` works in, confirmed against nerdbox 0.2.3's
  `internal/shim/task/networking.go`, where the `vfkit` flag is "the VFKIT magic sequence
  libkrun must send **after connecting to the socket**".
- `Plan.Annotations()` in `internal/network/network.go` emits `mode=unixstream`, and
  deliberately emits none of `vfkit`, `vnet_hdr` or `features`: the first two change what is
  on the wire, and the third would negotiate segmentation offload the stack has no reason to
  want.
- `internal/network/stack.go` (was `stack_unix.go`) passes `types.QemuProtocol` and has lost
  its `!windows` tag. `gateway_windows.go` is gone; the Windows refusal moved to
  `vmm_windows.go`, where it is about the missing VMM rather than the socket type, and it is
  raised from `Network.Start`.
- `internal/network/link.go` is new, and is the part that was not "one constant and a
  listener". A datagram carried its own length; on a stream the length in front of each frame
  is a number the peer writes, and `tap.Switch` acts on it twice before anything has checked
  it — it allocates a buffer of exactly that size, then reads six bytes of MAC out of the
  front of it. So a 4 GiB claim is a 4 GiB allocation, and a 4-byte claim panics inside the
  switch *while it holds `camLock`*, which deadlocks the deferred disconnect rather than
  crashing cleanly (observed). The wrapper bounds the length before the switch sees it,
  refuses one below an Ethernet header, and reports a failed write in a form the switch's
  ENOBUFS retry cannot match — retrying a partially written frame would desynchronise every
  frame after it.

**None of this has run against a real VMM**, on any platform. The framing, the reconnect
handling and the refusals are tested against `tap.Switch` itself and against a simulated
guest on a real socket; the datagram link they replace is the one a booted VM was seen to
use. That is the risk this change carries, and it is deliberate: the alternative was a
Windows-only transport that no test on this project would ever exercise.

## What this does *not* unblock

libkrun's virtio-net is necessary, not sufficient. Still outstanding for Windows:

- **The rest of libkrun's devices crate does not compile for Windows.** `vsock`,
  `file_traits`, `linux_errno` and `legacy/x86_64/serial` are Unix-only, and
  `fs/windows/passthrough.rs` references `windows-sys` `Wdk_*` features that its
  `Cargo.toml` never declares. There is no Windows job in libkrun's CI and `main` does not
  compile for Windows at all — the WHP port is visibly a work in progress.
- **nerdbox on Windows from the tree we build from.** A Windows build of the shim exists in
  production (`containerd-shim-nerdbox-v1.exe` in the Docker Sandboxes install), so this is
  a question of availability rather than feasibility — but whether the upstream nerdbox we
  consume builds and runs on Windows has not been checked. That is the next thing to find
  out, and it is cheap to.
- **containerd on Windows** running Linux guests the way the Boks stack assumes.
- **Our own Windows path**: nothing in `internal/network` any more — the stream transport is
  in, `stack.go` and `gateway.go` build for `windows/amd64`, and the only Windows-specific
  file left is `vmm_windows.go`, which since this was written holds a warning rather than a
  refusal. What is untested is everything: no frame has crossed this link on Windows.

So the realistic reading is: this removes one blocker of several. Its value is that the
blocker is now a patch in front of libkrun's maintainers rather than an unasked question,
and that we know the layers underneath it are not the obstacle.

(The comment that used to say "nerdbox does not support Windows either" is gone with those
files: a shipping Windows shim binary exists, so the accurate statement is that *we* have no
Windows VMM, not that none is possible.)

## Verification, honestly

The branch compiles. That is all.

- Linux: clippy (`-D warnings`) clean across the feature combinations CI checks and
  buildable, `cargo fmt --check` clean, no new test failures, and each commit builds on its
  own.
- Windows: `cargo check`/`cargo clippy --target x86_64-pc-windows-msvc` produce no errors
  or warnings for the net module — but only with a local scaffold that gates off the
  unrelated modules that do not build for Windows, and that scaffold is not part of the
  proposed change.
- Nobody has run a frame through it on Windows. The upstream PR asks explicitly for someone
  with Windows hardware to verify it.

The Docker Sandboxes findings above do not count as verification of this branch, and the PR
says so in as many words. They establish that a virtio NIC can be driven on Windows through
libkrun's ABI; our implementation of one remains unexecuted code.
