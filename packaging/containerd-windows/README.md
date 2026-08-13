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

## The patches

Three, in `patches/`. The first registers the plugins; the other two make them reachable, which
turned out to be a separate problem.

### `0001` — register the plugins

Two import lines:

```go
+_ "github.com/containerd/containerd/v2/plugins/diff/erofs/plugin"
+_ "github.com/containerd/containerd/v2/plugins/snapshots/erofs/plugin"
```

No code was ported, because none needed porting: the erofs packages were already made
non-Linux-capable upstream and `builtins_unix.go` registers both for darwin, freebsd and solaris.
They carry `//go:build !linux` fallbacks (`erofs_other.go`, `compare_other.go`,
`dmverity_other.go`, `plugin_other.go`) and compile for `windows/amd64` and `windows/arm64`
unmodified. Windows was simply never added to the list.

### `0002` — put erofs in the Windows diff-service order

`0001` alone gets you a differ that loads and is never consulted. `containerd config default` on
Windows says:

```toml
[plugins.'io.containerd.service.v1.diff-service']
  default = ['windows', 'windows-lcow']
```

The erofs differ is not in that list, so the diff service never offers it a layer. The walk
reaches the windows differ, which is handed the erofs snapshotter's mounts and rejects them:

```
number of mounts should always be 1 for Windows layers
```

**`ctr plugins ls` showing `ok` actively hides this.** `ok` means the plugin initialised. It says
nothing about whether anything will ever call it.

erofs goes **first**. It is the only differ in the list that declines politely: shown a mount
that is not an EROFS layer, `MountsToLayer` returns `ErrNotImplemented` and the loop continues.
The windows differs fail hard, which ends the loop — so erofs anywhere but first is erofs absent.

Naming a differ in that order made it *mandatory*: a missing one fails the diff service, which
`io.containerd.metadata.v1.bolt` depends on, which everything else depends on. The erofs differ
is explicitly not mandatory — it returns `ErrSkipPlugin` when `mkfs.erofs` is not on `PATH`. So
`0002` also makes the order tolerant of a differ that *registered and then skipped itself*, with
a warning, while a name that was never registered at all (a typo in `config.toml`) stays fatal,
and an order left empty is fatal. Without that, this patch would turn "no `mkfs.erofs.exe` on
`PATH`" from a confusing pull failure into a dead daemon.

### `0003` — add a Linux/erofs unpack config on Windows

With `0001` and `0002` in place, `ctr images pull` still fails before fetching anything:

```
ctr: unable to initialize unpacker: no unpack platforms defined: invalid argument
daemon: "Unpack configuration not supported, skipping" platform=linux/amd64 snapshotter=erofs
```

The transfer service's Windows default `unpack_config` has one entry — `windows/amd64` on the
windows snapshotter with the windows differ — so `--platform linux/amd64 --snapshotter erofs`
matches nothing and the unpacker is built with an empty platform list. `ctr --local` works
because it never goes through the transfer service.

Linux has had the equivalent entry since the erofs snapshotter landed. `0003` is the Windows one,
pinned to `linux/<arch>` because an EROFS layer is only useful to a Linux consumer, and
`optional = true` for the same reason as above.

Note this is *not* the "snapshotter `windows/amd64` versus differ `linux/amd64`" mismatch an
earlier revision of this README guessed at. containerd paired those two fine. The mismatch is in
`unpack_config`, and it is a different one.

The pin is `CONTAINERD_VERSION` — **v2.3.3**, matching the containerd that the nerdbox revision
in `packaging/nerdbox/NERDBOX_REV` vendors.

**All three belong upstream, not here.** Delete this directory once containerd takes them.

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
`containerd.exe`, `ctr.exe`, `mkfs.erofs.exe` and `config.toml`.

**Run all of this unelevated, and pass the config.** Both matter, and both cost you a run if you
skip them:

- **Do not use the default `--root` / `--state`.** They live under `C:\ProgramData\containerd`,
  which a normal user cannot create — `mkdir C:\ProgramData\containerd\root: Access is denied.`
  Elevating fixes the permission and creates a worse problem: those are the same directories a
  Docker Desktop or Docker Sandboxes containerd uses, and you would be testing two daemons at
  once. The shipped `config.toml` puts everything under `C:\cdtest` on a private pipe instead.
- **Do not use the default plugin set.** `io.containerd.snapshotter.v1.cimfs` cannot initialise
  unelevated — `failed to init base scratch VHD: … A required privilege is not held by the
  client` — and `io.containerd.metadata.v1.bolt` requires *every* snapshotter, so that one
  failure cascades through bolt, the differs, the services and the whole gRPC layer: about forty
  plugins, the erofs differ among them. `ctr plugins ls` then shows `error` next to erofs for a
  reason that has nothing to do with erofs. The shipped `config.toml` disables the two cimfs
  plugins.

`config.toml` is `packaging/containerd-windows/config.toml` in this repo, and is in the bundle.
Every line of it is commented with the failure it prevents.

### 0. Put the three binaries in one directory, on PATH

```powershell
mkdir C:\boks-test
# copy containerd.exe, ctr.exe, mkfs.erofs.exe, config.toml into it
mkdir C:\cdtest\root, C:\cdtest\state
$env:Path = "C:\boks-test;$env:Path"
Get-Command mkfs.erofs
mkfs.erofs -V
```

**Proves:** the binary resolves by bare name (containerd calls `exec.Command("mkfs.erofs", ...)`,
relying on `PATHEXT` to add `.exe`) and it executes at all. `mkfs.erofs.exe` must be on `PATH`
**before** step 2 starts containerd, or the differ skips itself.

### 1. Check the daemon runs

```powershell
containerd.exe --version
```

**Proves:** the PE loads and links. Expect `v2.3.3+boks-erofs`.

### 2. Check `mkfs.erofs.exe` on its own, before containerd is involved

```powershell
cd $env:TEMP
tar -cf tiny.tar C:\Windows\System32\drivers\etc\hosts
mkfs.erofs --tar=f tiny.erofs tiny.tar
```

**Proves:** the formatter works on this machine, with a two-line reproducer instead of a whole
daemon. This step exists because the first Windows run failed here and it took isolating it from
containerd to see that.

If this reports `failed to initialize diskbuf: No space left on device` while the disk is plainly
not full, you are running a `mkfs.erofs.exe` from before
`packaging/mkfs-erofs-windows/patches/0001` — it wants `C:\tmp` to exist. `mkdir C:\tmp` works
around it; a newer bundle fixes it.

### 3. Start containerd in a console — do not install it as a service

```powershell
containerd.exe --config C:\boks-test\config.toml --log-level debug
```

Leave it running and open a second shell for the rest. A console process is deliberate: you see
the plugin-loading log live, including the info-level line where a plugin skips itself, which a
service would bury in the event log.

**Proves:** containerd initialises its plugin graph on Windows, unelevated. Watch for lines
mentioning `erofs`. A skip looks like `failed to check mkfs.erofs availability` or
`mkfs.erofs does not support tar mode`.

Every `ctr` command below needs the same pipe:

```powershell
$env:CONTAINERD_ADDRESS = '\\.\pipe\boks-containerd'
```

### 4. Did the erofs plugins load — and did anything else fail?

```powershell
ctr.exe plugins ls | Select-String erofs
```

**Proves:** whether registration actually took. Two rows are expected:

```
io.containerd.snapshotter.v1    erofs    windows/amd64    ok
io.containerd.differ.v1         erofs    linux/amd64      ok
```

`ok` means the plugin initialised — **not** that anything will call it. That distinction is
patch `0002` above, and it is not observable from this table.

`skip` on the differ means `mkfs.erofs.exe` was not on PATH when containerd started; fix step 0
and restart. `error` on it usually means something *else* failed and took it down with it, so
read the whole table, not the filtered rows:

```powershell
ctr.exe plugins ls
```

### 5. Pull a Linux image through the erofs snapshotter

```powershell
ctr.exe -n test images pull --platform linux/amd64 --snapshotter erofs docker.io/library/alpine:latest
```

**Proves:** the whole unpack path end to end — containerd fetches a Linux image on a Windows
host, hands each layer's tar to the erofs differ, the differ runs `mkfs.erofs.exe --tar=f`, and
the erofs snapshotter commits the result. This is the single most informative command in the
list, and it now exercises the transfer service, which is the path patch `0003` fixes.

If it fails with `no unpack platforms defined`, the transfer service found no matching
`unpack_config` — either the config was not passed or this binary predates `0003`. `--local`
bypasses the transfer service entirely and is the useful A/B:

```powershell
ctr.exe -n test images pull --local --platform linux/amd64 --snapshotter erofs docker.io/library/alpine:latest
```

If `--local` works and the default does not, the problem is in `unpack_config`, not in the
differ.

### 6. Confirm real EROFS blobs landed on disk

```powershell
ctr.exe -n test images ls
$blobs = Get-ChildItem -Recurse -Filter layer.erofs C:\cdtest\root\io.containerd.snapshotter.v1.erofs
$blobs
```

**Proves:** the layers were genuinely formatted as EROFS rather than silently unpacked by some
other differ. One `layer.erofs` per image layer. Check one is really an EROFS image — the
superblock magic `0xE0F5E1E2` sits at offset 1024:

```powershell
$b = [System.IO.File]::ReadAllBytes($blobs[0].FullName)
'{0:X2}{1:X2}{2:X2}{3:X2}' -f $b[1027],$b[1026],$b[1025],$b[1024]
```

**Proves:** `E0F5E1E2` means a real EROFS superblock, written by `mkfs.erofs.exe` on Windows.

### 7. Check nothing was left behind in TEMP

```powershell
Get-ChildItem $env:TEMP -Filter 'ero*.tmp'
```

**Proves:** `packaging/mkfs-erofs-windows/patches/0001` cleans up after itself. Empty is the
expected result; before that patch, every layer left a temp file behind — under `C:\tmp`, which
is also where to look if you are testing an older bundle.

### 8. Clean up

```powershell
ctr.exe -n test images rm docker.io/library/alpine:latest
```

## Verified here, on Linux, 2026-08-14

Cross-compiled and inspected. **No Windows binary has been executed** — this is a Linux machine.
Where a claim below was checked by running something, it says so.

| Check | How | Result |
| --- | --- | --- |
| the three patches apply to pristine v2.3.3 | `git apply --check` | clean |
| PE32+ x86-64 | `objdump -f` | `pei-x86-64` for `containerd.exe` and `ctr.exe` |
| erofs plugins linked in | `strings` | `plugins/diff/erofs/plugin`, `plugins/snapshots/erofs/plugin` present, both arches |
| existing plugins kept | `strings` | `plugins/snapshots/windows` still present |
| **diff order names erofs, first** | `assert-diff-order.py` reads the `[]string` out of `.data` | `['erofs', 'windows', 'windows-lcow']`; an unpatched build of the same tree has `['windows', 'windows-lcow']` and fails the same check |
| a skipped differ does not kill the diff service | **executed** — `go test ./plugins/services/diff/...` | passes, on Linux, with the same platform-neutral code Windows runs |
| `config.toml` decodes to what it claims | **executed** — containerd's own `srvconfig.LoadConfig` + `Decode` | order `[erofs windows windows-lcow]`, both `unpack_config` entries, `optional=true` on erofs |
| the Windows-only assertions type-check | `GOOS=windows go vet` | clean |

No binary sizes are quoted. They move with the Go toolchain in the runner image and a stale
number in a README is worse than no number; the workflow prints the real `sha256sum` and byte
count into its job summary.

### What this evidence does not cover

`assert-diff-order.py` proves the daemon will *start* with erofs in its diff order. It does not
prove the erofs differ then does anything useful with a layer, and nothing here can: that
requires Windows. Steps 5 and 6 above are the only test of it.

Likewise `plugins ls` reporting `ok` remains a weak signal by construction, which is why step 5
and not step 4 is the one that answers the question.
