# nerdbox's shim, patched to start on Windows

Boks' container runtime is [nerdbox](https://github.com/containerd/nerdbox), whose shim
binary is `containerd-shim-nerdbox-v1`. It compiles for Windows unpatched. It could not
**start** there: containerd's shim library stubs out the entire process-lifecycle layer for
that platform, so the binary exited before it did any work. `patches/` holds the changes that
get past that, and `.github/workflows/nerdbox-windows.yml` builds the result so the series
cannot rot unnoticed.

With the series applied, the shim starts, serves ttrpc over a named pipe, boots a microVM
through `krun.dll`, and runs a container in it. `ctr tasks start` on a real Windows 11 machine
on 2026-08-14 printed the container's stdout, the guest kernel's `uname -a` and a non-zero
`/proc/uptime`, in `t_boot=1.90127s` + `t_create=121.648ms`
([`docs/verification.md`](../../docs/verification.md)). That is `ctr` and this shim; it is not
`boks run`, which still stops earlier, at Boks' own network refusal.

**These patches are a staging area, not a fork**, exactly like `packaging/libkrun-windows/`.
The goal is to delete this directory: patch 0001 is someone else's unmerged upstream pull
request that we are carrying early, and patches 0002 through 0005 belong in nerdbox. Nothing
in Boks links against a patched nerdbox — the patches are compiled, never shipped.

Not every patch here is about Windows. Patch 0003 fixes a plain API drift against the libkrun
revision Boks pins and patch 0004 an error-path stall in the shim's IO teardown; both fail
identically on Linux — as does the `io.go` half of patch 0005, which is the only part of that
patch outside a `//go:build windows` file. Patch 0005 is otherwise Windows-only, and is the
one patch here found by running the thing rather than by reading it.

They live here because this was the only place Boks built nerdbox from source. It no longer
is: [`packaging/linux/`](../linux/) builds the same pinned revision **unpatched**, for
amd64 and arm64, and publishes it. That directory deliberately does not apply this series, and
patch 0003 is the reason it can get away with it — 0003 exists because libkrun `de84d01`
removed the implicit vsock device, and that commit is in the 2.x revision this series targets
but not in the 1.x revision Linux pins. If the Linux pin ever moves onto 2.x, 0003 stops being
a Windows concern and becomes a prerequisite there too.

The revision they apply to is `packaging/nerdbox/NERDBOX_REV`. There is no second pin file
here on purpose: the guest kernel, the rootfs, and the shim that boots them all have to come
from the same nerdbox commit, and a SHA that lives in two files eventually disagrees with
itself.

## The failure this fixes

Measured on a real Windows 11 machine on 2026-08-14, with an unpatched
`containerd-shim-nerdbox-v1.exe` built from the pinned revision:

| Invocation | Result |
| --- | --- |
| `-v` | exit 0, prints the version |
| `-info` | exit 0, prints the info protobuf — `io.containerd.nerdbox.v1`, `containerd.io/runtime-allow-mounts`, mounts `mkdir/*,format/*,erofs` |
| `-namespace default -id test start` | **exit 1 in 118 ms, stderr `io.containerd.nerdbox.v1: not implemented`** |

`vendor/github.com/containerd/containerd/v2/pkg/shim/shim_windows.go` stubs six functions with
`errdefs.ErrNotImplemented`: `setupSignals`, `newServer`, `subreaper`, `serveListener`, `reap`
and `openLog`.

The one that actually fires is **`setupSignals`, called at `pkg/shim/shim.go:228`**. `run()`
calls it before the `switch action`, so it is reached by every real action — `start`, `delete`,
and the argument-less long-running server alike — and none of them get as far as their own
code. `-v` and `-info` work only because `run()` returns at lines 215 and 219 respectively,
above that call. The other five stubs are unreachable until `setupSignals` returns; they are
the next five walls rather than the current one.

That is also why the failure is so fast and so uninformative. 118 ms is the process starting,
parsing flags, and dying; `not implemented` is `errdefs.ErrNotImplemented`'s `Error()` string
rendered by `RunShim`'s `fmt.Fprintf(os.Stderr, "%s: %s", shim.Name(), err)`, with no
indication of which of the six it came from.

## What is in `patches/`

Five patches, generated with `git format-patch` from a working branch off the pinned
revision.

### `0001-vendor-implement-the-Windows-shim-server-*`

A hand-port of **[containerd/containerd#13948](https://github.com/containerd/containerd/pull/13948)**,
"[pkg/shim] implement Windows support for the shim server", by `rawahars`, opened 2026-08-12,
into nerdbox's `vendor/` tree. It implements all six stubs — `setupSignals` and
`handleExitSignals` over `signal.Notify`, `newServer` as a plain `ttrpc.NewServer` (Windows has
no peer-credential handshaker), `subreaper` and `setupDumpStacks` as no-ops (no subreaping, no
`SIGUSR1`), `serveListener` over `winio.ListenPipe`, `reap` as a signal-drain loop (Windows has
no `SIGCHLD`), and `openLog` as a named pipe with a reconnecting writer in front of it.

**#13948 is open and unmerged, and it has never been run against nerdbox by anyone**,
including us. We are carrying an unmerged third-party pull request because the alternative is
writing the same six functions ourselves and diverging from what will land. When it merges and
nerdbox's containerd dependency moves past it, delete this patch — do not maintain it.

It is a hand-port rather than a `git apply` of `gh pr diff 13948`. The PR is written against
containerd `main`, whose copy of `shim_windows.go` has already drifted from the v2.3.3 one
nerdbox vendors: `main` imports `golang.org/x/sys/windows` and carries a different
`awaitPipeReady`. The function bodies here are the PR's verbatim; only the import block and the
placement around nerdbox's existing `awaitPipeReady` differ. The PR's `shim_windows_test.go` —
the larger half of its diff — is **not** carried, because `go mod vendor` strips `_test.go`
files and it could never run from `vendor/`. That is a real loss: the only tests for this code
live in a file we cannot execute.

#### Things in #13948 that look wrong for our use

Flagged rather than smoothed over. None of these are fixed by our patches.

- **`openLog` returns a writer that silently discards.** `reconnectingLogWriter.Write` returns
  `len(p), nil` when no reader is attached, and again when a write to the pipe fails. Every
  shim log line emitted before containerd connects to `\\.\pipe\containerd-shim-<ns>-<id>-log`
  is gone. For bringing up a shim that has never started, the early lines are exactly the ones
  worth having. nerdbox's shim sets `NoSetupLogger`, so `openLog` is not on the `main()` path
  today — but it is one config flag away from being the reason a boot failure has no logs.
- **`reap` never returns while the process lives.** It loops on `signals` and only returns on
  `ctx.Done()`. `serve()` ends with `return reap(...)`, so shutdown depends entirely on
  `handleExitSignals` cancelling the context. The two functions both `signal.Notify` on
  `os.Interrupt`/`SIGTERM` with separate channels, so a single Ctrl+C is delivered to both;
  that is benign, but it means `reap`'s "received signal" log line and the actual shutdown are
  racing to the log.
- **`serveListener` rejects an empty path, and `setupPprof` calls it with one.** With `-debug`
  and no `-debug-socket`, `setupPprof` gets `named pipe path is required on Windows` back. It
  is logged as a warning, not fatal, so it degrades rather than breaks — but debug runs will
  carry a confusing warning that has nothing to do with the failure being debugged.
- **`winio.ListenPipe(path, nil)` passes no `PipeConfig`,** so the pipe gets the default
  security descriptor rather than an explicit one. On Unix the shim socket's access control is
  the filesystem's; here it is whatever go-winio's default is. Not our call to make, but worth
  knowing before this is treated as a security boundary.

### `0002-fix-shim-pass-the-shim-s-pipe-address-as-socket-on-W`

A nerdbox bug, independent of #13948 and present before it.

`pkg/shim/manager/manager_windows.go`'s `Start` computed the child shim's named pipe address
and passed it down as a `TTRPC_SOCKET=` environment variable. **Nothing reads it.** Grepping
the whole nerdbox tree including vendored containerd finds exactly two hits, both the write
itself; containerd's shim knows only `TTRPC_ADDRESS`, `GRPC_ADDRESS`, `NAMESPACE` and
`MAX_SHIM_VERSION` (`pkg/shim/shim.go:119-122`). The child therefore started with `socketFlag`
empty.

Unix survives the same omission because `manager_unix.go` binds the socket in the parent and
passes it to the child as `cmd.ExtraFiles` fd 3, which is the `3` in
`serveListener(socketFlag, 3)`. Windows named pipes cannot be inherited as descriptors, so the
address has to travel on the command line.

The flag is `-socket`, verified against the vendored source rather than assumed:
`pkg/shim/shim.go:138` binds `flag.StringVar(&socketFlag, "socket", "", "socket path to
serve")`, and `serve()` passes that variable to `serveListener` at `pkg/shim/shim.go:469`. The
patch adds `"-socket", address` to `newCommand`'s args and moves the `shimPipeAddress` call
above `newCommand`, since the address is now an argument rather than something appended to the
environment afterwards.

Without this, patch 0001 turns a silent misconfiguration into a hard error: its `serveListener`
rejects an empty path outright, and rejects anything not prefixed `\\.\pipe\`. `shimPipeAddress`
already produces `\\.\pipe\containerd-shim-<sha256[:16]>`, so passing it as `-socket` is all
that was missing.

This one is a straightforward nerdbox bug report and should go upstream to
`containerd/nerdbox` on its own, without waiting for #13948.

### `0003-fix-libkrun-create-the-vsock-device-explicitly-befor`

Not a Windows bug. nerdbox v0.2.3 builds libkrun `v1.19.0` (its `Dockerfile`, `ARG
LIBKRUN_VERSION`); Boks pins `07fd40d`. Those are not the same lineage — `v1.19.0` is a tag on
the `stable-1.19.x` branch, which forked from `main` at `v1.18.0` on 2026-04-24, so the gap is
everything `main` did afterwards. `main` spent that time making implicitly-created devices
explicit, and nerdbox is written against the implicit versions.

libkrun `de84d01`, "lib: remove `krun_disable_implicit_vsock` and implicit vsock creation",
deleted the `Implicit` variant of `VsockConfig` and made `Disabled` the default. So:

```
krun_add_vsock_port2(ctx, 1025, ".../run_vminitd.sock", false) -> -19  (-ENODEV)
krun_add_vsock_port2(ctx, 1026, ".../streaming.sock",   true)  -> -19  (-ENODEV)
```

1025 is the ttrpc control plane and 1026 the streaming channel. Without them there is no
sandbox. The patch calls `krun_add_vsock(ctx, 0)` in `newvmcontext`, immediately after
`krun_create_ctx`, so the device exists before any port is mapped onto it.

`tsi_features = 0` is an empty `TsiFlags` — a plain guest-IPC vsock with no TSI hijacking,
which is what nerdbox wants, since it does its networking with a virtio-net device added
through `krun_add_net_unix{stream,gram}` and never calls `krun_set_port_map` or
`krun_set_passt_fd`. That is precisely what the deleted heuristic decided for this
configuration: a non-empty net list took the `enable_tsi = false` branch, yielding empty TSI
flags and no host port map. The `VsockDeviceConfig` libkrun ends up building is unchanged; only
the way it is asked for has changed.

**`-EEXIST` is accepted as success**, which is what makes this patch safe to send upstream. On
libkrun ≤ 1.19.x the implicit device is still there, `VsockConfig` is `Implicit` rather than
`Disabled`, and `krun_add_vsock` answers `-EEXIST` on the very first call. Both answers leave
the context holding the device nerdbox needs, so one source tree works against libkrun on
either side of `de84d01` — nerdbox does not have to move its own pin in lockstep with ours.

This adds a **20th** bound symbol. nerdbox's loader reflects over the whole binding struct and
resolves every `C:"krun_..."` tag eagerly at `dlopen`, so `krun_add_vsock` must now be exported
or nothing loads at all. It has been exported since long before 1.19.0, and
`libkrun-windows.yml` asserts it alongside the other nineteen.

Upstreamable to `containerd/nerdbox` on its own. It is strictly more correct against every
libkrun, not a workaround for our pin.

### `0004-fix-shim-stop-draining-IO-that-a-failed-create-can-n`

Also not a Windows bug — the code has no platform condition in it — but found on Windows, on
2026-08-14, when a container whose spec `crun` refused took a further **30.0 s** to say so:

```
ERRO ... failed to create task
ERRO ... failed to shutdown container after create failure: io shutdown: context deadline exceeded
```

Thirty seconds apart, at 0–3% CPU. Everything worth reporting was known at the first line.

The wait is the shutdown function `forwardIO` returns (`internal/shim/task/io.go`). It waits
for the host-side copy goroutines to drain into the destination FIFO and for stdin to reach a
real EOF, and when the caller's context carries no deadline of its own it grants that wait 30 s.
For a process that ran, that is right: `ioDone` is the only thing standing between the last byte
of output and the `Delete` that discards it.

For a process that never started it cannot succeed. `ioDone` closes when both the stdout and
stderr copies return (`io_copystreams_*.go`), and those are `io.CopyBuffer` calls on a stream
whose guest end was never attached to anything — so nothing will write to them and nothing will
close them **except the force-close the shutdown performs after the wait it is doing**. The
stdin side is the same: the client still holds its write end open, so `stdinDone` will not fire
either. Both waits share one context, which is why the stall is exactly the ceiling and not
twice it.

`0004` gives the five teardowns that can only be reached by a process that never started —
`bindSockets` failed, bad task options, the guest's `Create` failed, socket forwarding failed to
start, `Exec` found no container — a one-second budget instead. A second rather than nothing, so
a stdin copy caught mid-write has a moment to finish; after it the streams close and the
goroutines unwind exactly as they do today. Every path where a process actually ran — `Delete`,
`Wait`, the service's own shutdown callback — keeps the full budget.

**Not executed.** The stall was measured on hardware; the fix is compiled for `windows` and
`linux` and reasoned from the code above. No VM has been booted with it.

Upstreamable to `containerd/nerdbox` on its own, and independent of the other four.

### `0005-fix-shim-end-the-Windows-stdin-copy-no-client-will-e`

The same 30 seconds, on the other side of the success/failure line — and this one is
Windows-only. Measured on 2026-08-14, on the run that first carried a container end to end:

```
12:47:08.080  reaped child process
12:47:38.089  ERRO ... failed to shutdown io after delete: io shutdown: context deadline exceeded
```

30.009 s, with the container's output already on the console. `0004` does not help: that patch
shortens teardown for a process that *never started*, and this process ran, exited and drained.
`Delete` legitimately keeps the full budget.

What does not end is the **stdin** half. `forwardIO`'s shutdown waits for `ioDone` (the output
copies draining) and then `stdinDone` (the stdin copy reaching EOF and delivering its in-band
`CloseWrite`). `ioDone` is fine here — the guest closes stdout and stderr when the process
exits, which is exactly what a failed create could not do, and the `Wait` handler had already
blocked on it and returned. `stdinDone` is not: on Windows the stdin copy is an
`io.CopyBuffer` reading a **named-pipe connection**, and a named pipe ends only when a peer
disconnects. Neither peer does.

- The shim's `stdinEOF` only closed a `closeRequested` channel, which is examined *between*
  copies and while re-dialing after a detach — never during a read, which is where the
  goroutine actually is.
- containerd's own client never disconnects either. `pkg/cio/io_windows.go`'s `copyIO` accepts
  the stdin connection inside a goroutine and appends only the **listener** to the cio's
  closers, so `Close()` closes the listener and leaves the connection open; the Windows cio
  sets no `cancel` at all, so the `io.Cancel()` that `task.Delete` calls before the RPC does
  nothing. The connection stays open, held by a goroutine copying an `os.Stdin` that a console
  never EOFs.

The asymmetry is the whole explanation for why Linux does not have this bug: there, cio's
`cancel` closes the client's own `O_WRONLY` FIFO handle and the shim drops its matching
reference in `stdinEOF`, so the FIFO reaches a real EOF. On Windows there is no second
reference to drop and no client close to wait for, so the wait can only expire.

So `stdinEOF` now disconnects the client, closing the connection the copy is reading. The
cause, not the symptom: shortening the ceiling would leave a stream that never closes, and
would shorten the output drain too, which is a wait worth its full budget.

**It cannot truncate stdin.** The copy reads in a tight loop, so it is blocked in a read only
when the pipe's buffer is empty — every byte written before EOF was requested has already been
relayed. A byte racing the close would be one written after `CloseIO`, which the client has by
then promised not to send. Detach and re-attach are untouched: a client that disconnects
*without* `CloseIO` is still a detach, still delivers no EOF, and the goroutine still waits
indefinitely for a new client on the same pipe path.

Alongside it, `io.go` now names **which** of the two waits expired — `io shutdown: output
drain: …` or `io shutdown: stdin drain: …`. They share one deadline, so the bare
`context deadline exceeded` above does not distinguish "the guest never closed the output
streams" from "the stdin copy never ended", and telling them apart is otherwise a VM run away.
If the fix is right the line does not appear at all; if the reading of the mechanism is wrong,
the next run says which half in one word.

`TestCopyStreamsStdinEOFWithClientAttached` covers it without a VM — a client that writes and
then holds its connection open forever, as containerd's does, and a `stdinDone` that must close
anyway — and runs in the existing `windows-latest` unit-test job.

**Not executed.** The stall was measured on hardware; the mechanism is read from nerdbox's and
containerd's own sources and the fix compiles for `windows/amd64`, `windows/arm64`,
`linux/amd64` and `linux/arm64`. **The new test has never run**: this repository's development
machines cannot execute Windows binaries, and it is a Windows-only test.

Upstreamable to `containerd/nerdbox` on its own. Send it with the two log lines, the timestamps
and the `pkg/cio/io_windows.go` reading.

## The version skew, audited once instead of one boot at a time

Patches 0035 (libkrun, `krun_set_exec`) and 0003 (nerdbox, vsock) are the same bug twice.
Each surfaced only after the previous call started succeeding, which is the worst possible
discovery order: one reboot per defect, on hardware, with a two-month API gap left to go.

The gap is structural. nerdbox v0.2.3 builds libkrun `v1.19.0`; Boks pins `07fd40d`. Those are
**not the same lineage** — `v1.19.0` is a tag on `stable-1.19.x`, which forked from `main` at
`v1.18.0` (`8018a20`, 2026-04-24). `git log v1.19.0..07fd40d` is 230 commits, and `main` spent
them turning implicitly-created devices into explicitly-requested ones and deleting deprecated
entry points. nerdbox is written against the implicit side of every one of those changes.

So rather than wait for the next boot, here is every symbol nerdbox binds, diffed for
**implementation and precondition** across the two revisions. "Never called" matters as much as
"changed": nerdbox's loader resolves the whole binding table eagerly, so a symbol it never
invokes still has to exist, and a symbol it does invoke can break without moving.

| # | symbol | what changed between `v1.19.0` and `07fd40d` | nerdbox | failure shape |
| --- | --- | --- | --- | --- |
| 1 | `krun_set_log_level` | **removed** (`df19d2a`) | bound, never called | load fails → no VM. Fixed: libkrun 0014 re-exports it as `-ENOTSUP` |
| 2 | `krun_init_log` | `RawFd` → `LogTarget`; a HANDLE on Windows (`85991bd` + our 0011) | called once | none — nerdbox passes `os.Stderr.Fd()`, which *is* a HANDLE on Windows and fd 2 on Unix |
| 3 | `krun_create_ctx` | libkrunfw now loaded lazily (`3899a78`) | called | none — nerdbox boots an external kernel and never touches libkrunfw |
| 4 | `krun_free_ctx` | — | called | none |
| 5 | `krun_set_vm_config` | — | called | none |
| 6 | `krun_set_kernel` | cmdline prolog no longer carries `init=` (`502116e`) | called | none — nerdbox passes its own full cmdline, which overrides the prolog |
| 7 | `krun_set_exec` | **stubbed `-ENOTSUP`** for non-nitro (`4d2201e`) | called | no workload. **Was** the blocker; fixed by libkrun 0035 |
| 8 | `krun_set_console_output` | **removed** for non-nitro, and the implicit console with it (`ce4146d`) | called unconditionally | load fails, and no `hvc0` for `console=hvc0`. Fixed by libkrun 0026 |
| 9 | `krun_start_enter` | `VsockConfig::Implicit` branch, implicit console and init injection all deleted | called | see rows 8, 10, 12 |
| 10 | `krun_add_vsock_port2` | **now requires `krun_add_vsock` first** (`de84d01`) | called ×2 (1025, 1026) | `-ENODEV` on both → no ttrpc, no streaming, no sandbox. **Was the current blocker**; fixed by nerdbox 0003 |
| 11 | `krun_add_vsock` | unchanged, but its precondition inverted | **newly bound** by 0003 | `-EEXIST` on ≤ 1.19.x, which 0003 accepts |
| 12 | `krun_add_virtiofs3` | no longer injects `/init.krun` under the `/dev/root` tag (`502116e`) | called per share | **none, checked** — nerdbox boots `init=/sbin/vminitd` from its own erofs root on `/dev/vda` and never tags a share `/dev/root` |
| 13 | `krun_get_shutdown_eventfd` | `-ENOTSUP` on Windows (our 0012: an `int32_t` cannot hold a HANDLE) | bound, never called | none |
| 14 | `krun_set_gpu_options` | — | bound, never called | none |
| 15 | `krun_set_gvproxy_path` | **removed** (`decdbca`) | bound, never called | load fails. Fixed: libkrun 0014 re-exports as `-ENOTSUP` |
| 16 | `krun_set_net_mac` | **removed** (`decdbca`) | bound, never called | as above |
| 17 | `krun_add_disk` | — | bound, never called (`AddDisk2` is used) | none |
| 18 | `krun_add_disk2` | `block_id` now surfaces as the virtio-blk serial (`df85b8b`) | called ×N | none — cosmetic in-guest; `KRUN_DISK_FORMAT_*` values are unchanged |
| 19 | `krun_add_net_unixstream` | Windows takes the path form only, never an fd (our 0021) | called in `unixstream` mode | none — nerdbox always passes `fd = -1` |
| 20 | `krun_add_net_unixgram` | Unix-only upstream; Windows stub returns `-ENOTSUP` (our 0020) | called in `unixgram` mode | would fail on Windows — Boks configures `mode=unixstream` |

Also removed on `main` but **not bound** by nerdbox, so inert here: `krun_set_root`,
`krun_set_root_disk`, `krun_set_data_disk`, `krun_set_mapped_volumes`, `krun_set_passt_fd`,
`krun_disable_implicit_vsock`, `krun_disable_implicit_console`.

Two things the audit found that are **not** version skew, recorded so they are not rediscovered
as if they were:

- nerdbox's `NET_FLAG_INCLUDE_VNET_HEADER` is `1 << 1`, but libkrun's bit 1 is
  `NET_FLAG_DHCP_CLIENT` — the same value at `v1.19.0` and at our pin. Setting `vnet_hdr=` in a
  network config therefore turns on libkrun's DHCP client. Boks sets neither, so `flags` is 0.
- `krun_add_net_unixstream` accepts only `NET_FLAG_DHCP_CLIENT` and returns `-EINVAL` for
  `NET_FLAG_VFKIT`, which only `krun_add_net_unixgram` honours. Again identical in both
  revisions. A `vfkit=true` network in `unixstream` mode would be rejected.

### What to expect next

The audit predicts **no further libkrun API break** for this call sequence: every one of the 20
is now either unchanged, never called, or explicitly handled. That is a claim about
configuration calls returning 0, and it is the only kind of claim any of this evidence
supports.

What it did not cover — and where the next failure was therefore to be looked for first — was
everything after `krun_start_enter` returns, because the audit's method (diff the entry point,
diff its preconditions) stops at the library boundary. That prediction held on both counts. No
further libkrun API break occurred; every failure after it was past the boundary, and the
console was the first of them: `ce4146d` had deleted the implicit console *device
construction* in `builder.rs`, our 0026 rebuilt one from `VirtioConsoleConfigMode::Output`,
and whether it would enumerate as `hvc0` was verified by construction and not by observation.
It does — the console baseline in `docs/verification.md` records `activate event`, `Device is
ready`, `Port ready 0` and `Starting port io for port 0`, once each in order, handshake in
11 ms.

The stall patch 0005 fixes is the same shape one layer up: not an API that moved, but a
behaviour nothing had ever executed.

`GOOS=windows GOARCH=amd64 go build ./cmd/containerd-shim-nerdbox-v1`, from a fresh checkout of
the pinned revision with all five patches applied, produces an **18,663,936-byte PE
executable**. `GOOS=windows GOARCH=arm64` also builds. `GOOS=linux` still builds for both
`amd64` and `arm64`, so nothing here regressed the platform that works.

Patch 0003 had, before any of this ran, one piece of evidence the others did not, because
unlike them it is testable without a VM. `krun_create_ctx`, `krun_add_vsock` and
`krun_add_vsock_port2` only mutate a
configuration struct on the host, so a small C program linked against the built `libkrun.so`
can call them in nerdbox's order and read the return codes. Against `libkrun.so` built from
our series on `aarch64-unknown-linux-gnu`:

| call | returns |
| --- | --- |
| `krun_add_vsock_port2(ctx, 1025, …, false)` with no prior `krun_add_vsock` | `-19` (`-ENODEV`) |
| `krun_add_vsock(ctx, 0)` | `0` |
| `krun_add_vsock_port2(ctx, 1025, …, false)` after it | `0` |
| `krun_add_vsock_port2(ctx, 1026, …, true)` after it | `0` |
| `krun_add_vsock(ctx, 0)` a second time | `-17` (`-EEXIST`) |

The first row is the failure the patch exists to fix, reproduced away from any hardware; the
last row is the value the patch tolerates, confirming `EEXIST` is 17 rather than assuming it.

### What has actually been run

The shim **has** now been started on Windows, under a real containerd, and has run a container.
On 2026-08-14, on a Windows 11 machine, `ctr tasks start` through
`io.containerd.nerdbox.v1` produced:

```
HELLO-FROM-THE-VM
Linux (none) 6.12.44 #1 SMP Thu Aug 13 14:58:57 UTC 2026 x86_64 Linux
1.22 0.56
```

`uname` names the guest kernel this project builds rather than the Windows host, so the process
ran in the microVM and not beside it. `t_boot=1.90127s`, `t_create=121.648ms`. Six stubs are
implemented and five of them — `setupSignals`, `newServer`, `serveListener`, `subreaper`,
`reap` — have now executed; `openLog` remains unexercised, since nerdbox's shim sets
`NoSetupLogger`. Full evidence and its limits: [`docs/verification.md`](../../docs/verification.md).

The limits are worth stating in the same breath. This is `ctr`, not `boks run`; Boks' own path
still stops earlier, at the network refusal in `internal/network/vmm_windows.go`. No network
device has been added on Windows and no Ethernet frame has crossed one. The runs to date used
an elevated containerd, for its task-bundle symlink — `packaging/containerd-windows/patches/0006`
replaces that with a junction and has not been run — and they used no NIC, one container at a
time, and a hand-made writable layer, since `mkfs.ext4` still does not exist for Windows.

The build claim and the run claim remain separate: compiling a shim is not starting one, and
patch 0005 exists because a defect survived every compile and every review in this file until
something ran.

## Working on these patches

Reproduce what CI does:

```sh
git clone https://github.com/containerd/nerdbox
cd nerdbox
git checkout "$(grep -v '^[[:space:]]*#' ../boks/packaging/nerdbox/NERDBOX_REV | tr -d '[:space:]')"
git apply ../boks/packaging/nerdbox-windows/patches/*.patch
GOOS=windows GOARCH=amd64 go build ./cmd/containerd-shim-nerdbox-v1
```

CI uses `git apply` rather than `git am` for the same reason `libkrun-windows.yml` does: `am`
writes commits, and a commit needs a committer identity that a fresh runner does not have — it
fails with `empty ident name` before it ever reads the patch.

### Adding or amending a patch

Work on a branch off the pinned revision, then regenerate the whole series so the files stay a
faithful `format-patch` of real commits:

```sh
rm -f packaging/nerdbox-windows/patches/*.patch
git -C /path/to/nerdbox format-patch --no-signature \
    -o /path/to/boks/packaging/nerdbox-windows/patches "$NERDBOX_REV..HEAD"
```

Do not hand-edit a `.patch` file. The series is regenerable by construction, and that is what
makes it possible to check that it still reproduces the branch it came from.

### Moving the pin

`packaging/nerdbox/NERDBOX_REV` is shared with the guest-image build, so moving it moves both.
Bump the file, rebase the branch onto the new revision, regenerate the series, and let the
workflow tell you what changed. Expect patch 0001 to drop out entirely once #13948 lands and
nerdbox revendors — that is the outcome this directory is aiming for.

## Testing the result on Windows

The build still proves nothing about runtime, and a green CI here means the series compiles,
not that it works. To check a build on a Windows machine with the patched
`containerd-shim-nerdbox-v1.exe`:

```powershell
# 1. Baseline — these worked before the patches and must still work.
.\containerd-shim-nerdbox-v1.exe -v
.\containerd-shim-nerdbox-v1.exe -info

# 2. The invocation that used to fail in 118 ms.
.\containerd-shim-nerdbox-v1.exe -namespace default -id test start
echo "exit=$LASTEXITCODE"
```

What to look for, per #13948's design:

- **It must no longer print `io.containerd.nerdbox.v1: not implemented`.** Any other error is
  progress; that exact string means the patch did not take.
- `start` should **spawn a child process** — the long-running shim — and that child should
  serve a named pipe. Check with
  `[System.IO.Directory]::GetFiles("\\.\pipe\") | Select-String containerd-shim` and expect a
  `containerd-shim-<32 hex chars>` entry.
- `start` should **print the bootstrap params protobuf to stdout** and exit 0. It is binary, so
  redirect it: `... start > boot.bin` and check the file is non-empty and contains the pipe
  address as a substring.
- Expect a **10-second hang followed by a failure** if the child dies on startup:
  `waitForShimPipe` polls for the pipe with a 10 s budget. If you get that, the child is
  crashing — run the same argument vector the parent uses
  (`-namespace default -id test -address <containerd grpc address> -socket \\.\pipe\test-1`,
  with no trailing action word) directly and see what it says.

Under a real containerd, the equivalent is `ctr --namespace default run --runtime
io.containerd.nerdbox.v1 ...`; the shim log is a named pipe at
`\\.\pipe\containerd-shim-<namespace>-<id>-log`, and per the caveat above it drops everything
written before a reader attaches.

**That run has its own document.** It needs `krun.dll`, `nerdbox-kernel-x86_64` and
`nerdbox-rootfs.erofs` on *containerd's* `PATH` — not your shell's, since the shim inherits the
daemon's environment — an EROFS snapshotter, a Linux OCI spec that `ctr` on Windows will not
generate unaided, and a writable layer Windows cannot format. See
[`docs/windows-e2e.md`](../../docs/windows-e2e.md) for the full procedure, what each step proves,
and the ranked list of ways it is expected to fail.

## Upstreaming

- Patch 0001: nothing to do but wait for
  [containerd/containerd#13948](https://github.com/containerd/containerd/pull/13948). We have
  now run it — under containerd, serving ttrpc, through a container's whole lifecycle — which
  as far as we know no one else has, so anything we find in it belongs in that PR's review
  thread. Nothing in it has needed fixing so far.
- Patch 0002: an independent bug report against `containerd/nerdbox`. Small, self-contained,
  and does not depend on #13948 landing.
- Patch 0003: likewise independent, and the easiest of the five to argue for — it adopts an
  API change upstream libkrun made deliberately, and because it treats `-EEXIST` as success it
  does not require nerdbox to move its own `LIBKRUN_VERSION` first. Send it with the `-ENODEV`
  return codes above as the reproduction.
- Patch 0004: independent of the rest, and of Windows. Send it with the two log lines and
  their timestamps as the reproduction; the mechanism is entirely in nerdbox's own
  `internal/shim/task`, so there is nothing about our pins to explain first.
- Patch 0005: independent of the rest, and the strongest case of the five — it is a bug
  observed in a normal successful container lifecycle on the platform nerdbox already claims to
  build for, with a reproduction that is two log lines and a test that does not need a VM. Send
  it with the `pkg/cio/io_windows.go` reading, since the fix only makes sense once it is clear
  that containerd's own client never closes that connection.
