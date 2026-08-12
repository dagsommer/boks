# Upstream: virtio-net on Windows (libkrun)

Boks enforces network policy at the virtio-net boundary. Upstream libkrun cannot provide
that boundary on Windows, which is why `boks run` cannot work there — see
`internal/network/gateway_windows.go`, which says so and fails early.

Not because it is impossible: a shipping product drives a virtio NIC on Windows through
libkrun's own ABI today (below). Because upstream has no virtio-net backend for it.

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
  `Cargo.toml` never declares. There is no Windows job in libkrun's CI and `main` does not
  compile for Windows at all — the WHP port is visibly a work in progress.
- **nerdbox on Windows from the tree we build from.** A Windows build of the shim exists in
  production (`containerd-shim-nerdbox-v1.exe` in the Docker Sandboxes install), so this is
  a question of availability rather than feasibility — but whether the upstream nerdbox we
  consume builds and runs on Windows has not been checked. That is the next thing to find
  out, and it is cheap to.
- **containerd on Windows** running Linux guests the way the Boks stack assumes.
- **Our own Windows path**: `gateway_windows.go`, a Windows counterpart to `stack_unix.go`,
  and the stream transport described above.

So the realistic reading is: this removes one blocker of several. Its value is that the
blocker is now a patch in front of libkrun's maintainers rather than an unasked question,
and that we know the layers underneath it are not the obstacle.

The comment in `gateway_windows.go` and `gateway_unix.go` saying "nerdbox does not support
Windows either" should be softened when someone next touches it — a shipping Windows shim
binary exists, so the accurate statement is that *we* have no Windows path, not that none
is possible.

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
