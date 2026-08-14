# Running a container in a microVM on Windows

The procedure for the first `ctr run` on Windows: containerd, the nerdbox shim, `krun.dll`, a
Linux guest kernel and an EROFS rootfs, composed into one container.

> **Steps 0–3 have been executed on Windows 11. Steps 4–7 have not.** This document was written
> entirely by reading source — containerd v2.3.3, nerdbox v0.2.3 (`cd2c23f`), and the Boks patch
> series in `packaging/` — and every expectation is labelled **source** (traced to a specific
> file and line), **inference** (reasoned from one), or **unknown**. Where a step *was* measured
> on the Windows 11 test machine, it says so and gives the date.
>
> The first run through it found two things reading had missed, and both are now folded in: a
> silent-corruption trap in containerd's `mkfs` handling (`patches/0005`, and the note in step 5)
> and the fact that **creating a task needs elevation or Developer Mode** ("Before step 0"). The
> version of this document that said to run all of it unelevated was wrong.

The command this is all for:

```powershell
ctr.exe -n boks run --rm --null-io `
  --runtime io.containerd.nerdbox.v1 `
  --snapshotter erofs `
  --platform linux/amd64 `
  docker.io/library/alpine:latest e2e /bin/true
```

Everything below exists to make each of those words resolvable. It also cannot be run as a single
command today — step 5 has to happen in the middle of it, for a reason that is Windows' fault and
nobody else's — so the procedure splits it in two.

## Where the chain already stands

| Stage | Status |
| --- | --- |
| `krun.dll` boots a Linux 6.12.44 guest, clock advancing | **measured**, Windows 11, 2026-08-14 — [verification.md](verification.md) |
| the nerdbox shim starts, serves its ttrpc and log pipes, emits valid bootstrap params | **measured**, 2026-08-14 |
| containerd unpacks a Linux image with the EROFS snapshotter | **measured**, 2026-08-14 |
| containerd creates a usable `--root` unelevated | **fixed by a documented prerequisite** — see `packaging/containerd-windows/README.md` |
| `ctr run` can produce a Linux OCI spec on Windows | **patched**, never run — `patches/0004` |
| a failed `mkfs.ext4` cannot be mistaken for a formatted image | **patched** — `patches/0005`. Before it, the file a failed format left behind was accepted by the next attempt and the guest would have been handed 64 MiB of zeroes as ext4 |
| creating a task bundle at all | **blocked without elevation or Developer Mode.** `NewBundle` symlinks unconditionally; measured 2026-08-14. See "Before step 0" below |
| any of the above composed into one container | **never attempted.** This document. |

Each stage works alone. Nothing has ever been run in sequence.

## 1. What you need, and where each piece comes from

Ten files from four places: three CI artifacts, and a `krun.dll` you build yourself. One of the
ten is a file this document exists partly to explain.

| File | From | Notes |
| --- | --- | --- |
| `containerd.exe`, `ctr.exe` | `containerd-windows-amd64-bundle` | must report `v2.3.3+boks-erofs`; `ctr.exe` must carry `patches/0004` |
| `mkfs.erofs.exe` | same bundle | needed at *daemon start*, or the erofs differ skips itself |
| `config.toml`, `new-containerd-root.ps1`, `rwlayer-64m.img` | same bundle | see steps 0 and 5 |
| `containerd-shim-nerdbox-v1.exe` | `containerd-shim-nerdbox-v1-windows` (`nerdbox-windows.yml`) | must be the **patched** shim — the unpatched one dies in 118 ms |
| `nerdbox-kernel-x86_64`, `nerdbox-rootfs.erofs` | `nerdbox-guest-x86_64` (`guest-image.yml`) | exact filenames, no extension on the kernel |
| `krun.dll` | **you build it** — `packaging/libkrun-windows/README.md` | not published as an artifact; it is the same DLL that booted the guest on 2026-08-14 |

### The names are not negotiable

nerdbox looks for exactly these, and nothing else (`internal/vm/libkrun/instance.go:80-120`,
**source**):

| artifact | filenames tried, in order |
| --- | --- |
| libkrun | `krun.dll` — and only that |
| kernel | `nerdbox-kernel-x86_64` — **no arch-less fallback exists** |
| rootfs | `nerdbox-rootfs-x86_64.erofs`, then `nerdbox-rootfs.erofs` |

`kernelArch()` maps `amd64` → `x86_64` (`instance.go:481-488`). The kernel is an **ELF
`vmlinux`**, not a `bzImage` — that is what `krun_set_kernel` takes and what
`guest-image.yml` deliberately produces. Renaming any of these gives you one of three errors,
verbatim (`instance.go:121-129`):

```
krun.dll not found in PATH or LIBKRUN_PATH
nerdbox-kernel not found in PATH or LIBKRUN_PATH
nerdbox-rootfs-x86_64.erofs or nerdbox-rootfs.erofs not found in PATH or LIBKRUN_PATH
```

Note the middle one does not echo the architecture it actually looked for, which makes it the
easiest of the three to misread.

### The search order, and whose `PATH` it is

```go
// internal/vm/libkrun/instance.go:70-79
p1 = filepath.SplitList(os.Getenv("PATH"))
p2 = filepath.SplitList(os.Getenv("LIBKRUN_PATH"))
...
if runtime.GOOS != "windows" && len(p2) == 0 {
    p2 = []string{"/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/lib"}
}
```

then one pass over `append(p1, p2...)` — **`PATH` first, then `LIBKRUN_PATH`**, first match wins
per artifact, an empty entry meaning `.` (`instance.go:90-120`). On Windows the `/usr/lib`
fallback list is explicitly skipped, so with `LIBKRUN_PATH` unset only `PATH` is scanned.
`filepath.SplitList` splits on `;` here.

**The `PATH` that matters is containerd's, not your shell's.** The shim is a child of
containerd, and containerd builds its command line with

```go
// containerd v2.3.3, pkg/shim/util.go:103-104
cmd.Env = append(
    os.Environ(),
```

so the shim inherits the daemon's environment (**source**). Practically:

- Start `containerd.exe` from a console **after** setting `$env:Path` in that console, and the
  shim sees it. This is what the procedure below does.
- Install containerd as a Windows service and the shim gets the *machine* `PATH` instead, which
  is almost certainly not what you edited. Do not use a service for this.

`krun.dll` itself is loaded by full path — `syscall.LoadLibrary(krunPath)`,
`internal/vm/libkrun/dlfcn_windows.go:28-34` — so the OS loader search order does not apply to
*it*. It still applies to **its own dependencies**, so whatever MSVC runtime `krun.dll` imports
must be resolvable the ordinary way. Every one of the 19 exported symbols is then bound eagerly
with `GetProcAddress` (`dlfcn_windows.go:48-54`); one missing symbol panics, is recovered
(`krun.go:330-343`) and surfaces as a load error. That has bitten once already —
`krun_set_console_output` was gated behind an unrelated Cargo feature and a four-symbol sample
had reported everything fine — which is why `libkrun-windows.yml` checks all nineteen.

### The shim's filename is derived, not configured

`io.containerd.nerdbox.v1` → last two dot-components → `containerd-shim-%s-%s.exe`
(`pkg/shim/util_windows.go:30`), i.e. **`containerd-shim-nerdbox-v1.exe`**, resolved on
containerd's `PATH` (**source**). Put it in the same directory as everything else.

## 2. Lay it out

One directory, on `PATH`, containing all ten files:

```
C:\boks-test\
    containerd.exe
    ctr.exe
    mkfs.erofs.exe
    containerd-shim-nerdbox-v1.exe
    krun.dll
    nerdbox-kernel-x86_64
    nerdbox-rootfs.erofs
    config.toml
    new-containerd-root.ps1
    rwlayer-64m.img
```

One directory is deliberate. Two separate processes resolve five names out of `PATH` — containerd
for `mkfs.erofs.exe` and the shim binary, and the shim for the DLL, the kernel and the rootfs —
and splitting them across directories is how you end up debugging a search order instead of a
container.

## 3. The procedure

Every step says what success looks like and what the likeliest failure means. Run all of it as
one user, from one console.

### Before step 0: this needs elevation, or Developer Mode

**This document used to say "run all of it unelevated". That was wrong, and the first
end-to-end run on hardware is what found out.**

Steps 0–3 are fine unelevated — they were measured that way on 2026-08-14, and they create no
task. **Steps 4, 6 and 7 create a task, and unprivileged Windows cannot.**
`core/runtime/v2/bundle.go:103` does, in `NewBundle`, for every task bundle:

```go
if err := os.Symlink(work, filepath.Join(b.Path, "work")); err != nil {
```

Creating a symlink needs `SeCreateSymbolicLinkPrivilege`. Go already passes
`SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE` (`os/file_windows.go:371`), which Windows
honours **only under Developer Mode**. Measured on the test machine:
`New-Item -ItemType SymbolicLink` → *"Administrator privilege required"*; `mklink /J` succeeds.

**The junction trick used for the root-ACL blocker does not apply here.** That one works
because `MkdirAllWithACL` accepts a directory that already exists. Here the link must land
inside `b.Path`, and `b.Path` is created by containerd itself with `os.Mkdir(b.Path, 0700)`
(`bundle.go:74`) whose error is returned unconditionally — so `b.Path` must *not* pre-exist,
and there is no moment at which a junction could be placed inside it.

So, three options, and their costs:

| | What it costs |
| --- | --- |
| **Run `containerd.exe` elevated** | the daemon, every shim and every VM run with an administrator's token. **Its `--root` must then be created by the elevated daemon itself** — see below |
| **Turn on Developer Mode** | machine-wide. It grants unprivileged symlink creation to **every process run by every user on that machine**, not to containerd and not to you, and switches on other developer features besides |
| **Neither** | steps 0–3 work; steps 4 onwards do not. This is a real option, and it is what the containerd bundle's own test procedure assumes |

**On a corporate-managed machine, Developer Mode may not be your call.** It is commonly
disabled by policy (`AllowDevelopmentWithoutDevLicense`) and the toggle may be greyed out or
revert. Nothing here can tell you it is fine to enable, because that depends on a policy this
document cannot see. Ask first.

**If you run elevated, do not reuse the `--root` from step 0.** `MkdirAllWithACL` applies its
protected DACL only to components it creates and returns early on ones that exist, so a root
pre-created by an ordinary user keeps that user's permissions — and an elevated daemon then
fills it with the content store, the snapshotters and the bolt database, all writable by an
unprivileged account. That account can put a binary into a layer a later container runs. Give
the elevated daemon a fresh root of its own and let it create it; `new-containerd-root.ps1` is
for the unelevated case only. Full reasoning in `packaging/containerd-windows/README.md`,
"Elevation, Developer Mode, and the choice you actually have".

*That elevation makes the symlink succeed is inferred from Windows' default privilege
assignment plus the unelevated failure measured above; nobody has watched containerd create a
task bundle elevated.*

### Step 0 — create containerd's root and state

```powershell
$env:Path = "C:\boks-test;$env:Path"
C:\boks-test\new-containerd-root.ps1
```

**Success:** `writable` next to both paths.

**Failure:** anything else. Do not continue — an unelevated containerd creates its own `--root`
with a DACL granting only `Administrators` and `SYSTEM`, then fails to write inside it, taking
43 plugins down behind an error that names the content store rather than a permission. This is
measured, and the full explanation is in `packaging/containerd-windows/README.md`, "The root
directory must exist first".

### Step 1 — start containerd in a console and leave it there

```powershell
containerd.exe --config C:\boks-test\config.toml --log-level debug
```

Open a second shell for everything after this, and in it:

```powershell
$env:Path = "C:\boks-test;$env:Path"
$env:CONTAINERD_ADDRESS = '\\.\pipe\boks-containerd'
```

**Success:** the plugin graph initialises. Unelevated, `ctr.exe plugins ls` shows at most two
failures — `io.containerd.internal.v1.opt` wanting `C:\ProgramData\containerd\root` and `cri`
wanting `C:\Program Files\containerd` — neither of which cascades. **Elevated, both disappear.**

This document previously called those two "known unrelated failures". They are not unrelated:
they are the same root cause as step 0's, arriving at two more plugins. Both paths are under
`%ProgramData%` and `C:\Program Files`, which an ordinary user cannot create; with an
administrator's token they can be, and are. Measured, 2026-08-14. It changes no advice — they
still cascade into nothing either way — but "unrelated" was simply wrong, and it was the kind
of wrong that stops you looking.

**Failure:** more than two unelevated (or any at all elevated) means step 0 did not take, or
`config.toml` was not passed.

**A console, not a service** — twice over: you need the live plugin log, and you need the shim to
inherit *this* console's `PATH`.

### Step 2 — check the runtime is visible to containerd before using it

```powershell
containerd-shim-nerdbox-v1.exe -v
containerd-shim-nerdbox-v1.exe -info
```

**Success:** both exit 0, and `-info` prints a protobuf containing `io.containerd.nerdbox.v1` and
`containerd.io/runtime-allow-mounts` with the value `mkdir/*,format/*,erofs`. Both were
**measured** on 2026-08-14, on the unpatched binary as well — these two paths return before the
Windows stubs are reached.

This matters more than it looks. containerd runs `-info` itself, at task-create time, to learn
which mount types to leave alone (`core/runtime/v2/task_manager.go:179-185`,
`loadShimInfo` → `handledMounts` → `mount.WithAllowMountType`). If it fails, containerd's mount
manager tries to handle `format/mkdir/overlay` and `erofs` on the Windows host, which it cannot.

**Failure:** `io.containerd.nerdbox.v1: not implemented` means you have the **unpatched** shim.
Get the one from `nerdbox-windows.yml`.

### Step 3 — pull the image

```powershell
ctr.exe -n boks images pull --platform linux/amd64 --snapshotter erofs `
  docker.io/library/alpine:latest
```

**Success:** the daemon log shows `running mkfs.erofs.exe [... --tar=f ...]` and `image unpacked`.
This whole step was **measured** on 2026-08-14; if it fails now, something in the environment
regressed rather than something new being discovered. `packaging/containerd-windows/README.md`
steps 4–7 diagnose it.

Alpine is chosen because it is **one layer**. Layer count matters later: nerdbox packs more than
eight EROFS layers into a single GPT-partitioned VMDK instead of one disk each
(`internal/shim/task/mount.go:48`, `gptLayerThreshold = 8`), and there is a hard cap of 26 disks
including the guest's own rootfs (`internal/shim/task/service.go:375-377`). One layer keeps both
paths out of the first test.

### Step 4 — run it once, expecting it to fail, and read the spec it generated

```powershell
ctr.exe -n boks run --null-io `
  --runtime io.containerd.nerdbox.v1 `
  --snapshotter erofs `
  --platform linux/amd64 `
  --dump-config C:\boks-test\spec.json `
  docker.io/library/alpine:latest e2e /bin/true
```

**Note the absence of `--rm`.** That is what makes this step useful. `ctr run` creates the
container (and its snapshot), writes the spec, and only then creates the task; without `--rm`
nothing is cleaned up when the task fails (`cmd/ctr/commands/run/run.go:189` — the deferred
`container.Delete` is registered only when `rm && !detach`). So the container and its snapshot
survive for steps 5 and 6.

**Expected outcome: it fails**, at task creation, with

```
failed format "C:\cdtest\root\io.containerd.snapshotter.v1.erofs\snapshots\<N>\rwlayer.img":
  mkfs.ext4 failed: ... executable file not found in %PATH%
```

That is step 5's problem and is expected here. `--dump-config` runs *before* task creation
(`run.go:196-210`), so `spec.json` is written regardless.

**Now check the spec — this is what `patches/0004` is for:**

```powershell
$s = Get-Content -Raw C:\boks-test\spec.json | ConvertFrom-Json
$null -ne $s.linux          # must be True
$s.process.cwd              # must be /
$s.process.args             # must be /bin/true
$s.root.path                # rootfs
```

**Success:** a `linux` section exists, `process.cwd` is `/`, `process.args` is `/bin/true`.

**Failure:** no `linux` section and `process.cwd` = `C:\`. Then this `ctr.exe` predates `0004`,
or `--platform linux/amd64` was omitted. On Windows,
`cmd/ctr/commands/run/run_windows.go` chooses the spec's platform from the **snapshotter name**
alone, and `--snapshotter erofs` is not `windows-lcow`, so you get a Windows spec for a Linux
image. Nothing downstream rejects it — nerdbox never reads `spec.Linux`
(`grep spec\.Linux` over the shim: two hits, both guest-side and nil-guarded) and copies
`spec.Windows` verbatim into the guest's `config.json` (`bundle/bundle.go:78-88`) — so the error
surfaces much later, inside crun, as something about a missing `linux` block.

> Expect an empty `"windows": {}` object in the spec even when it is correct.
> `oci.WithDefaultSpecForPlatform("linux/amd64")` adds one on a Windows host on purpose, for LCOW
> (`pkg/oci/spec.go:97-100`). nerdbox forwards it to the guest untouched. Whether crun 1.24
> objects to an empty `windows` object is **unknown** — nothing in either tree answers it.

> `--platform` exists only on `ctr run`, not on `ctr containers create`
> (`cmd/ctr/commands/commands.go`, `ContainerFlags`), which is the other reason this step is a
> deliberately-failing `run` rather than a `create`.

### Step 5 — put the writable layer where Windows cannot format it

This is the step nobody expects, and it is unavoidable without shipping a `mkfs.ext4` for
Windows.

On non-Linux the erofs snapshotter defaults to **block mode**: `defaultWritableSize = 64 MiB`
(`plugins/snapshots/erofs/erofs_other.go:27-30`; Linux's is `0`) and
`blockMode = config.defaultSize > 0` (`erofs.go:187`). An active snapshot's mount list therefore
begins with (`erofs.go:395-411`):

```go
{ Source: s.writablePath(snap.ID), Type: "mkfs/ext4",
  Options: []string{"X-containerd.mkfs.fs=ext4", "X-containerd.mkfs.size=67108864", "rw", "loop"} }
```

`mkfs/ext4` is **not** in nerdbox's `mkdir/*,format/*,erofs` allow list, so containerd's own
mount manager handles it — by running `mkfs.ext4` (`core/mount/manager/mkfs.go:106,143`). There
is no `mkfs.ext4` on Windows in any packaged form. nerdbox's README tells macOS users to
`brew install e2fsprogs` for exactly this reason, and adds that Homebrew does not put it on
`PATH` (nerdbox `README.md:114-140`).

The escape is in the same file: **formatting is skipped when the image is already formatted.**
So a correct image put there in advance is accepted. `rwlayer-64m.img` in the bundle is exactly
that: 64 MiB, `mkfs.ext4 -q`, made on the Linux CI runner where that binary exists.

> **This used to say "skipped when the file already exists", and it meant it.** Upstream
> containerd decided by `Stat` alone, with a `// Check magic number` comment where the check
> belonged. Since the format path creates and truncates the file *before* running mkfs, the
> failure in step 4 left behind 67,108,864 bytes of zeroes that the next attempt accepted on
> sight — and the tester avoided mounting them only by overwriting that file before retrying.
> Measured on the machine: `ext4 magic @1080 : 0x0000`.
>
> `patches/0005` fixes it: the magic is read (`53 ef` at offset 1080 for ext4), a file that
> fails the check is refused and left alone, and a file this code created and failed to format
> is removed. **Two consequences for the procedure below.** The copy in step 5 now *creates*
> `rwlayer.img` rather than overwriting one — step 4 no longer leaves anything there. And if
> you copy the wrong file, or a truncated one, step 6 now says so instead of booting a guest
> onto a filesystem that does not exist.

`writablePath(id)` is `<erofs root>\snapshots\<id>\rwlayer.img` (`erofs.go:206-208`). **Step 4's
error message names the exact path** — take it from there rather than guessing:

```powershell
Copy-Item C:\boks-test\rwlayer-64m.img `
  'C:\cdtest\root\io.containerd.snapshotter.v1.erofs\snapshots\<N>\rwlayer.img'
```

If for some reason you do not have the message, the active snapshot is the newest directory under
`…\snapshots` that contains no `layer.erofs`:

```powershell
$snapRoot = 'C:\cdtest\root\io.containerd.snapshotter.v1.erofs\snapshots'
Get-ChildItem $snapRoot |
  Where-Object { -not (Test-Path (Join-Path $_.FullName 'layer.erofs')) } |
  Sort-Object LastWriteTime | Select-Object -Last 1
```

**Success:** `<snapshots>\<N>\rwlayer.img` exists and is exactly 67,108,864 bytes.

**Failure:** the same `mkfs.ext4` error again in step 6 means the file went somewhere unused.
Nothing worse happens; the copy is inert wherever it lands.

### Step 6 — start the task

The container from step 4 is still there, with its spec and its snapshot. Give it a task:

```powershell
ctr.exe -n boks tasks start --null-io e2e
```

`ctr tasks start` does the `NewTask` that step 4 could not (`cmd/ctr/commands/tasks/start.go:95`),
waits, and prints the exit status. `--null-io` first, on purpose: it removes the stdio path
(vsock port 1026, the streaming handshake, the console pipe) from the experiment, so that a
failure means "the VM did not run a container" rather than "something about I/O".

**Success:** exit status 0 from `/bin/true`, and in the containerd console a shim log showing
the VM starting.

This is the step where everything unexercised lives. What is *supposed* to happen, read out of
nerdbox:

1. containerd creates the bundle at `C:\cdtest\state\io.containerd.runtime.v2.task\boks\e2e`
   and execs `containerd-shim-nerdbox-v1.exe ... start` with that as its **working directory**
   (`pkg/shim/util.go:102`, `cmd.Dir = config.WorkDir`).
2. The shim reads `config.json` from the cwd, requires `spec.Root != nil`, and rewrites
   `Root.Path` to `"rootfs"` unconditionally (`bundle/bundle.go:90-102`).
3. `transformMounts` turns each `erofs` mount into a read-only virtio-blk disk and the `ext4`
   one into a writable disk (`internal/shim/task/mount.go:113-238`). Disks start at **`/dev/vdb`**
   because the guest's own rootfs occupies `/dev/vda` (`instance.go:59-62`, `ReservedDisks() 1`).
4. The `format/mkdir/overlay` mount is forwarded to the guest **only because its `upperdir=` and
   `workdir=` contain `{{` templates** (`mount.go:207-213`). Block mode is what produces those;
   in non-block mode they are concrete host paths and Windows has no fallback —
   `mount_other.go:29-31` is a bare pass-through, unlike `mount_linux.go:68-85`. This is the
   second reason not to set `default_size = 0` in `config.toml`.
5. The shim resolves `krun.dll`, `nerdbox-kernel-x86_64` and `nerdbox-rootfs.erofs` from
   containerd's `PATH`, attaches the rootfs as the first virtio-blk device, read-only, raw
   (`instance.go:153`).
6. It configures **both vsock ports** before starting the VM (`instance.go:299-332`, both
   unconditional, nothing gates them):

   | port | mode | host path | direction |
   | --- | --- | --- | --- |
   | 1025 | `listen=false` | `vm\run_vminitd.sock` | the **guest dials out**; libkrun connects to the AF_UNIX socket the shim is already listening on |
   | 1026 | `listen=true` | `vm\streaming.sock` | libkrun listens; the host dials in per stream |

   Both are ordinary AF_UNIX sockets — `net.Listen("unix", ...)` — not named pipes, even on
   Windows, and both are made relative to the shim's cwd, so they are `vm\run_vminitd.sock` and
   `vm\streaming.sock`, about 21 bytes, far under the 108-byte guard at `instance.go:305`.
7. The kernel is booted with a fixed cmdline and no initrd (`instance.go:257-263`):

   ```
   console=hvc0 root=/dev/vda rootfstype=erofs ro init=/sbin/vminitd
   ```

   and `krun_set_exec("/sbin/vminitd", ["-vsock-rpc-port=1025", "-vsock-stream-port=1026",
   "-vsock-cid=3"], …)`. Note those three arguments differ from `vminitd`'s own flag defaults
   (1024/1025/0, `pkg/vminit/initd/initd.go:92-94`), which are never used in practice.
8. `vminitd` and `crun` are **baked into `nerdbox-rootfs.erofs`** at `/sbin/vminitd` and
   `/sbin/crun` (nerdbox `Dockerfile:236-248`). On boot, vminitd dials **out** to host CID 2 port
   1025 (`initd.go:298-301`, `listener.go:55-61`) — the RPC roles are inverted relative to the
   TCP direction: the guest connects, then serves ttrpc over that one connection, and the shim
   wraps its end in a ttrpc *client* (`instance.go:381-382`).
9. The shim waits **30 seconds** on Windows, not 15 (`instance.go:42-49`, raised explicitly for
   WHP's startup overhead), and gives up with `VM did not connect within 30s`.
10. Guest side: the bundle is written to `/run/bundles/e2e`, the container rootfs is mounted at
    `/run/bundles/e2e/rootfs`, and `/sbin/crun` is invoked against it
    (`plugins/services/bundle/service.go:64-82`, `internal/vminit/runc/container.go:45,60-102`).

**About the earlier kernel panic.** A previous bare `krun.dll` probe ended in
`Kernel panic - not syncing: Attempted to kill init!`, and the reason is now legible: that probe
configured no vsock at all, so `vminitd`'s dial-back to host CID 2 had nothing to reach and PID 1
exited. Through this path both ports are configured before `krun_start_enter`. So the **expected
behaviour is that vminitd survives** — that is an **inference from source**, and the single most
informative thing this step can tell us either way.

Read the guest kernel's own output while diagnosing: `console=hvc0` is wired to a named pipe at
`\\.\pipe\krun-console-<shim pid>-<vm id>` (`internal/vm/libkrun/console_windows.go:40`), which
the shim dials back with `winio.DialPipe` and up to 5 s of backoff. That is where a panic message
would appear.

### Step 7 — stdio, then a shell

Only once step 6 has passed. Repeat the 4–5–6 shape with a new id and real I/O. `ctr tasks start`
without `--detach` already deleted the task on exit, so only the container is left to clean up:

```powershell
ctr.exe -n boks containers delete e2e

# fails at mkfs again, and creates a new snapshot
ctr.exe -n boks run --runtime io.containerd.nerdbox.v1 --snapshotter erofs `
  --platform linux/amd64 docker.io/library/alpine:latest e2e2 /bin/echo hello
Copy-Item C:\boks-test\rwlayer-64m.img '<the path from the error>'
ctr.exe -n boks tasks start e2e2
```

**Success:** `hello` on your console. That exercises vsock port 1026, the length-prefixed
stream-ID handshake with echo ack (`instance.go:387-431`) and the Windows stdio pipes.

Then, and only then, a terminal:

```powershell
ctr.exe -n boks run -t --runtime io.containerd.nerdbox.v1 --snapshotter erofs `
  --platform linux/amd64 docker.io/library/alpine:latest e2e3 /bin/sh
Copy-Item C:\boks-test\rwlayer-64m.img '<the path from the error>'
ctr.exe -n boks tasks start e2e3
```

Every new container gets a new snapshot and therefore needs the writable-layer image again. That
repetition is the cost of not having a `mkfs.ext4` for Windows, and it is the strongest argument
for eventually getting one.

## 4. Networking: none, and nothing to ask for

**A no-network run is not only possible, it is the default.** Pass nothing.

`o.NICs` is populated only from `io.containerd.nerdbox.network.*` annotations
(`internal/shim/task/networking.go:187-206`); with none, the loop in
`internal/shim/sandbox/vm/vm.go:108-116` does not execute, no `krun_add_net_*` is called, and the
VM boots with no NIC. There is no error path and no warning (**source**). nerdbox's own
`docs/vm-configuration.md:12-18` states the same intent: *"By default, nerdbox creates microVMs
with no network interface set up."*

There is no CNI in the shim at all — grepping `internal/ pkg/ plugins/ cmd/` for CNI finds
nothing. `--net-host` is not read by the shim anywhere; it appears only in nerdbox's Linux/macOS
doc examples, and on Windows `ctr` rejects it outright before the shim is involved
(`run_windows.go`: `cannot use host mode networking with Windows containers`).

One networking-adjacent thing runs unconditionally: `addResolvConf` reads the **host's**
`/etc/resolv.conf` (`ctrnetworking.go:190-195,234-244`). On Windows that path does not exist, the
read fails, and the code falls through to the guest's own `/etc/resolv.conf`. Not fatal
(**source**).

**Boks' own refusal does not apply here.** `internal/network/vmm_windows.go` still declines to
start sandbox networking on Windows, and it should — that refusal rests on "no frame has ever
crossed that device", which is still true. But this test goes through `ctr` and the shim, not
through Boks, so nothing in that file is consulted. A container that runs here with no NIC
neither lifts that refusal nor contradicts it.

## 5. Runtime options and annotations

**Nothing is required.** For the minimal run, pass no annotations and no runtime options.

| Annotation | Effect | Default |
| --- | --- | --- |
| `io.containerd.nerdbox.resources.cpu` | vCPUs | **2** |
| `io.containerd.nerdbox.resources.memory` | RAM in **MiB** | **2048** |
| `io.containerd.nerdbox.dump-info` | presence alone adds `-dump-info` to vminitd's args | off |
| `io.containerd.nerdbox.network.<suffix>` | a VM NIC — **not wanted here** | none |
| `io.containerd.nerdbox.ctr.network.<suffix>`, `io.containerd.nerdbox.ctr.dns` | in-guest container netns and DNS | none |

Defaults are set explicitly at `internal/shim/task/transformers.go:39-42`, and `cpu == 0 ||
mem == 0` is a hard error. The network annotations need a suffix after the dot — the code matches
`networkAnnotation + "."` (`networking.go:81`) — so a bare
`--annotation io.containerd.nerdbox.network=…` is silently ignored.

Worth passing on a first run for the extra guest logging:

```powershell
--annotation io.containerd.nerdbox.dump-info=1
```

**There is no annotation or option for the kernel path, the rootfs path, the vsock ports, the
guest CID, the kernel cmdline or the init binary.** Those are compile-time constants or come from
the `PATH` scan. If a path is wrong, `PATH` is the only lever.

**`--runtime-config-path` buys nothing.** nerdbox's `Info()` returns no `Options` at all — the
block that would read them is commented out with a TODO (`pkg/shim/manager/manager.go:88-104`) —
and task-level options are accepted only as `containerd.runc.v1.Options`, of which exactly two
fields survive into the guest: `NoPivotRoot` and `NoNewKeyring`
(`internal/shim/task/service.go:88-99`). Anything else is rejected with
`unsupported options type %T, guest runtime only supports runc options`.

## 6. Failure modes, most likely first

1. **`NewBundle` cannot create its symlink** — certain from step 4 onwards on an unelevated
   machine without Developer Mode, and it lands before the shim is ever launched. Measured,
   2026-08-14. *Fix: elevate, or Developer Mode, or stop after step 3. See "Before step 0".*
2. **`mkfs.ext4` not found** — certain on any single-command `ctr run`, which is why step 4 is
   written as a deliberate failure. `failed format "…rwlayer.img": mkfs.ext4 failed: …
   executable file not found`. Deterministic from source; there is no branch that avoids it.
   With `patches/0005` the half-made image is removed as part of that failure, so step 5
   creates the file rather than overwriting one. *Fix: step 5.*
3. **`…rwlayer.img` exists but carries no ext4 superblock** — new with `patches/0005`. It means
   step 5's copy did not land, landed truncated, or landed on the wrong path. Before that patch
   this was not an error at all; it was a guest booting onto zeroes. *Fix: recopy, and check
   the size is exactly 67,108,864 bytes.*
4. **A guest artifact or `krun.dll` not on containerd's `PATH`.** Likely on a first run, and the
   likeliest specific cause is starting containerd before editing `PATH`, or as a service, since
   the shim inherits the daemon's environment and not your shell's. The three error strings in
   §1 name the file. *Fix: restart containerd from a console whose `PATH` is right.*
5. **A Windows OCI spec.** Certain without `patches/0004` or without `--platform linux/amd64`.
   Surfaces late and confusingly, as a crun error about a missing `linux` block, because nothing
   between `ctr` and crun inspects the spec's platform. *Fix: step 4's check.*
6. **containerd locked out of its own root.** Certain on a clean machine without step 0.
   Measured. Reported as a content-store `mkdir` denial with 43 plugin failures.
7. **The five shim stubs that have never executed.** PR #13948 implements `newServer`,
   `serveListener`, `reap`, `openLog` and `subreaper` for Windows; `setupSignals` is the only one
   that has ever run, and only because it is the first. A real containerd driving a real ttrpc
   TaskService over `serveListener`'s pipe is new ground, and #13948 is unmerged upstream and has
   never been run against nerdbox by anyone, including its author.
   `packaging/nerdbox-windows/README.md` lists four specific things in it that look wrong.
   Symptom shape: the shim starting and then failing to serve, or a 10 s hang followed by
   `waitForShimPipe` giving up.
8. **The vsock link between Go's AF_UNIX and libkrun's Winsock backend.** Never exercised in
   either direction. Both ends are ours, both are new, and they must meet on a relative path with
   backslashes (`vm\run_vminitd.sock`). Symptom: `VM did not connect within 30s`, with the guest
   either never having dialled or having dialled somewhere else. The libkrun side is
   `packaging/libkrun-windows/patches/0017`, whose own message says "Not executed."
9. **The VM failing to boot under the shim although it boots under a bare probe.** The shim's VM
   has strictly more in it: two or more virtio-blk disks, two vsock ports, a console pipe, and a
   guest kernel cmdline the probe did not use. nerdbox's own `integration/test.ps1` records that
   **Windows allows only one VM partition per process with the current libkrun build**, which
   also means a shim that leaks a partition poisons every later container in that shim.
10. **`vminitd` exiting.** The panic seen before was caused by a probe with no vsock; this path
   configures both ports, so it should survive (**inference**). Everything after the dial-back —
   the tmpfs overlays over `/etc`, `/run`, `/tmp` on a read-only erofs root, mounting the
   container layers, invoking crun — has never run on a Windows-hosted VM. Watch the console pipe.
11. **crun rejecting the spec.** Two candidates, both from containerd's spec generation rather
   than from nerdbox: the empty `"windows": {}` object that nerdbox forwards verbatim, and any
   Windows-shaped field that survives. **Unknown** — crun is not in either tree.
12. **The overlay mount inside the guest.** Requires `{{ mount 0 }}` templates in `upperdir=` and
    `workdir=`, which block mode supplies. If `default_size = 0` is ever set in `config.toml`,
    this becomes `cannot use virtiofs for upper dir in overlay: not implemented` with **no
    fallback on Windows**. Do not set it.
13. **Stdio.** Step 7's territory: vsock 1026, the stream-ID handshake, `-t` and the console. Also
    where `openLog`'s silently-discarding writer would cost you the early shim log lines, though
    nerdbox sets `NoSetupLogger` so it is not on the current path.
14. **Teardown.** `service_windows.go:195` does `os.RemoveAll("rootfs")` and
    `manager_windows.go` carries a `removeRootfs` for the bind-filter unmount problem. A failed
    teardown leaves a bundle and a snapshot behind and makes the *next* run confusing rather than
    the current one.

## 7. What this procedure cannot tell you

- **Nothing here has been run.** Steps 0–3 rest on measurements from 2026-08-14; steps 4–7 rest
  entirely on reading.
- **A container running proves nothing about isolation.** It proves a Linux process executed
  behind WHP. Boks' claims about what a sandbox contains are answered by
  [verification.md](verification.md), and none of that has been re-established on Windows.
- **Committing a layer will still fail.** `plugins/snapshots/erofs/erofs_other.go` stubs
  `convertDirToErofs` and `setImmutable` with `ErrNotImplemented`. Running a container does not
  go through them; `ctr commit` and anything that turns an upper directory back into an EROFS
  blob will.
- **The writable-layer workaround is a workaround.** Shipping a pre-formatted image because
  `mkfs.ext4` does not exist on Windows is not a fix; the fix is either a Windows `mkfs.ext4` or
  a snapshotter that does not need one. Neither exists today. An earlier version of this
  document worried that the mount manager's skip-if-exists was "a TODO comment away from
  growing a magic-number check that would defeat it". The check has now been written — by us,
  as `patches/0005` — and it does **not** defeat this: a genuinely formatted image passes it,
  which is what `rwlayer-64m.img` is. What the check ends is the *other* reading of
  skip-if-exists, where a file that merely existed was good enough.
- **Whether elevation is acceptable is not this document's call.** Steps 4 onwards need an
  elevated daemon or a machine-wide Developer Mode, and both are decisions with costs outside
  this procedure. Nothing here has been run elevated end to end either.
- **The `-info` / `plugins ls` weak-signal warning still applies.** A shim that reports its
  runtime id is a shim that parsed its flags.
