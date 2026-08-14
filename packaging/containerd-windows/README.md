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
4. **We would have to carry it, and argue for it.** The other three patches in `patches/` are
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
`containerd.exe`, `ctr.exe`, `mkfs.erofs.exe` and `config.toml`.

**Run all of this unelevated, pass the config, and create the directories first.** All three
matter, and each one costs you a run if you skip it:

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
