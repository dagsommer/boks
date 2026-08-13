# containerd for Windows, with the EROFS plugins registered

`containerd.exe` as upstream ships it cannot use an EROFS snapshotter, because it does not have
one. `cmd/containerd/builtins/builtins_windows.go` registers exactly four plugins:

```go
_ "github.com/containerd/containerd/v2/plugins/diff/lcow"
_ "github.com/containerd/containerd/v2/plugins/diff/windows"
_ "github.com/containerd/containerd/v2/plugins/snapshots/lcow"
_ "github.com/containerd/containerd/v2/plugins/snapshots/windows"
```

No erofs differ, no erofs snapshotter. `ctr --snapshotter erofs` has nothing to bind to.

## Why Boks needs them

Boks' runtime is nerdbox. Its non-Linux mount handling — `mount_other.go` — recognises exactly
three mount types (`erofs`, `ext4`, `overlay`) and forwards **anything else verbatim into the
guest**, where a `windows-layer` or `lcow` mount is meaningless and fails. An EROFS-backed
snapshotter is the only one on Windows that hands nerdbox mounts the Linux guest can consume.

## The patch

One patch, two import lines, in `patches/`:

```go
+_ "github.com/containerd/containerd/v2/plugins/diff/erofs/plugin"
+_ "github.com/containerd/containerd/v2/plugins/snapshots/erofs/plugin"
```

That is the whole change. No code was ported, because none needed porting: the erofs packages
were already made non-Linux-capable upstream and `builtins_unix.go` registers both for darwin,
freebsd and solaris. They carry `//go:build !linux` fallbacks (`erofs_other.go`,
`compare_other.go`, `dmverity_other.go`, `plugin_other.go`) and compile for `windows/amd64` and
`windows/arm64` unmodified. Windows was simply never added to the list.

The pin is `CONTAINERD_VERSION` — **v2.3.3**, matching the containerd that the nerdbox revision
in `packaging/nerdbox/NERDBOX_REV` vendors.

**This belongs upstream, not here.** It is two imports and it is obviously correct on its face;
the reason it is in Boks is that nobody has yet demonstrated that the plugins *work* on Windows,
and that demonstration is what the artifacts from this workflow are for. Delete this directory
once containerd takes it.

## What actually works on Windows, and what does not

Registering a plugin is not the same as it functioning. Reading the code:

**Should work — unpacking image layers.** `erofsDiff.Apply` reads a layer blob from the content
store and pipes it through `mkfs.erofs --tar=f` to produce `layer.erofs`. That path is
platform-neutral Go plus a subprocess. It is the path `ctr images pull --snapshotter erofs`
exercises.

**Cannot work — committing a writable layer.** `plugins/snapshots/erofs/erofs_other.go` stubs
`convertDirToErofs` and `setImmutable` with `errdefs.ErrNotImplemented`. Anything that turns a
container's upper directory back into an EROFS blob will fail on Windows. Pulling and unpacking
does not go through it.

**Unknown — running a container.** Actually mounting the result is nerdbox's and the guest's
problem, not containerd's, and none of it has been tried.

### The differ is invisible without `mkfs.erofs.exe`

The erofs **differ**'s `InitFn` runs `mkfs.erofs --help` and greps for `--tar=`. If the binary is
absent, or is too old to report tar mode, it returns `plugin.ErrSkipPlugin` — containerd logs one
line at info level and carries on. Nothing fails loudly; the plugin is just gone, and a pull with
`--snapshotter erofs` then fails for a reason that does not mention `mkfs.erofs` at all.

So `mkfs.erofs.exe` must be on `PATH` before `containerd.exe` starts, and `ctr plugins ls` is how
you check rather than assume. See `packaging/mkfs-erofs-windows/README.md`.

The erofs **snapshotter** has no such gate — it registers unconditionally. It is therefore
entirely possible to have the snapshotter present and the differ missing, which is exactly the
confusing half-state the test commands below are designed to detect.

## Testing it on Windows

Nothing here needs the nerdbox shim, a guest kernel, or a working microVM. This tests one
question — *can containerd on Windows unpack a Linux image with EROFS?* — and nothing else.

Download the `containerd-windows-amd64-bundle` artifact from the workflow run. It contains
`containerd.exe`, `ctr.exe` and `mkfs.erofs.exe`.

### 0. Put all three on PATH, in one directory

```powershell
mkdir C:\boks-test
# copy containerd.exe, ctr.exe, mkfs.erofs.exe into it
$env:Path = "C:\boks-test;$env:Path"
```

`mkfs.erofs.exe` must be on `PATH` **before** step 2 starts containerd. Confirm the shell can
find it:

```powershell
Get-Command mkfs.erofs
mkfs.erofs -V
```

**Proves:** the binary resolves by bare name (containerd calls `exec.Command("mkfs.erofs", ...)`,
relying on `PATHEXT` to add `.exe`) and it executes at all. `-V` printing a version is the first
time this binary will ever have run. If it fails with a missing-DLL error, nothing after this
point is worth trying.

### 1. Check the daemon runs

```powershell
containerd.exe --version
```

**Proves:** the PE loads and links. Expect `v2.3.3+boks-erofs`.

### 2. Start containerd in a console — do not install it as a service

```powershell
containerd.exe --log-level debug
```

Leave it running and open a second shell for the rest. A console process is deliberate: you see
the plugin-loading log live, including the info-level line where a plugin skips itself, which a
service would bury in the event log.

**Proves:** containerd initialises its plugin graph on Windows. Watch for lines mentioning
`erofs`. A skip looks like `failed to check mkfs.erofs availability` or
`mkfs.erofs does not support tar mode`.

### 3. The key check — did the erofs plugins load?

```powershell
ctr.exe plugins ls | Select-String erofs
```

**Proves:** whether registration actually took. Two rows are expected:

```
io.containerd.snapshotter.v1    erofs    windows/amd64    ok
io.containerd.differ.v1         erofs    linux/amd64      ok
```

Read the last column. `ok` means loaded. **`skip` on the differ means `mkfs.erofs.exe` was not
found on PATH when containerd started** — fix step 0 and restart containerd. An error there is
the interesting failure and worth capturing in full:

```powershell
ctr.exe plugins ls
```

### 4. Pull a Linux image through the erofs snapshotter

```powershell
ctr.exe -n test images pull --platform linux/amd64 --snapshotter erofs docker.io/library/alpine:latest
```

**Proves:** the whole unpack path end to end — containerd fetches a Linux image on a Windows
host, hands each layer's tar to the erofs differ, the differ runs `mkfs.erofs.exe --tar=f`, and
the erofs snapshotter commits the result. This is the single most informative command in the
list. Success here answers the question this directory exists to ask.

### 5. Confirm real EROFS blobs landed on disk

```powershell
ctr.exe -n test images ls
Get-ChildItem -Recurse -Filter layer.erofs C:\ProgramData\containerd\root\io.containerd.snapshotter.v1.erofs
```

**Proves:** the layers were genuinely formatted as EROFS rather than silently unpacked by some
other differ. One `layer.erofs` per image layer, each a few MB for alpine. Check one is really an
EROFS image — the superblock magic `0xE0F5E1E2` sits at offset 1024:

```powershell
$b = [System.IO.File]::ReadAllBytes((Get-ChildItem -Recurse -Filter layer.erofs C:\ProgramData\containerd\root\io.containerd.snapshotter.v1.erofs)[0].FullName)
'{0:X2}{1:X2}{2:X2}{3:X2}' -f $b[1027],$b[1026],$b[1025],$b[1024]
```

**Proves:** `E0F5E1E2` means a real EROFS superblock, written by `mkfs.erofs.exe` on Windows.

### 6. Clean up

```powershell
ctr.exe -n test images rm docker.io/library/alpine:latest
```

### Known likely snag

The snapshotter's `InitFn` declares its platform as `platforms.DefaultSpec()` — `windows/amd64`
— while the differ declares `linux/amd64`. Whether containerd's unpacker is happy pairing a
`windows/amd64` snapshotter with a `linux/amd64` differ for a `--platform linux/amd64` pull has
not been checked. If step 4 fails with a platform or "no differ" complaint rather than an
`mkfs.erofs` one, that mismatch is the first place to look.

## Verified here, on Linux, 2026-08-13

Cross-compiled in a container and inspected. **Not run** — this is a Linux machine, and none of
the commands above have been executed by anyone.

| Check | Result |
| --- | --- |
| patch applies to v2.3.3 | yes, `git am` clean |
| `head -c2` | `MZ` for both `containerd.exe` and `ctr.exe` |
| `objdump -f` | `file format pei-x86-64` — PE32+ x86-64 |
| erofs linked in | `strings containerd.exe` finds `plugins/diff/erofs/plugin/plugin.go`, `plugins/snapshots/erofs/plugin/plugin.go`, `erofsutils.ConvertTarErofs`, `NewErofsDiffer` |
| existing plugins kept | `plugins/snapshots/windows` still present |
| sizes | `containerd.exe` 36,757,504 B; `ctr.exe` 20,391,936 B |

Compile-time evidence only. It proves the plugins are *in* the binary. It proves nothing about
whether they initialise, and the failure mode we most expect — the differ quietly skipping itself
— is invisible to a compiler by construction.
