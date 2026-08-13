# nerdbox's shim, patched to start on Windows

Boks' container runtime is [nerdbox](https://github.com/containerd/nerdbox), whose shim
binary is `containerd-shim-nerdbox-v1`. It compiles for Windows today. It cannot **start** on
Windows: containerd's shim library stubs out the entire process-lifecycle layer for that
platform, so the binary exits before it does any work. `patches/` holds the changes that get
past that, and `.github/workflows/nerdbox-windows.yml` builds the result so the series cannot
rot unnoticed.

**These patches are a staging area, not a fork**, exactly like `packaging/libkrun-windows/`.
The goal is to delete this directory: patch 0001 is someone else's unmerged upstream pull
request that we are carrying early, and patch 0002 belongs in nerdbox. Nothing in Boks links
against a patched nerdbox — the patches are compiled, never shipped.

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

Two patches, generated with `git format-patch` from a working branch off the pinned revision.

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

## How far they get

`GOOS=windows GOARCH=amd64 go build ./cmd/containerd-shim-nerdbox-v1`, from a fresh checkout of
the pinned revision with both patches applied, produces an **18,654,720-byte PE executable**.
`GOOS=windows GOARCH=arm64` also builds. `GOOS=linux` still builds, so nothing here regressed
the platform that works.

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

## Upstreaming

- Patch 0001: nothing to do but wait for
  [containerd/containerd#13948](https://github.com/containerd/containerd/pull/13948). If we
  find bugs in it by running it, they belong in that PR's review thread — we would be the
  first people to have run it.
- Patch 0002: an independent bug report against `containerd/nerdbox`. Small, self-contained,
  and does not depend on #13948 landing.
