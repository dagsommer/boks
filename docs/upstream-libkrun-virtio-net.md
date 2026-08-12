# Upstream: virtio-net on Windows (libkrun)

Boks enforces network policy at the virtio-net boundary. On Windows that boundary does not
exist yet, which is the single reason `boks run` cannot work there — see
`internal/network/gateway_windows.go`, which says so and fails early.

This document records the upstream change that closes the libkrun half of that gap, and
what Boks would have to do once it lands.

## Status

**Written, not submitted, never run.** A branch exists against
`github.com/containers/libkrun` main; nothing has been proposed to the maintainers and no
part of it has been executed on Windows.

## Why this was the blocker

libkrun's Windows Hypervisor Platform backend was merged upstream in May–June 2026 (PRs
#660, #665, #691, #692). virtio-fs, blk, console, balloon and rng were ported with it.
virtio-net was not, and virtio-net is the device Boks needs: TSI has no policy decision
point we control, so the NIC is the boundary.

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

## What Boks would have to change

**The protocol switch is the known one: vfkit/unixgram → qemu/unixstream.**

- `internal/network/gateway_unix.go` uses `transport.ListenUnixgram` +
  `transport.AcceptVfkit`. The Windows path needs the stream equivalent — a listening
  `AF_UNIX` `SOCK_STREAM` socket, with gvisor-tap-vsock's `qemu` protocol framing: each
  frame prefixed by its length as a 4-byte big-endian `uint32`. That is exactly what the
  ported libkrun backend speaks.
- `Plan.Annotations()` in `internal/network/network.go` hardcodes `mode=unixgram`. It would
  become `mode=unixstream` on Windows, which in turn requires the shim to call
  `krun_add_net_unixstream()` rather than `krun_add_net_unixgram()`.
- `internal/network/gateway_windows.go` currently exists only to fail with a sentence. It
  would grow a real implementation, and `stack_unix.go` would need a Windows counterpart
  or a build-tag widening — the host stack itself has no Unix-specific dependency beyond
  the transport.

Note that the framing change is not Windows-only work we can hide: gvisor-tap-vsock
implements the `qemu` protocol on all platforms, so the stream path could be developed and
tested on Linux first, and only then pointed at Windows.

## What this does *not* unblock

libkrun's virtio-net is necessary, not sufficient. Still outstanding for Windows:

- **The rest of libkrun's devices crate does not compile for Windows.** `vsock`,
  `file_traits`, `linux_errno` and `legacy/x86_64/serial` are Unix-only, and
  `fs/windows/passthrough.rs` references `windows-sys` `Wdk_*` features that its
  `Cargo.toml` never declares. The WHP port is visibly a work in progress with no Windows
  CI behind it.
- **nerdbox has no Windows support**, and Boks reaches libkrun only through the shim.
- **containerd on Windows** does not run Linux guests the way the Boks stack assumes.

So the realistic reading is: this removes one blocker of several, and its main value right
now is that it puts the question in front of libkrun's maintainers rather than leaving it
unasked.

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
