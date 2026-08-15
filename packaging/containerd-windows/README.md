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

Six, in `patches/`. The first registers the plugins; `0002` and `0003` make them reachable,
which turned out to be a separate problem; `0004` is about `ctr`, not the daemon, and only
matters once you try to *run* a container rather than unpack one. `0005` is not about EROFS at
all — it is a silent-corruption bug in containerd's mount manager that Windows makes certain to
hit, found on the first end-to-end run. Neither is `0006`, which is the reason an unelevated
containerd could not create a task bundle on Windows at all.

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

### `0004` — let `--platform` choose the spec's platform in `ctr run` on Windows

`0001`–`0003` get a Linux image unpacked. `0004` is about the next command, and it was found by
reading `ctr`, not by running it.

On a Windows host, `ctr run` decides which platform's OCI spec to generate from the
**snapshotter name**, and from nothing else — `cmd/ctr/commands/run/run_windows.go`:

```go
snapshotter := cliContext.String("snapshotter")
if snapshotter == "windows-lcow" {
    opts = append(opts, oci.WithDefaultSpecForPlatform("linux/amd64"))
    opts = append(opts, oci.WithRootFSPath(""))     // clear the rootfs section
} else {
    opts = append(opts, oci.WithDefaultSpec())
    opts = append(opts, oci.WithWindowNetworksAllowUnqualifiedDNSQuery())
    opts = append(opts, oci.WithWindowsIgnoreFlushesDuringBoot())
}
```

`--snapshotter erofs` takes the `else`. The container gets a spec with `s.Windows != nil` and
`s.Linux == nil`, so `oci.WithImageConfigArgs` takes its **Windows** branch for a Linux image,
and the shim is handed a `config.json` with no `linux` section at all.

The `--platform` flag `ctr run` advertises — *"Run image for specific platform"* — does not
help: it is read only by `run_unix.go` (for the spec at lines 104–119 and 141, and for image
resolution at 162–168). `run_windows.go` never mentions it. It is accepted and ignored.

Nor is there a way around it. `ctr run --config <spec.json>` is the only other route to a spec
of your choosing, and on the Windows path it does not apply `containerd.WithNewSnapshot`, so the
task is created with **no rootfs mounts** — `client/container.go`'s `handleMounts` adds nothing
when `SnapshotKey` is `""`. A correct spec and a rootfs are mutually exclusive on Windows today.

So `0004` makes `--platform` select the spec's platform here as it already does on Unix, and
constrain image resolution the same way — `client.GetImage` matches the *host* platform, and a
`linux/amd64` image pulled onto a Windows host has no `windows/amd64` manifest to find, so
without that the run fails at `image.Config` instead.

Nothing that works today changes: with no `--platform`, `windows-lcow` still implies
`linux/amd64` and still clears the rootfs path (an LCOW property, not a Linux one, so it stays
keyed to the snapshotter), and every other invocation still gets the Windows spec plus the two
Windows-only options — which are also still applied when `--platform` names a Windows platform.

#### The `windows` section has to come back out again

That much was written before anything ran. On 2026-08-14 it ran, and the answer to the last
paragraph's open question arrived: the guest boots, mounts the container's rootfs, and then
`crun` refuses the spec.

```
ctr: failed to create shim task: OCI runtime create failed: load `config.json`:
     Required field 'layerFolders' not present
```

A Linux spec generated on a Windows host carries a `windows` section.
`generateDefaultSpecWithPlatform` (`pkg/oci/spec.go:97-100`) ends its non-Windows branch with

```go
if err == nil && runtime.GOOS == "windows" {
    // To run LCOW we have a Linux and Windows section. Add an empty one now.
    s.Windows = &specs.Windows{}
}
```

— a condition on the *host's* OS that says nothing about the runtime that will read the spec.
`omitempty` does not drop it: `specs.Spec.Windows` is a **pointer**, and to `encoding/json` a
non-nil pointer to a zero struct is not empty. `specs.Windows.LayerFolders` has no `omitempty`
of its own, so what is marshalled — and what nerdbox forwards into the guest verbatim
(`internal/shim/task/bundle/bundle.go`) — is `"windows":{"layerFolders":null}`.

`crun` parses that with libocispec, whose generated parser checks a schema's required fields
whenever the object holding them is present (`src/ocispec/sources.py`, `emit_make_body`), and
runtime-spec's `schema/config-windows.json` lists `layerFolders` as required. The `windows`
object is optional; its contents are not. So the empty, harmless-looking object is fatal, and
a Linux host never meets it because a Linux host never generates one.

`0004` removes the section rather than filling it in: a Linux guest has no layer folders, and a
path invented to satisfy a parser would be data the guest has to ignore — worse than the error,
because it would not be an error. Only for a non-Windows platform with a snapshotter that is not
`windows-lcow`; LCOW keeps its section, because the runhcs shim is what fills `LayerFolders` in.

**The order is load-bearing**, and it is the opposite of the obvious one. containerd's own
image-config options read `s.Windows != nil` as *"this is LCOW, the guest resolves users and
groups for itself"*: `oci.WithAdditionalGIDs` returns early (`spec_opts.go:873-876`) and
`oci.WithUser` parks the image's user string in `Process.User.Username`
(`spec_opts.go:624-627`). Both alternatives mount the image's snapshot **on the host**, and
`mount_windows.go` rejects any mount whose type is not `windows-layer`. Remove the section
before `oci.WithImageConfig` has run and `Required field 'layerFolders' not present` becomes
`invalid windows mount type: erofs`, one step earlier. So it is applied last, after every other
spec option in `run_windows.go`.

The flags that write into the Windows section and nowhere else — `--cni`, `--isolated`,
`--cpu-count`, `--cpu-shares`, `--cpu-max`, `--device` — would be discarded along with it, so
they are now **refused** rather than ignored on a non-Windows spec. `--memory-limit` is not
among them: `oci.WithMemoryLimit` writes both sections when both exist, and the Linux one
survives. Nothing could have reached this combination before `0004`, so no existing invocation
starts failing.

`withoutWindowsSection` lives in `cmd/ctr/commands/run/guestspec.go`, a file with **no build
constraint**, purely so that it can be tested somewhere other than Windows. Its tests build the
spec a Windows host would have produced by hand — the condition is the host's own OS, which no
test can choose — and assert both halves: that the section marshals as the text `crun` rejected,
and that removing it disturbs nothing else. `containerd-windows.yml` runs them by name and fails
if any of them did not run, because `go test -run` on a pattern that matches nothing exits 0.

**What is verified and what is not.** The failure was measured on real hardware; the mechanism
is read from containerd, runtime-spec and libocispec; the removal is exercised by tests on
Linux. That a container then *starts* has not been observed on any machine.

Boks' own spec generation had the identical defect and is fixed separately, in
`internal/sandbox`, with its own tests — the bug is in the containerd library both callers
share, so patching `ctr` fixes only `ctr`.

### `0005` — check the superblock before believing an image is formatted

This one is a bug, not a port. It was found by the first end-to-end run on real hardware, and it
silently corrupts.

`core/mount/manager/mkfs.go` decided an image was already formatted by calling `Stat` on it.
Where the check belonged there was a comment:

```go
if _, err := r.Stat(subpath); err == nil {
    // Check magic number
} else if os.IsNotExist(err) {
```

That is not a harmless TODO, because of the order of operations in the branch it guards. The
format path **creates the file and truncates it to the requested size, and only then runs
mkfs.** So every way mkfs can fail leaves behind a file of exactly the size the next call
expects, containing nothing but zeroes. The next call `Stat`s it, finds it, skips the format,
and returns the mount. **The guest is handed a zero-filled image presented as ext4.**

Windows makes that certain rather than unlikely. There is no `mkfs.ext4` for Windows in any
packaged form, and the erofs snapshotter asks for `mkfs/ext4` with
`X-containerd.mkfs.size=67108864` on every active snapshot, so the format step fails by
construction on the first `ctr run` and every attempt after it "succeeds". Measured on the
Windows 11 machine, on the file the failing run left behind:

```
snapshots\3\rwlayer.img   exists: True   size: 67,108,864
ext4 magic @1080 : 0x0000        (a real ext4 superblock is 0xEF53)
=> formatted     : False
```

The tester did not hit the consequence only because the next thing they did was overwrite that
file with a pre-formatted one. Do it in the other order and the guest mounts 64 MiB of zeroes.

**The fix is deliberately asymmetric about who owns the file**, because there are two different
dangers here and they pull in opposite directions.

- **Read the superblock.** `superblockProbes` records, per filesystem, where the magic lives and
  what it is. An image carrying the right magic is accepted exactly as before. An image that
  does not is **refused, and left untouched** — we did not create it, we cannot know what it is,
  and reformatting or deleting someone's data would trade a corruption bug for a data-loss one.
  The error says what is missing and that removing the file will get one created.
- **Delete what we created and could not format.** That file is unambiguously ours and contains
  only zeroes we just wrote. `O_EXCL` replaces `O_TRUNC` on the create so that "this file is
  ours" is a property of the open rather than an inference from the `Stat` several lines
  earlier. Without the removal, the superblock check above would make a retry fail forever
  instead of retrying.

**This does not break the pre-formatted-image workflow, which is the whole reason the bundle
ships `rwlayer-64m.img`.** That image is made by `mkfs.ext4` on the CI runner, so it carries a
real superblock and is accepted. What no longer works is dropping in a file that is merely the
right *size* — which never worked, it just failed silently instead of loudly.

The offsets and magics, and where each was checked:

| fs | magic | offset | source |
| --- | --- | --- | --- |
| ext2, ext3, ext4 | `53 ef` (`EXT2_SUPER_MAGIC` 0xEF53, `__le16`) | 1080 | kernel docs give `s_magic` at `0x38` of a superblock that starts 1024 bytes in, so 1024 + 56; **confirmed by running `mkfs.ext4` on a 64 MiB image here and reading offset 1080** |
| xfs | `XFSB` (`XFS_SUPER_MAGIC` 0x58465342, big-endian) | 0 | `/usr/include/linux/magic.h`, plus XFS's documented `__be32` metadata magics; corroborated by `file(1)` recognising `XFSB` at offset 0 and not at 512. **No `mkfs.xfs` was run** — no such binary on this machine |

One behaviour change beyond the bug: an existing image for a filesystem the format path cannot
create — anything but ext2/3/4 and xfs — is now refused with the same `unsupported filesystem`
error the format path already gives, rather than accepted because a file happened to be there.

### `0006` — link the bundle's work directory with a junction on Windows

The last thing that made an elevated daemon compulsory, and the only one of these six that
stops an unelevated containerd before it does anything at all. `NewBundle` ends by symlinking
the task's working directory into the bundle, and unprivileged Windows will not create a
symlink. Every task bundle, every time.

`0006` creates a **junction** instead, falling back to the symlink, and the reasoning — why a
junction, why the fallback, why this cannot silently leak working directories, and what
remains unverified — is in "Elevation, and the choice you no longer have to make" below,
because that is the section a reader arrives at with the question.

Two things about its shape are worth stating here, next to the other patches:

- **`!windows` is untouched.** `worklink_other.go` calls `os.Symlink(work, link)` and nothing
  else. The symlink is the right link wherever making one is unprivileged, and the runc shim
  resolves `<bundle>/work` by *walking* that path rather than reading it, so Linux must keep
  a link the path parser follows.
- **The reparse buffer is in a file with no build constraint** (`core/runtime/v2/worklink.go`),
  for the same reason `0004`'s `guestspec.go` is: it is pure byte-shuffling, the one part
  that can be wrong quietly, and the machine that cannot compile it is the machine that needs
  it. `worklink_test.go` checks it against the layout in [MS-FSCC] 2.1.2.2 on Linux —
  including that the tag is a mount point and not a symlink, that `ReparseDataLength` counts
  the right bytes (getting it wrong buys nothing but `ERROR_INVALID_PARAMETER`), and that
  decoding the result the way `os.Readlink` does returns the path that went in.

The pin is `CONTAINERD_VERSION` — **v2.3.3**, matching the containerd that the nerdbox revision
in `packaging/nerdbox/NERDBOX_REV` vendors.

**All six belong upstream, not here.** Delete this directory once containerd takes them.
`0005` most of all: it is a data-corruption fix that has nothing to do with Boks, Windows is
merely where it was impossible to miss. `0006` is close behind — it is not a Boks problem
either, it is every unelevated Windows containerd's problem, and the earlier claim on this
page that it was "upstream's design call, not something to fork here" was a way of not
choosing.

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
problem, not containerd's, and it has been attempted once. `docs/windows-e2e.md` is the
procedure, and it turned up three more containerd-side obstacles: the OCI spec's platform (patch
`0004` above); the writable layer, which the erofs snapshotter asks containerd to format with
`mkfs.ext4`, a binary that does not exist on Windows; and the way containerd handled the failure
of that format, which is patch `0005`. There is a fourth — `NewBundle` creates a symlink for
every task, which unprivileged Windows will not do — and it is patch `0006`. See "Elevation,
and the choice you no longer have to make" below.

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


## Elevation, and the choice you no longer have to make

**Read this whole section if you have used an earlier bundle.** The advice has changed twice.

- It first said *run everything unelevated*. That was wrong for anything that creates a task.
- It then said *elevate, or turn on Developer Mode, or stop before `ctr run`* — correct for
  the binaries it was written about.
- Patch `0006` is the answer to it, and this section now says: **run unelevated**, and be
  aware that **nobody has yet watched an unelevated daemon create a task bundle.** The patch
  is verified as far as a Linux machine can verify it, which is not far enough to claim it
  works.

Note what elevation was ever needed *for*. It was the **containerd daemon**, and only the
daemon, because the daemon is what creates task bundles. `boks create`, `boks ls`,
`boks inspect` and `boks rm` never touched this and always worked as an ordinary user.

### What it was

`core/runtime/v2/bundle.go:103`, in `NewBundle`, for **every task bundle**, unconditionally:

```go
// symlink workdir
if err := os.Symlink(work, filepath.Join(b.Path, "work")); err != nil {
    return nil, err
}
```

Creating a symlink on Windows requires `SeCreateSymbolicLinkPrivilege`, which an ordinary user
does not hold. Go already does the one thing that can help — `os/file_windows.go` passes
`SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE`, and retries without it if that fails — but
**Windows honours that flag only when Developer Mode is on**, which is machine-wide, grants
unprivileged symlink creation to every process run by every user, and is commonly disabled by
policy (`AllowDevelopmentWithoutDevLicense`) on managed machines. So on a stock Windows 11
desktop, an unelevated `ctr run` failed inside containerd before any shim was launched.
Measured on the test machine:

```
New-Item -ItemType SymbolicLink   ->  "Administrator privilege required"
mklink /J  (a junction)           ->  succeeds
```

That second line is the entire patch.

### What `0006` does

A **junction** — a directory mount point — instead of a symlink, on Windows only. It is the
same thing for this purpose and needs no privilege at all. Windows has no `CreateJunction`: a
junction is an empty directory carrying an `IO_REPARSE_TAG_MOUNT_POINT` reparse point, made
with `CreateFile` plus `DeviceIoControl(FSCTL_SET_REPARSE_POINT)`, with the handle flags the
Go standard library's own `os.TestDirectoryJunction` uses.

**The junction is tried first and `os.Symlink` is kept as the fallback**, rather than replaced.
A junction is a strictly *less* capable link: its target must be a fully qualified path on a
local volume, so a `--root` on a UNC share cannot be one and can be a symlink. Ordering it this
way means every configuration that works today still works, the unelevated one now works too,
and both failing produces one error naming both causes. There is no configuration in which the
patch removes an option.

**An earlier revision of this page said no such patch was possible**, on the grounds that the
link has to appear inside `b.Path` and `b.Path` is created by `os.Mkdir` with no `IsExist`
tolerance twenty lines earlier, so there is nowhere to stand. That reasoning was about
*pre-creating* the junction from outside containerd — which is indeed impossible, and is the
trick that fixes the `--root` ACL problem further down. It says nothing about containerd
creating the junction itself, which is what `0006` does.

### Why this cannot leak working directories

The link has exactly one consumer, `Bundle.Delete` (`bundle.go:133`):

```go
work, werr := os.Readlink(filepath.Join(b.Path, "work"))
...
if werr == nil { return atomicDelete(work) }
```

A failed `Readlink` is *tolerated* — the working directory is simply not removed. So the
dangerous failure here is not an error; it is a `work` that exists and is not a link, which
would leave every task's working directory behind under `--root` and fail nothing. Three
things stand against that:

- **`os.Readlink` reads junctions.** `os/file_windows.go`'s `readReparseLinkHandle` handles
  `IO_REPARSE_TAG_MOUNT_POINT` alongside `IO_REPARSE_TAG_SYMLINK` and passes the substitute
  name through `normaliseLinkPath`; `os.TestReadlink` covers `mklink /J` explicitly, with an
  absolute drive target, and runs on Go's Windows builders. Read out of the Go 1.26.3 source
  tree containerd v2.3.3's `go.mod` selects.
- **A half-made junction is removed, not left.** `createWorkDirJunction` makes the directory
  and then sets the reparse point; if the second step fails it removes the directory before
  returning the error, so `NewBundle` fails loudly rather than returning a bundle with a
  plain directory called `work` in it.
- **Nothing else on Windows touches it.** The only other consumer of `<bundle>/work` in
  containerd is the runc shim (`cmd/containerd-shim-runc-v2/runc/container.go:127`), which is
  `//go:build linux`. nerdbox's host shim does not read it either. So the junction never has
  to be *evaluated*, only read — which matters, because `fsutil behavior` can disable
  evaluation of some link classes without affecting `FSCTL_GET_REPARSE_POINT`.

`os.RemoveAll` does not descend into a junction either: a mount point is a *name-surrogate*
reparse point, so `fileStat.mode()` withholds `ModeDir` from it exactly as it does from a
symlink, and teardown behaves identically. (Its `Lstat` mode is `ModeIrregular` rather than
`ModeSymlink` — the one observable difference, and nothing in containerd on Windows looks.)

Even in the worst case the loss would be bounded rather than permanent: `cleanupWorkDirs`
(`core/runtime/v2/shim_load.go:242`) reaps working directories with no loaded shim at every
daemon start. That makes a missing link quieter, not better.

### If you still want to run elevated

You can, and the security note below still applies in full — it is about `--root`, not about
symlinks, and nothing in `0006` changes it. What is no longer true is that you *must*.

### If you do run elevated: `--root` must be created by the elevated daemon

**Do not point an elevated containerd at a `--root` that an unprivileged user created.** This
is the one thing in this section that is a security boundary rather than a convenience.

`MkdirAllWithACL` applies its protected DACL only to path components **it** creates, and
returns early on anything that already exists (see the next section for the exact mechanism).
So a root pre-created by an ordinary user keeps that user's inherited permissions, and the
elevated daemon then fills it with the content store, the snapshotters' root filesystems and
the bolt metadata database — all writable by that unprivileged user. They can put a binary into
a layer a later container executes, or repoint an image's metadata at content they control,
against a daemon running as an administrator. That is precisely the escalation the `P`
(PROTECTED) in containerd's SDDL exists to prevent, reached by a different route.

Elevated, you do not need to pre-create anything: `MkdirAllWithACL` succeeds and the ACL it
writes names `Administrators` and `SYSTEM`, which is what the daemon is. Just start it.

**`new-containerd-root.ps1` is for the unelevated case only.** Its whole job is to hand you a
root you can write to without the ACL, and that is exactly the wrong shape for a privileged
daemon. If you switch to running elevated, use a *different* root directory that the elevated
daemon creates for itself — not the one the script made, which an unprivileged account still
owns.

### The `opt` and `cri` plugin failures were not unrelated

Unelevated, `ctr plugins ls` shows two failures — `io.containerd.internal.v1.opt` and `cri` —
and this project described them as known and unrelated. **Elevated, both disappear.** They were
artefacts of running unelevated, not independent problems.

That is consistent with what they want: `opt` and `cri` default to paths under
`%ProgramData%\containerd` and `C:\Program Files\containerd`, neither of which an ordinary user
can create — the same root cause as the `--root` problem below, arriving at two more plugins.
Neither cascades either way, so it changes no advice; it changes what "two known unrelated
failures" means, which was simply not true.

## The root directory must exist first

**Create `root` and `state` before starting `containerd.exe`.** This is a hard prerequisite,
not housekeeping, and it is the one thing on this page that will cost you a whole run if you
skip it:

```powershell
.\new-containerd-root.ps1        # ships in the bundle; or: mkdir C:\cdtest\root, C:\cdtest\state
```

### What goes wrong without it

Unelevated, on a machine where those directories do not already exist, containerd creates them
itself and then cannot write inside them. Measured on Windows 11: **43 plugins failed**, the
first error being

```
failed to mkdir "C:\cdtest\root\io.containerd.content.v1.content": Access is denied.
   id=io.containerd.content.v1.content
```

and the ACL on the directory containerd had just created being

```
NT AUTHORITY\SYSTEM         FullControl   Allow
BUILTIN\Administrators      FullControl   Allow
Owner: <the unelevated user>
```

The owner is the user. There is no ACE for the user.

### Why containerd does that

`cmd/containerd/server/server.go` creates both directories through a Windows-specific helper:

```go
if err := sys.MkdirAllWithACL(config.Root, 0o700); err != nil {   // :70
if err := sys.MkdirAllWithACL(config.State, 0o711); err != nil {  // :81
```

`pkg/sys/filesys_windows.go` is a clone of `os.MkdirAll` that additionally applies one SDDL to
**every path component it creates**:

```go
// :31
const SddlAdministratorsLocalSystem = "D:P(A;OICI;GA;;;BA)(A;OICI;GA;;;SY)"
```

Read that left to right:

| | |
| --- | --- |
| `D:` | this is the DACL |
| `P` | **PROTECTED** — do not inherit any ACE from the parent directory |
| `(A;OICI;GA;;;BA)` | Allow, Object+Container Inherit, `GENERIC_ALL`, `BUILTIN\Administrators` |
| `(A;OICI;GA;;;SY)` | Allow, Object+Container Inherit, `GENERIC_ALL`, `NT AUTHORITY\SYSTEM` |

`P` is the load-bearing letter. Without it the directory would inherit whatever the parent
grants — under `C:\` that includes an inheritable `Modify` for Authenticated Users — and the
hardening would be undone by wherever the admin happened to point `--root`. The Unix mode
argument is discarded outright: the signature is `MkdirAllWithACL(path string, _ os.FileMode)`,
so the `0o700` and `0o711` in `server.go` mean nothing here.

**This ACL is correct for the deployment containerd on Windows is built for.** That deployment
is a service under the SCM running as LocalSystem. `root` holds the content store (image
blobs), the snapshotters (container root filesystems) and the bolt metadata database; `state`
holds shim bootstrap params and pipe addresses. A user who can write there can put a binary
into a layer that a later container executes, or repoint an image's metadata at content they
control. That is arbitrary code execution as whatever the daemon runs containers as. The ACL is
the thing standing in the way, and it is the same SDDL — and the same reasoning — as Moby's
`system.MkdirAllWithACL` for the Docker data root.

What it is *not* is conditioned on the identity containerd is running under. Unelevated,
containerd is neither `BA` nor `SY`, so it writes a DACL that excludes itself.

### Why the failure names a plugin instead of a permission

Both `MkdirAllWithACL` calls in `server.go` **succeed**. Creating a directory is a right on the
*parent*, and the user has that. The denial arrives later, the first time something writes
*inside* `root`, through a plain `os.MkdirAll` with no ACL involved:

```go
// plugins/content/local/store.go:94
if err := os.MkdirAll(root, 0755); err != nil {
    return nil, fmt.Errorf("failed to mkdir %q: %w", root, err)
}
```

`io.containerd.metadata.v1.bolt` requires the content store, and essentially every service
requires bolt — the same cascade the `disabled_plugins` block in `config.toml` exists to
prevent for a different cause. Hence 43 failures and an `id=` pointing at the content store.

### Why pre-creating works, exactly

`mkdirall`'s first act is an early return for a directory that is already there:

```go
// pkg/sys/filesys_windows.go:65
dir, err := os.Stat(path)
if err == nil {
    if dir.IsDir() {
        return nil
    }
    ...
```

An existing directory is accepted **unchanged** — no SDDL is applied, no ACL is inspected. A
`root` you created yourself carries ordinary inherited permissions, and everything containerd
builds underneath it is made with plain `os.MkdirAll`, which inherits from it. That is the
whole mechanism; it is not a race and not luck.

Two consequences worth stating because both get guessed wrong:

- **Both directories need it, separately.** `MkdirAllWithACL` is called once for `root` and
  once for `state`, and it recurses — it *does* create missing parents, and it hardens each one
  it creates. So `C:\cdtest` itself gets the ACL too if containerd is the one to create it.
- **Relocating `root` does not help.** The early return skips only directories that already
  exist. Pointing `--root` at `%LOCALAPPDATA%\containerd\root` produces exactly the same
  lockout, because that leaf does not exist either. Pre-creation is the only fix short of
  patching containerd.

### If you are already locked out

The user is the **owner**, and a Windows owner is implicitly granted `WRITE_DAC` by the access
check even when the DACL names nobody. So the directory is recoverable without an
administrator, unelevated (*inferred from Windows access-check semantics, not from anything we
have run*):

```powershell
icacls C:\cdtest\root  /grant "*$([System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value):(OI)(CI)F"
icacls C:\cdtest\state /grant "*$([System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value):(OI)(CI)F"
```

`new-containerd-root.ps1` does this for you: it creates whatever is missing, then proves each
directory is usable by creating and deleting a probe directory inside it — the same operation
containerd performs, rather than a second, wrong reimplementation of the Windows access check —
and repairs the DACL only if that probe fails.

The SID form (`*S-1-5-…`) rather than the account name is deliberate: `icacls` matches account
and group names literally and they are localised.

### Why Boks does not patch this out

The obvious alternative is a fourth patch: have an unelevated containerd add an ACE for its own
token user, so that a root it creates is one it can use. It was considered and rejected. The
honest comparison is not "secure versus insecure" — both routes end with a directory the user
can write to, which is unavoidable, because containerd running as that user must be able to
write there. The difference is **who decides, and how long the decision lasts.**

1. **The ACE outlives the run that justified it.** `MkdirAllWithACL` is a no-op on an existing
   directory, so an ACE written once is permanent. Install containerd as a service afterwards,
   or point a LocalSystem daemon at the same `--root`, and an unprivileged user now has
   inheritable (`OICI`) full control over a privileged daemon's content store and metadata
   database — and over everything that daemon creates inside it from then on. A patch made for
   the unelevated case would silently degrade the elevated one. That is precisely the
   escalation the `P` in the SDDL exists to prevent, arrived at by a different route.
2. **"Which SID" is not a one-line decision.** Elevated administrators have split tokens;
   service accounts are not LocalSystem; a `runas` shell is a different user from the one who
   will use the daemon. Every one of those writes a different persistent ACE.
3. **Pre-creating is chosen locally, by a person, on a directory they own.** The permissive
   permissions land on one test root on one desktop, put there deliberately. A patched binary
   applies the same relaxation on every host it is ever run on, including hosts where the root
   is later reused by a service.
4. **We would have to carry it, and argue for it.** The other four patches in `patches/` are
   things we want upstream and expect containerd to take. "Relax a deliberate security control
   so our test setup needs one command fewer" is not that, and Boks — whose entire premise is
   isolation — is a bad place for it to originate.

If containerd upstream decides to support a non-service, non-elevated Windows daemon properly,
the right shape is probably an explicit opt-in (a config key, or a documented `--root` that the
operator prepares) rather than an implicit ACE. That is upstream's call, and this directory is
not the place to pre-empt it.

## Testing it on Windows

Nothing here needs the nerdbox shim, a guest kernel, or a working microVM. This tests one
question — *can containerd on Windows unpack a Linux image with EROFS?* — and nothing else.

Download the `containerd-windows-amd64-bundle` artifact from the workflow run. It contains
`containerd.exe`, `ctr.exe`, `mkfs.erofs.exe`, `config.toml`, `new-containerd-root.ps1` and
`rwlayer-64m.img`. The last of those is not needed for this test — it is a pre-formatted 64 MiB
ext4 image that only matters when you go on to *run* a container, and
[`docs/windows-e2e.md`](../../docs/windows-e2e.md) explains why it has to be made on Linux.

**Run all of this unelevated, pass the config, and create the directories first.** All three
matter, and each one costs you a run if you skip it.

Unelevated is correct for this test and, since patch `0006`, is meant to be correct past it
too: `NewBundle`'s link is a junction now, which an ordinary user can create. Nothing in the
steps below reaches a task bundle either way — they unpack an image and stop — so this test
says nothing about whether that patch works. If you go on to `ctr run`, read "Elevation, and
the choice you no longer have to make" above first; and if you decide to run elevated anyway,
do **not** reuse the `--root` that step 0 creates here.

- **Create `root` and `state` before starting containerd** — `.\new-containerd-root.ps1`. See
  "The root directory must exist first" above for the failure this avoids and why the fix is
  exact rather than lucky. This is step 0 below and it is not optional.
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

### 0. Put the binaries in one directory on PATH, and create root and state

```powershell
mkdir C:\boks-test
# unzip the bundle into it: containerd.exe, ctr.exe, mkfs.erofs.exe, config.toml,
# new-containerd-root.ps1
$env:Path = "C:\boks-test;$env:Path"

C:\boks-test\new-containerd-root.ps1     # unelevated, as the user who will run containerd
Get-Command mkfs.erofs
mkfs.erofs -V
```

**Proves:** `root` and `state` exist and this user can write inside them, so containerd will
accept them rather than replace them with a DACL that excludes you; and that `mkfs.erofs.exe`
resolves by bare name (containerd calls `exec.Command("mkfs.erofs", ...)`, relying on `PATHEXT`
to add `.exe`) and executes at all.

Both halves must happen **before** step 3 starts containerd: a late `mkfs.erofs.exe` means the
differ skips itself, and a missing `root` means containerd hardens it against you.

If the script is blocked by execution policy, either run
`powershell -ExecutionPolicy Bypass -File C:\boks-test\new-containerd-root.ps1` or just do the
`mkdir` — the script's extra value is the writability probe and the repair path, not the
directory creation:

```powershell
mkdir C:\cdtest\root, C:\cdtest\state
```

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

## Verified here, on Linux, 2026-08-14 and 2026-08-15

Cross-compiled and inspected. **No Windows binary has been executed** — this is a Linux machine.
Where a claim below was checked by running something, it says so.

| Check | How | Result |
| --- | --- | --- |
| all six patches apply to pristine v2.3.3, in order | `git apply --check`, then `git apply`, on a fresh v2.3.3 clone | clean |
| PE32+ x86-64 | `objdump -f` | `pei-x86-64` for `containerd.exe` and `ctr.exe` |
| erofs plugins linked in | `strings` | `plugins/diff/erofs/plugin`, `plugins/snapshots/erofs/plugin` present, both arches |
| existing plugins kept | `strings` | `plugins/snapshots/windows` still present |
| **diff order names erofs, first** | `assert-diff-order.py` reads the `[]string` out of `.data` | `['erofs', 'windows', 'windows-lcow']`; an unpatched build of the same tree has `['windows', 'windows-lcow']` and fails the same check |
| **the superblock probe table is in the PE** | `assert-mkfs-magic.py` reads the `[]superblockProbe` out of `.data` | `ext2@1080=53ef, ext3@1080=53ef, ext4@1080=53ef, xfs@0=58465342`, both arches; an unpatched build of the same tree has no such table and fails, as do a wrong offset, a byte-swapped magic and a reordered table |
| **the pre-fix behaviour is what we said it was** | **executed** — a throwaway test against unpatched `mkfs.go` | a 64 MiB zero-filled file was accepted and returned as a formatted `mkfs/ext4` mount; a failed format left 67,108,864 bytes behind and the retry accepted them |
| **the ext4 magic and offset are right** | **executed** — `mkfs.ext4` on a real image, then `hexdump -s 1024` | `53 ef` at offset 1080, matching the kernel's documented `s_magic` at `0x38` of a superblock starting at 1024 |
| **a formatted image is still accepted, and an unformatted one is not** | **executed** — `go test ./core/mount/manager/...` | passes; one test formats with the real `mkfs.ext4` and feeds the result back in, so the constants are checked against the formatter rather than against themselves |
| a skipped differ does not kill the diff service | **executed** — `go test ./plugins/services/diff/...` | passes, on Linux, with the same platform-neutral code Windows runs |
| `config.toml` decodes to what it claims | **executed** — containerd's own `srvconfig.LoadConfig` + `Decode` | order `[erofs windows windows-lcow]`, both `unpack_config` entries, `optional=true` on erofs |
| the Windows-only assertions type-check | `GOOS=windows go vet`, both arches | clean |
| **the empty `windows` section is what crun rejected, and `0004` removes it** | **executed** — `go test ./cmd/ctr/commands/run/... -run WindowsSection` | passes; one test asserts a Linux spec holding an empty `specs.Windows` marshals as `"windows":{"layerFolders":null}` — so `omitempty` on a pointer field is shown not to save it — and the others that the option removes it and leaves the rest of the spec alone |
| **the junction's reparse buffer has the layout Windows documents** | **executed** — `go test ./core/runtime/v2/ -run WorkDirJunction` | passes; the golden buffer in the test was written out by hand from [MS-FSCC] 2.1.2.2 and matched the encoder on the first run, which is the only reason it is evidence rather than a copy |
| **the junction path is in the shipped PE and reachable from `main`** | `assert-func-linked.py` reads the pclntab function-name table | `linkWorkDir`, `createWorkDirJunction`, `encodeMountPointReparseBuffer` present in `.rdata`, both arches; absent from an unpatched build of the same tree |
| **that check can fail** | **executed** — built pristine v2.3.3 for `windows/amd64` and re-ran all three scripts | `assert-func-linked.py --absent` passes, `assert-diff-order.py` and `assert-mkfs-magic.py` both exit 1. The `strings` greps a lazier check would have used pass on that same unpatched binary: `grep -c Reparse` → 22, `grep -c WorkDir` → 4 |
| **`os.Readlink` reads a junction, so `Bundle.Delete` still cleans up** | read out of the Go 1.26.3 source `containerd/go.mod` selects | `os/file_windows.go` `readReparseLinkHandle` handles `IO_REPARSE_TAG_MOUNT_POINT`; `os.TestReadlink` has a `junction_dir_drive_absolute` case that runs `mklink /J` and requires `Readlink` to return the absolute target. **Not executed here** — it runs on Go's Windows builders, not on this machine |
| **the platform that works still works** | **executed** — `go test ./core/runtime/v2/... -count=1` on Linux | passes, including the existing bundle tests, with `worklink_other.go` calling `os.Symlink` verbatim |

The xfs row is the one gap: `mkfs.xfs` is not on this machine, so `XFSB` at offset 0 is taken
from `/usr/include/linux/magic.h` and XFS's documented big-endian metadata magics, and
corroborated only by `file(1)` recognising it at offset 0 and not at 512. Nothing has formatted
an XFS image and read it back.

No binary sizes are quoted. They move with the Go toolchain in the runner image and a stale
number in a README is worse than no number; the workflow prints the real `sha256sum` and byte
count into its job summary.

### What this evidence does not cover

`assert-diff-order.py` proves the daemon will *start* with erofs in its diff order. It does not
prove the erofs differ then does anything useful with a layer, and nothing here can: that
requires Windows. Steps 5 and 6 above are the only test of it.

`assert-mkfs-magic.py` is weaker still in the same way: it proves the shipped binary carries a
table saying ext4's magic is `53 ef` at offset 1080, not that the code consulting that table
runs, and certainly not that a real Windows daemon rejects a real corrupt image. The Go tests
are what exercise the logic, on Linux, against platform-neutral code.

Likewise `plugins ls` reporting `ok` remains a weak signal by construction, which is why step 5
and not step 4 is the one that answers the question.

**And `0006` is the least covered of the six.** Everything above establishes that the binary
carries a junction path, that the bytes it would write match the format Microsoft documents,
and that the Go runtime reads such a link back. **Not one of those is Windows accepting the
`FSCTL_SET_REPARSE_POINT`.** Nobody has run `ctr run` against an unelevated daemon built from
these patches, on any machine. Until somebody does, "elevation is no longer required" is a
claim about code, not about a computer. The commands that would settle it are in
[`docs/windows-e2e.md`](../../docs/windows-e2e.md), step 4 onwards.
