# nerdbox's shim, patched to start on Windows

Boks' container runtime is [nerdbox](https://github.com/containerd/nerdbox), whose shim
binary is `containerd-shim-nerdbox-v1`. It compiles for Windows today. It cannot **start** on
Windows: containerd's shim library stubs out the entire process-lifecycle layer for that
platform, so the binary exits before it does any work. `patches/` holds the changes that get
past that, and `.github/workflows/nerdbox-windows.yml` builds the result so the series cannot
rot unnoticed.

**These patches are a staging area, not a fork**, exactly like `packaging/libkrun-windows/`.
The goal is to delete this directory: patch 0001 is someone else's unmerged upstream pull
request that we are carrying early, and patches 0002 and 0003 belong in nerdbox. Nothing in
Boks links against a patched nerdbox — the patches are compiled, never shipped.

Not every patch here is about Windows. Patch 0003 fixes a plain API drift against the libkrun
revision Boks pins, and it fails identically on Linux; it lives here because this is the only
place Boks builds nerdbox from source.

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

Three patches, generated with `git format-patch` from a working branch off the pinned
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

What it does not cover — and where the next failure should therefore be looked for first — is
everything after `krun_start_enter` returns, because the audit's method (diff the entry point,
diff its preconditions) stops at the library boundary. The removals on `main` were not only API
deletions; `ce4146d` also deleted the implicit console *device construction* in `builder.rs`,
and `502116e` deleted init injection. Our 0026 rebuilds a console from
`VirtioConsoleConfigMode::Output`, but whether that device enumerates as `hvc0` — the console
nerdbox's cmdline names, and the only channel through which an early guest panic could ever be
seen — has been verified by construction and not by observation. A guest that boots silently,
or one that panics with no console to say so, is the shape to expect.

`GOOS=windows GOARCH=amd64 go build ./cmd/containerd-shim-nerdbox-v1`, from a fresh checkout of
the pinned revision with all three patches applied, produces an **18,655,232-byte PE
executable**. `GOOS=windows GOARCH=arm64` also builds. `GOOS=linux` still builds for both
`amd64` and `arm64`, so nothing here regressed the platform that works.

Patch 0003 has one piece of evidence the other two do not, because unlike them it is testable
without a VM. `krun_create_ctx`, `krun_add_vsock` and `krun_add_vsock_port2` only mutate a
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
**This still says nothing about whether the guest boots** — it says the configuration call that
used to be rejected is now accepted.

**That is the entire claim. The shim has not been started on Windows.** No one has run
`start`, no named pipe has been listened on, no ttrpc call has been served, and the patched
binary has never been in the same room as a containerd. Compiling a shim is not starting one,
and the bug this directory exists to fix was itself only visible at runtime — the unpatched
binary compiled perfectly too.

The next wall is unknown by construction. Getting past `setupSignals` makes the other five
stubs reachable for the first time; they now have implementations, but implementations that
have never executed.

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

The build proves nothing about runtime. To find out what actually happens, on a Windows machine
with the patched `containerd-shim-nerdbox-v1.exe`:

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
  [containerd/containerd#13948](https://github.com/containerd/containerd/pull/13948). If we
  find bugs in it by running it, they belong in that PR's review thread — we would be the
  first people to have run it.
- Patch 0002: an independent bug report against `containerd/nerdbox`. Small, self-contained,
  and does not depend on #13948 landing.
- Patch 0003: likewise independent, and the easiest of the three to argue for — it adopts an
  API change upstream libkrun made deliberately, and because it treats `-EEXIST` as success it
  does not require nerdbox to move its own `LIBKRUN_VERSION` first. Send it with the `-ENODEV`
  return codes above as the reproduction.
