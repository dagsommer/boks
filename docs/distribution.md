# Distribution: owning the runtime stack

Boks orchestrates a stack it does not contain. Today a user assembles that stack by hand, and
when a piece of it misbehaves the error names Boks. This document decides what Boks should
ship, how it should run what it ships, how a user updates it, and in what order to build all
that — with the costs, not the preferences.

The judgement it starts from is the maintainer's:

> Optimally we want to bundle everything, so the user doesn't have to run a bunch of random
> things they don't know what is. We would want to control the daemon like sbx does, because
> it's us they are going to blame if containerd or nerdbox doesn't work.

That is supported by evidence rather than taste. Five separate blockers have now cost real
time, and not one of them was Boks' code:

| # | What broke | Where it surfaced | What the user saw |
|---|---|---|---|
| 1 | containerd chowns its ttrpc socket to uid 0 | Linux, macOS | `chown …containerd.sock.ttrpc: operation not permitted` at startup |
| 2 | diff-service default order is `['walking']` | Linux | an image unpack failing, naming a differ |
| 3 | `MkdirAllWithACL` gives containerd's own root a protected DACL | Windows | `Access is denied` naming a plugin |
| 4 | cimfs snapshotter fails at init, bolt requires every snapshotter | Windows | ~40 plugins `error`, including erofs |
| 5 | erofs differ registered but not in the Windows diff order | Windows | `number of mounts should always be 1 for Windows layers` |

Every one of those is a line in a configuration file or a directory that needs creating first.
[Part 2](#part-2--boks-daemon) is now implemented for exactly that reason.

Two more have since been measured, and they are a *different* kind of problem — not
configuration but **version skew between the pieces**. They are the subject of
[Part 5](#part-5--versions-and-upgrades), and they change the answer to Part 1.

---

## Part 1 — embed containerd, or supervise it?

sbx embeds. `sbx.exe` carries containerd's `core/runtime/v2` in-process — a string scan finds
`core/runtime/v2`, `NewBundle`, `bundle.go` and `io.containerd.runtime.v2.task` in it, matching
our own `containerd.exe` and matching `sailor.dll` on none — and its MSI ships no
`containerd.exe`. sbx is one binary that *is* the daemon.

### The size question, measured

This was the argument everyone expects to be decisive, and it is not, because the number is
much smaller than intuition suggests. Measured on this branch, linux/arm64, with release flags
(`-trimpath -ldflags "-s -w"`):

| Binary | Size |
|---|---|
| `boks` as it is today | 23.8 MB |
| `boks` **plus** an embedded containerd daemon | 24.1 MB |
| **Delta** | **0.4 MB** |
| a standalone binary embedding only the daemon | 18.8 MB |
| Ubuntu's `/usr/bin/containerd` (already stripped) | 43.3 MB |
| Ubuntu's `/usr/bin/ctr` | 22.4 MB |

The embedded build links `cmd/containerd/server`, `core/runtime/v2`, the metadata (bolt)
plugin, the diff service, the walking and erofs differs, the erofs snapshotter and the task
service — the set Boks actually needs, not all of containerd.

Embedding costs **0.4 MB** because Boks already links the containerd *client*, and with it
gRPC, protobuf, bbolt and OpenTelemetry — which is most of what the daemon's 18.8 MB consists
of. Bundling instead costs a **43 MB** second binary, or ~30 MB for one built without CRI and
CNI. Embedding is the smaller option by roughly two orders of magnitude of delta, and it is
also the smaller option in absolute terms.

> **What that number is and is not.** It is a link, not a run. The probe compiles and the
> plugin registrations are reachable (blank imports keep `init` alive, so nothing is dead-code
> eliminated). **No embedded daemon has been started.** The claim is "the code links for
> 0.4 MB", nothing more.

### The rest of the ledger

| | Embed | Supervise a bundled containerd |
|---|---|---|
| **Binary size** | +0.4 MB to `boks` | +43 MB (or ~30 MB trimmed) per platform in the package |
| **Our six Windows patches** | compiled in — strictly easier. Patches 0001/0002 (diff order, graceful skip) become ordinary Go we own; the junction patch that removes the elevation requirement is applied to code we build anyway | must be re-applied to each containerd release and rebuilt in CI, which is `containerd-windows.yml` today and is a standing cost |
| **Upgrade story** | one artifact. `boks` and its daemon cannot be skewed against each other because they are the same file | two artifacts to keep in step, and the package manager may update one without the other |
| **Version skew (Part 5)** | collapses containerd-in-the-daemon and containerd-in-`boks` into one version. Does **not** fix shim skew — the shim is still a separate process | every combination is possible, including the measured `Yunix` failure |
| **A user already running containerd for Docker** | untouched — different endpoint, different root, different process. But we cannot *reuse* theirs without also keeping the supervise path | untouched for the same reason; and their binary can be used directly if we let them |
| **Debuggability** | worse. There is no `containerd --config x.toml` to run by hand, no `ctr` against a daemon you started yourself, and `containerd config dump` no longer exists. Every Windows debugging session so far has depended on exactly those | better. `packaging/containerd-windows/README.md` and `docs/windows-e2e.md` are both written around driving containerd and `ctr` directly |
| **Upstream drift** | we own a fork's worth of surface. containerd's plugin graph is not a stable API; a 2.4 that moves a plugin path breaks our build rather than our config | we track releases and re-apply patches, which is visible work rather than silent breakage |
| **Licensing** | Apache-2.0 either way. No obligation changes |
| **`boks` binary becomes a daemon** | the CLI a user types is now also a long-lived server. Startup cost, signal handling and crash semantics all become one process's problem | the CLI stays a CLI |

### The recommendation

**Supervise now; keep embedding open, and revisit it when the Windows patch series stops
moving.**

The size argument favours embedding and is not the deciding one. Three things decide it:

1. **Debuggability is the thing we cannot afford to lose right now.** Every Windows result in
   `docs/verification.md` was obtained by running `containerd.exe --config config.toml
   --log-level debug` and driving it with `ctr`. The platform is not finished — `virtio-net`
   is unproven there — so the debugging path is still load-bearing. Embedding removes it at
   precisely the wrong moment.
2. **The patch series is still moving.** `packaging/libkrun-windows/` carries 37 patches and
   gained several in the last week. Absorbing containerd into our binary while its patch set
   is still churning means every rebase lands in our own compile rather than in a `git apply`
   step CI already performs and checks.
3. **Supervising is a strict subset of the work.** Everything a supervised daemon needs —
   configuration generation, lifecycle, log capture, preflight — is needed by an embedded one
   too. None of Part 2 is wasted if we embed later; only the `exec.Command` is.

The honest cost of choosing supervise: **43 MB per platform in every package**, and a
permanent obligation to keep a patched containerd building against each upstream release.

### The single biggest risk in this decision

**Choosing "supervise" makes version skew a permanent, first-class failure mode, and skew
failures name nothing.**

Embedding would eliminate one axis of skew outright — the containerd `boks` links and the
containerd it drives become the same code, unable to disagree. Supervising keeps them
separate, and the evidence that this matters is not hypothetical: a shim linking containerd
2.3.3 against a daemon running 2.2.2 fails with `unsupported protocol: Yunix`, which is
protobuf framing rendered as letters and names no version, no shim and no protocol.

The mitigation is not "be careful". It is that **the supervise path must ship a compatibility
check, and that check must exist before the packages do.** That is why Part 2's slice includes
it rather than deferring it. If we ever decide the check is too expensive to maintain, that is
the moment to embed instead — the risk and the mitigation are the same conversation.

---

## Part 2 — `boks daemon`

**Implemented on this branch.** What follows describes what exists, with the parts that do not
yet exist marked.

### The commands

```
boks daemon start     write the configuration, run containerd, wait until it answers
boks daemon stop      end it; containerd's root, and the images in it, survive
boks daemon status    is a managed daemon running, and is it actually serving
boks daemon logs      what containerd wrote
boks daemon config    the configuration for this host, with the reason for every setting
boks daemon serve     run it in the foreground; this is what start puts in the background
```

`status` asks two questions rather than one, deliberately: a held lock says a process is alive,
a version returned over the socket says containerd is *serving*. They can disagree, and a
status that collapsed them would call a daemon answering nothing "running". It exits non-zero
when nothing is serving, so a script can gate on it.

`serve` is a normal command, not a hidden one, for the same reason `boks net serve` is: running
the background process by hand is the supported way to watch a daemon that will not start.

### Where state lives

Everything is under the existing `policy.StateDir()`, in a `containerd/` subdirectory:

| Platform | Path |
|---|---|
| Linux | `$XDG_STATE_HOME/boks/containerd`, else `~/.local/state/boks/containerd` |
| macOS | `~/Library/Application Support/boks/containerd` |
| Windows | `%LocalAppData%\boks\containerd` |

holding `config.toml`, `containerd.log`, `daemon.json`, `daemon.lock`, and containerd's own
`root/` and `state/`. `BOKS_STATE_DIR` moves all of it.

The endpoint is `<dir>/containerd.sock` on Unix and the named pipe `\\.\pipe\boks-containerd`
on Windows. It is deliberately *not* containerd's default, so a machine running containerd for
Docker or Kubernetes is untouched and the two daemons never see each other.

`root/` holds unpacked image layers and reaches gigabytes. It is under the state directory
rather than a cache directory on purpose: a cache is something a machine may clear at any
moment, and clearing this one under a running sandbox would remove the filesystem that sandbox
is executing from.

### Why the configuration is generated, not shipped

`packaging/containerd-windows/config.toml` is the hand-written ancestor and stays as the
reference for driving containerd by hand. It cannot be what Boks ships, because three of the
settings are not knowable ahead of time:

- **uid and gid are the running user's.** containerd's `sys.GetLocalListener` ends in an
  unconditional `os.Chown(path, uid, gid)`, and the default is 0. A rootless daemon therefore
  dies before it serves anything — on the *ttrpc* listener, because that one is created first,
  which is why the error names a socket nobody configured (blocker 1).
- **The paths are under the user's state directory**, which varies by platform and by
  `BOKS_STATE_DIR`.
- **Whether the erofs differ may be named at all depends on the host.** This is the one that
  justifies generation on its own, and it is measured.

#### The erofs trap, measured

On 2026-08-15, containerd v2.2.6 on linux/arm64, with `default = ['erofs', 'walking']` and a
`PATH` with no `mkfs.erofs` on it:

```
skip loading plugin  error="failed to check mkfs.erofs availability: ... executable file not
  found in $PATH: skip plugin"  id=io.containerd.differ.v1.erofs
failed to load plugin  error="needed differ not loaded: erofs"
  id=io.containerd.service.v1.diff-service
```

Seven plugins fail: the diff service, `grpc.v1.diff`, `cri.v1.images`, `grpc.v1.cri`,
`grpc.v1.sandbox-controllers`, the restart monitor and the podsandbox controller.

Note what does **not** happen. `io.containerd.metadata.v1.bolt` survives — unlike the Windows
cimfs cascade, which takes about forty plugins — and **the daemon stays running**. It answers
its socket, reports its version, and lists the erofs snapshotter as available.

That is worse than a crash. `boks doctor` would report `containerd ok` and `snapshotter ok`,
both truthfully, and the first `boks run` would fail inside an image unpack, because
`client.Pull` applies layers through the diff service that is no longer there.

So Boks omits erofs from the order when `mkfs.erofs` is absent, and says so in the generated
file. Verified both ways on the same host: with erofs named and no `mkfs.erofs`, seven plugins
fail; with the generated configuration, one unrelated CRI/CNI plugin fails and the erofs
snapshotter and differ are both `ok`.

#### One setting that is deliberately absent

`[plugins.'io.containerd.transfer.v1.local'].unpack_config` is **not** written, and the reason
matters for anyone porting the Windows file across. That is the `ctr` path. Boks pulls with
`client.Pull`, which builds its own `unpack.Platform` from the snapshotter the caller asked for
— so the daemon's unpack configuration never enters into it. Setting it in TOML *replaces*
containerd's built-in list rather than extending it, and the `optional` key that would make an
erofs entry survive a host without `mkfs.erofs` exists only in this project's patched
containerd (patch 0003), not in the stock daemon this file has to work with.

### Implicit start: not yet, and why

`boks run` does **not** start the daemon implicitly today. It reports a containerd it cannot
reach, and the remedy now names `boks daemon start`.

The argument for implicit start is strong and it is the maintainer's stated goal — one fewer
thing a user has to know about. The argument against is narrower than it looks: **starting *a*
containerd is not the same as starting *ours*.** Until a bundle exists, `boks daemon start`
runs whatever containerd is on `PATH`, which is exactly the binary that can be version-skewed
against the shim. Doing that silently, inside `boks run`, would mean Boks quietly starting a
daemon it did not ship and then owning the failure — the precise problem this document exists
to solve, moved one level down.

**When the bundle exists, `boks run` should start the daemon implicitly.** The design that
keeps a daemon failure visible rather than confusing is already in place and is the reason the
supervisor exists:

- the supervisor captures containerd's stderr, so a daemon that refuses to start surfaces
  *containerd's own words*, not "it did not come up";
- `Start` waits for containerd to answer its API, not for a socket file to appear, so "started"
  means "serving";
- the log is separated by a marker, so the tail shown to the user is containerd's output and
  not Boks' own preflight advice.

The one addition implicit start needs is a line on stderr saying a daemon was started, in the
same spirit as the network supervisor's.

### What `boks daemon` cannot fix

`/run/containerd` on Linux, `/var/run/containerd` on macOS. containerd derives each shim's
socket path from a compile-time constant (`pkg/shim/util_unix.go`, `socketRoot =
defaults.DefaultStateDir`), so no configuration setting moves it —
[containerd#12444](https://github.com/containerd/containerd/issues/12444).

Boks tries the harmless half — if the directory can simply be created, it creates it — and
otherwise prints the one `sudo` line up front rather than letting the first sandbox fail on
`mkdir`. It does not refuse to start: a daemon that can pull images is useful to somebody
debugging, and refusing would remove the machine they are debugging with.

### What it also fixes, almost for free

containerd resolves the runtime shim through **its own** `PATH`, and the shim then locates
libkrun and the guest kernel by scanning that same `PATH`. `docs/install.md` lists "start
containerd with the shim on its PATH" as one of the things Homebrew cannot do for the user;
`docs/verification.md` lists it as one of four things that cost time on the first macOS run.

A daemon Boks starts is a daemon whose environment Boks sets. `boks daemon` prepends the bundle
directories to containerd's `PATH` — prepends, not replaces, because containerd also needs
`mkfs.erofs` and a host that has erofs-utils installed normally must keep working.

---

## Part 3 — what ships in the bundle

Sizes are measured where a number is recorded and marked where they are not. The two guest
figures differ by provenance and it matters: `scripts/build-nerdbox-guest.sh` produces an arm64
`Image`, while `guest-image.yml` produces an ELF `vmlinux` (its comment says "Do not 'fix' it
into a bzImage"). They are not the same artifact and not the same size.

| Piece | Linux | macOS/arm64 | Windows | Size |
|---|---|---|---|---|
| `boks` | ✔ | ✔ | ✔ | 23.8 MB measured (release flags, linux/arm64) |
| containerd | ✔ | ✔ | ✔ patched | 43.3 MB measured (Ubuntu's, stripped); ~30 MB without CRI/CNI |
| `ctr` | debug only | debug only | ✔ | 22.4 MB measured |
| `containerd-shim-nerdbox-v1` | ✔ | ✔ + codesigned | ✔ patched | ~18 MB (reported; `nerdbox-windows.yml` prints it, no number is committed) |
| libkrun / `krun.dll` | `libkrun.so` | `libkrun.dylib` | `krun.dll` | ~4 MB (reported; `libkrun-windows.yml` prints it, no number is committed) |
| guest kernel | ✔ | ✔ | ✔ | **15,835,648 bytes** measured, arm64 `Image`, 2026-08-13; the CI ELF `vmlinux` is materially larger (~34 MB reported) |
| `nerdbox-rootfs.erofs` | ✔ | ✔ | ✔ | **8,343,552 bytes** measured, 2026-08-13 |
| `mkfs.erofs` | distro if ≥1.8 | brew | ✔ patched build | 0.3 MB measured (Ubuntu's) |
| `mkfs.ext4` | distro | — | **no Windows build exists** | — |

**Bundle total, order of magnitude: 110–130 MB per platform**, dominated by containerd and the
guest kernel. Compressed, materially less — the rootfs and kernel are the incompressible half.

Two entries deserve their own note:

- **`mkfs.ext4` has no Windows build at all.** containerd wants it for a container's writable
  layer, and `docs/windows-e2e.md` supplies a pre-made `rwlayer-64m.img` by hand. That is fine
  for a documented procedure and is not a thing a package can ship — a fixed-size image is not
  a filesystem tool. This is an open item, not a solved one.
- **The guest kernel is GPL-2.0 and nerdbox patches it.** Distributing the compiled result
  carries a corresponding-source obligation. The recipe is entirely public — a pinned
  `cdn.kernel.org` tarball, a config and a patch set — so satisfying it is a matter of
  publishing that alongside. `release.yml` now does distribute it, with a `SOURCE.txt` pointer,
  in the guest archives and in the Windows bundle; **that is CI implementing a reading of the
  obligation, not the owner ruling on it**, and the last section of this document is where the
  reading is stated and where reversing it would start.

### The CI gap, and how it was closed

`.github/workflows/` builds every Windows piece, the guest images, and — since
2026-08-15 — the Linux runtime too: `linux-runtime.yml` publishes `libkrun.so` and a Linux
`containerd-shim-nerdbox-v1` for amd64 and arm64, with a bundle job and `SHA256SUMS`. It was
added because the first attempt to verify Boks on Linux found that neither piece exists as a
binary anywhere: nerdbox ships zero release assets and neither project is packaged in apt, the
AUR or nixpkgs.

The gap that remained was **retention**. Every one of those pieces was an artifact with an
expiry rather than a release asset, and `krun.dll` was not even that — `libkrun-windows.yml`
built it, size-checked it, asserted all twenty exported symbols, and threw it away. A bundle
cannot be assembled from artifacts that expire, so making these release assets was a
prerequisite to every packaging item below.

**`release.yml` now attaches all of it.** The maintainer's decision is that the first release
bundles the runtime for Linux and Windows, and that macOS stays a Homebrew source build,
because the shim there needs the `com.apple.security.hypervisor` entitlement and building
locally is what lets brew ad-hoc sign it.

| Piece | Built by | Published as |
|---|---|---|
| guest kernel + EROFS rootfs, per guest arch | `guest-image.yml` | `boks-guest_<v>_x86_64.tar.gz`, `boks-guest_<v>_arm64.tar.gz` |
| Linux `libkrun.so` + `containerd-shim-nerdbox-v1`, per arch | `linux-runtime.yml` | `boks-runtime_<v>_linux_amd64.tar.gz`, `…_linux_arm64.tar.gz` |
| `krun.dll` | `libkrun-windows.yml` | inside `boks-runtime_<v>_windows_amd64.zip`, and inside `boks_<v>_windows_amd64.zip` |
| Windows nerdbox shim | `nerdbox-windows.yml` | the same two zips, **renamed** to `containerd-shim-nerdbox-v1.exe` |
| Windows `containerd.exe`, `ctr.exe`, `mkfs.erofs.exe`, `config.toml`, `new-containerd-root.ps1`, `rwlayer-64m.img` | `containerd-windows.yml` | the same two zips |
| `boks` for linux/amd64, linux/arm64, darwin/arm64 and **windows/amd64**, plus `.deb` and `.rpm` | `release.yml` | as before; the Windows CLI is published only inside `boks_<v>_windows_amd64.zip` |

The Windows shim rename is not cosmetic. `nerdbox-windows.yml` builds both architectures and
therefore suffixes them; containerd resolves a runtime *id* to a binary *name*, so
`io.containerd.nerdbox.v1` looks for `containerd-shim-nerdbox-v1.exe` exactly, and a shim
shipped under its build name is a shim containerd reports as missing.

There is deliberately **no macOS runtime archive**, and no `windows/arm64` anything: there is
no `krun.dll` or `mkfs.erofs.exe` for aarch64 Windows, and go-erofs's recipe cross-compiles for
x86-64 alone.

### What goes in the Windows archive, and the guest

Two tracks landed on `main` disagreeing about this file, which is the reason the section
exists. The release workflow shipped the Windows CLI as `boks_<v>_windows_amd64.tar.gz`
(reasoning: Windows 10+ ships `tar`) with the runtime separately as
`boks-runtime_<v>_windows_amd64.zip`; the winget manifests named
`boks_<v>_windows_amd64.zip` with `boks_<v>_windows_amd64/boks.exe` inside it. That asset did
not exist, so a tag would have published a manifest whose `InstallerUrl` 404s.

**The resolution: `boks_<v>_windows_amd64.zip` is the CLI, the runtime and the guest in one
flat directory**, and it is both the winget target and the recommended Windows download.

Three things force the shape rather than merely favouring it:

- **A zip, not a tarball.** winget's `InstallerType` supports `zip` and has no `tar.gz`. The
  "Windows ships `tar`" argument is true and buys nothing here, because the winget asset has
  to be a zip regardless, and a second archive of the same binary in another format is a
  choice offered to a user who has no basis for making it.
- **Flat, with `boks.exe` at the root of one top-level directory**, because that is the path
  `NestedInstallerFiles.RelativeFilePath` already declares. `assemble` asserts the path
  exists rather than letting winget-pkgs' bot discover it is wrong.
- **The runtime is in it**, because the principle this whole document argues from is that a
  user should not assemble a stack from parts. A `winget install` delivering a CLI with no
  runtime would contradict it in the one place a new user meets the project first.

**The Windows CLI-only tarball is gone.** It is a strict subset of the zip — the same
`boks.exe`, the same `LICENSE` and `README.md` — so the only thing distinguishing it is that
it cannot start a sandbox. It would also create a contract nothing enforces: two assets whose
`boks.exe` must stay byte-identical. `scripts/build-release.sh windows amd64` still builds it,
because that is how the binary reaches `assemble` and because `make dist` and
`RELEASE_TARGETS` must keep naming the same platform set the workflow's matrix does — the
assertion in `internal/release/`. It is built and consumed, not published.

**`boks-runtime_<v>_windows_amd64.zip` stays.** Unlike the CLI tarball it is a complement
rather than a subset-in-practice: it is what someone wants who built `boks.exe` from source on
Windows (the route `docs/install.md` still recommends today), or who is driving `containerd`
and `ctr` by hand the way `docs/windows-e2e.md` does. Handing that person the all-in-one
archive would put a second `boks.exe` beside the one they built. It also keeps Windows
symmetric with Linux, where the `.deb` carries the runtime *and* `boks-runtime_*_linux_*.tar.gz`
is published for the tarball route.

#### The guest kernel and rootfs go in it

This was the open decision, and it is decided: **included**, x86_64 only.

**It removes the last download.** Without the guest, `winget install boks` ends at `boks
doctor` reporting `guest image fail` and telling the user to fetch an artifact from a GitHub
Actions run — which needs a GitHub login, expires, and is the worst possible last step for a
first install. That is the "assemble a stack from parts" failure in its purest form.

**It works, which was checked against the code rather than assumed.**
`internal/daemon/locate.go`'s `RuntimeDirs()` searches the directory the `boks` executable
resolves to, following symlinks — and a winget portable install *is* a symlink into the
package directory. `daemonPath()` then **prepends** those directories to the `PATH` it starts
containerd with, and the shim locates `krun.dll`, the kernel and the rootfs by scanning that
same `PATH` (`internal/doctor/checks.go` transcribes the scan). So a flat directory needs no
`PATH` entry, no environment variable and no configuration.

> **The proviso, and it is real.** All of that holds only when containerd is the one `boks
> daemon start` runs. Start the bundled `containerd.exe` by hand and none of the search
> applies, because the daemon's `PATH` is then whatever the shell handed it.

**The licence question is not made worse, and it is not resolved either.** The reading is
already recorded above and at the end of this document: `release.yml` publishes the guest on
the basis that GPL-2.0's corresponding-source obligation is met by shipping a pointer — a
`SOURCE.txt` naming the exact nerdbox revision and the two `docker buildx bake` commands that
reproduce it. Putting the same bytes in a second archive on the same release does not change
who is distributing or on what basis: winget hosts a **manifest**, not the bytes, and its
`InstallerUrl` points at our own GitHub release, so we remain the sole distributor and the
recipient is one click from the repository `SOURCE.txt` names. What it does change is the
blast radius if the reading is wrong — the kernel would then be in two archives rather than
one — and the fix remains what it was: one job in `release.yml`.

So the mechanism rides along by construction. `assemble` writes `SOURCE.txt` into **every**
archive that contains the kernel, guest archives and Windows bundle alike, from one function
rather than two copies of a heredoc. An archive carrying a GPL-2.0 kernel and no pointer would
be a genuinely new obligation rather than the same one, which is the only reason the file
exists.

**The size is not the deciding factor and should not be presented as one.** The guest adds the
kernel and the rootfs — 8,343,552 bytes measured for the arm64 rootfs on 2026-08-13, and a
kernel reported at ~34 MB for the CI x86_64 ELF `vmlinux`, which is the figure this archive
carries and is *reported rather than committed*. Against a bundle that already holds a 43 MB
containerd, a 22 MB `ctr` and an ~18 MB shim, the guest is not what makes this archive large.

**What excluding it would have bought**, stated so the decision can be reversed on its merits:
a materially smaller download, and one fewer place the corresponding-source reading is
exercised. Both are real. Neither outweighs an install that ends in a sandbox that will not
boot — which is exactly the failure mode this document names as the reason a bundle exists at
all.

There is no `aarch64` guest in it because there is no aarch64 Windows runtime to boot it with.

### How the runtime crosses a workflow boundary

`actions/download-artifact` cannot reach another workflow's run without a run id and a token,
so getting five workflows' outputs onto one release needed a decision. Two candidates:

**(a) Make the runtime workflows reusable and call them from `release.yml`.** A called workflow
runs at the caller's ref and uploads into the caller's run, so the assets are built *at the
tag* and a plain `download-artifact` can see them.

**(b) `gh run download` from the latest green run on `main`.** Cheap — a tag build stays
minutes rather than the better part of an hour — and it needs no change to the runtime
workflows.

**(a) was chosen, and it is what is implemented.** The argument is one property: under (b) the
published bytes correspond to *some* commit on `main`, chosen by whichever run happened to be
green, and nothing on the release records which. That is exactly the class of failure this
project keeps finding — pieces that are each individually fine and wrong as a set, which is
[5a](#5a-the-version-that-matters-is-the-set). Under (a) every asset on a tag was built from
that tag's tree, which the build-provenance attestation then records and anyone can check with
`gh attestation verify`.

The cost is real and is accepted: a tag build now compiles libkrun for
`x86_64-pc-windows-msvc` on a Windows runner, which is slow. A tag is rare; a release whose
runtime came from an unknown commit is a permanent problem.

**The `paths:` filters were not an obstacle**, contrary to the initial worry. A path filter
constrains only the event it is written under; `workflow_call` is a separate event with no
filter, so `libkrun-windows.yml` is fully callable even though its push trigger fires only when
its own patch directory changes. GitHub's syntax reference scopes `paths`/`paths-ignore` to the
`push` and `pull_request` events and defines them nowhere else — an affirmative scoping rather
than an explicit statement that `workflow_call` ignores them, which is as close as the
documentation comes.

**What the design rests on, checked against GitHub's documentation rather than assumed.** These
were written down as unverified when the workflows were built, and then looked up:

| Assumption | State |
|---|---|
| A called workflow's artifacts are visible to a later job in the caller by plain name | **Established, not stated in one sentence.** "The entire called workflow is used, just as if it was part of the caller workflow", and `download-artifact` defaults to "the current repo and the current workflow run". The compound claim is a conclusion from the two, and it is the load-bearing one. |
| `github.workflow` inside a called workflow is the *caller's* name | **Verified, explicitly.** The reusable-workflow reference states it, and warns about exactly the concurrency-group collision the change was made to avoid. |
| `strategy.matrix` may be used on a `uses:` job | **Verified.** `strategy` is on the documented list of keywords permitted on a job that calls a reusable workflow, with no restriction on its sub-keys; matrix calls arrived in the August 2022 reusable-workflow improvements. |
| A ~100 MB asset among fourteen is within limits | **Verified.** Each file in a release must be under 2 GiB, with no documented limit on a release's total size and a ceiling of 1000 assets. |

The first row is the one to watch. It is a sound conclusion from two documented facts rather
than a documented fact, so if a tag build fails at `assemble` with an artifact it cannot find,
that row is the first place to look and not the artifact names.

Two things did have to change to make (a) safe, and both are in the runtime workflows:

- **Concurrency groups now include `github.workflow`**, which under `workflow_call` is the
  *caller's* name. These workflows all set `cancel-in-progress: true`; without the change, a
  dispatched release on `main` and a push-triggered run of the same workflow on `main` would
  share a group and cancel each other, and the one that died could be the release. If that
  assumption about `github.workflow` is ever wrong, the group collapses to what it was before —
  today's behaviour rather than a new failure.
- **Every producing workflow now writes a `SHA256SUMS` into its own artifact.** `release.yml`
  checks all of them on the far side of the artifact transport, before a byte is copied into a
  release archive, then writes its own `.sha256` per asset which the publishing job checks
  again. That extends the chain the CLI tarballs already had by one hop rather than trusting
  the new hop. Each published archive also carries a `SHA256SUMS` over its own contents,
  regenerated after assembly — the incoming ones are wrong by construction once the Windows
  directory has gained a `krun.dll` and a renamed shim.

### What a release is signed with, and what it is not

**Binaries are not signed.** No Authenticode certificate, no Developer ID. That is the
maintainer's decision and its cost is stated in the winget section below: SmartScreen's
"Windows protected your PC", every time, with no reputation accruing.

**`SHA256SUMS` is GPG-signed**, with the key that already exists in `packaging/apt/`
(fingerprint `D5DD07C0F9589C164F7361C20EB93D3C39471E1E`, private half the Actions secret
`APT_GPG_PRIVATE_KEY`). Reusing it rather than minting a second key means one key to publish,
one to rotate and one set of instructions, and a detached signature over the checksum file
covers every asset transitively. A tag that cannot reach the key **fails** rather than
publishing an unsigned checksum file; a dispatched dry run, which produces a draft nobody can
install, warns and continues.

**Provenance covers every asset, the runtime included.** The publishing job holds
`contents: write` and runs no compiler and no third-party code: it downloads, re-verifies,
signs the checksum file, attests and creates the release. The runtime archives need that split
more than the CLI does, not less — they are the binaries that boot a VM on someone's machine.

**None of this has been executed.** No tagged run has happened. Everything above is a set of
workflows that parse, pass `actionlint` with no new findings, and whose shell was run locally
against stand-in files — which is evidence about the scripts and none at all about how GitHub
behaves.

The `assemble` job's three shell steps have been run that way, extracted verbatim from
`release.yml` rather than retyped, against stand-in artifacts named exactly as each producing
workflow names them. They produced every archive, and the Windows zip contained
`boks_<v>_windows_amd64/boks.exe` at the path the winget manifest declares. Four **negative
controls** were run against the same steps, because a check that never fails proves nothing:
a CLI archive with no `boks.exe`, an incoming artifact carrying neither a `SHA256SUMS` nor a
`.sha256`, a corrupted CLI tarball, and a corrupted runtime file. All four failed the step that
should catch them. That is evidence about the shell. It says nothing about whether
`actions/download-artifact` can see a called workflow's artifacts, which is still the row to
watch in the table above.

---

## Part 4 — package by package

### Homebrew (macOS/arm64)

**Formula, not cask.** A cask is for a pre-built application bundle; this is a CLI and its
dependencies, built from source, and `nerdbox.rb` already does the one thing casks make awkward
— running `codesign` after Homebrew's own relocation.

That ordering is the subtle part and it is already solved and documented. libkrun cannot use
Hypervisor.framework without `com.apple.security.hypervisor`; without it a sandbox does not
fail to start, it starts and dies inside libkrun with `krun_start_enter failed: -22`, an error
naming neither code signing nor the entitlement. Homebrew re-signs Mach-O files whose load
commands it patched, and on Apple silicon it uses ruby-macho's `MachO.codesign!`, which writes
a plain ad-hoc signature and carries no entitlements across. So the signature is applied in
`post_install`, which runs after `fix_dynamic_linkage`, and survives both a source build and a
bottle.

**The formulae are templates now**, in `packaging/homebrew/tap/`, rendered by
`packaging/homebrew/render.sh` in the same shape as `packaging/winget/render.sh`: it stamps the
version and two SHA-256s and refuses to emit a file with a placeholder left in it. The old
hand-edited `sha256 "0000…0000"` is gone.

**The guest gap is closed.** `release.yml` publishes `boks-guest_<v>_arm64.tar.gz`; `boks.rb`
now fetches it as a `resource` and installs `nerdbox-kernel-arm64` and `nerdbox-rootfs.erofs`
into `#{HOMEBREW_PREFIX}/lib`, which is the last directory the shim scans on Apple silicon. So
a successful `brew install boks` should no longer leave `guest image  fail` — untested, like
everything else here.

**What we do not have:**

- the tap repository `dagsommer/homebrew-boks` does not exist, and cannot be created from this
  repository;
- **a published release.** The `v0.1.0` release is a *draft*, and a draft carries no tag, so
  `archive/refs/tags/v0.1.0.tar.gz` 404s and the two digests the formula needs cannot be taken
  yet. This is now the first step of the publish procedure rather than a footnote;
- **no macOS machine on CI**, so neither formula has ever been *installed*. They have been
  linted, audited, dependency-resolved, trust-tested and bottle-fetched against a real
  Homebrew 6.0.17 on Linux — see `packaging/homebrew/README.md` for the command-by-command
  record — which is a document check and not an install;
- no bottles of our own. On Tahoe and Sequoia that costs little: the macOS/arm64 closure is
  **14 kegs and only `boks` and `nerdbox` compile**, both Go. On Sonoma it costs a lot, because
  `libkrun`, `libkrunfw` and `virglrenderer` bottle `arm64_tahoe` and `arm64_sequoia` only —
  31 kegs, with `rust`, `lld` and `llvm` pulled in to build them. That is the libkrun tap's
  coverage, not ours, and the only lever here is to warn about it.

**Cost of the install, as designed:** four commands, not one — `brew tap`, `brew trust
dagsommer/boks`, `brew trust libkrun/krun`, `brew install boks`. Homebrew 6.0.0 requires
explicit trust for non-official taps, and the check runs at formula load, so it fails in under
a second rather than after a build. Note the third line: `brew trust --formula
libkrun/krun/libkrun` was documented here and **is not enough** — libkrun depends on
`libkrunfw` and `virglrenderer` from the same tap and trust is not transitive. sbx has the same
requirement.

### winget (Windows)

**The `virtio-net` gate is passed.** This section used to say winget was blocked on it: no
Ethernet frame had crossed a `virtio-net` on Windows, and Boks refuses to start a sandbox whose
network policy it cannot enforce, so a package would have installed a binary whose only
possible output was a refusal. On 2026-08-15 `boks run` completed unelevated on Windows 11 with
the policy engine judging real traffic — the `boks run` bar, not the weaker `ctr` one. The
maintainer's note that native Windows needs no elevation is also right, and the junction patch
is what removed the requirement.

**The manifests exist**, in [`packaging/winget/`](../packaging/winget/): three templates, a
`render.sh` that stamps the version, digest and release date, and a `validate.py` that checks
the rendered files against winget's own published JSON schemas. That directory's README is the
authority on what has and has not been checked, and the summary is that schema validation has
been run on Linux with a negative control, and `winget` itself has not been run at all by
anyone on this project.

The artifact they name now exists too: `boks_<v>_windows_amd64.zip`, laid out to match
`RelativeFilePath` exactly — see [What goes in the Windows archive, and the
guest](#what-goes-in-the-windows-archive-and-the-guest).

**What we do not have:** a submission to `microsoft/winget-pkgs`, which is a pull request to
somebody else's repository needing a signed CLA and a human moderator and cannot be done from
here; a tagged release for its `InstallerUrl` to point at; and a `mkfs.ext4` story. None of it
has been through a tagged run, and no `winget install` has been attempted.

**Code signing and SmartScreen.** The maintainer has decided not to spend on this yet, and the
plan should state the consequence rather than argue with it. An unsigned installer triggers
SmartScreen's "Windows protected your PC" dialog, which requires clicking through "More info →
Run anyway". Reputation accrues per-certificate and per-binary over download volume, so an
unsigned package does not improve with time the way a signed one does. winget's own manifest
validation does not require signing. An OV certificate is roughly $200–400/year and still
accrues reputation slowly; an EV certificate is roughly $300–600/year and bypasses SmartScreen
immediately but requires a hardware token or a cloud HSM, which complicates CI signing. The
honest summary: **not signing is survivable for an experimental tool and visibly costs the
first-run experience**, and the decision can be deferred until there is a Windows release worth
protecting.

### Linux — `.deb`, `.rpm`, and the apt repository

The packages exist and are built by `release.yml`. They contain the CLI, its licence, three
shell completions **and the runtime** — `containerd`, `containerd-shim-nerdbox-v1` and
`libkrun.so` under `/usr/libexec/boks/`, which `scripts/package-linux.sh` refuses to build
without. They declare `Recommends: erofs-utils` and no `Depends:`, for a reason stated in their
own description: no distribution ships a containerd new enough, and nerdbox is packaged nowhere.
`Depends: containerd` would install 1.7.x on Ubuntu 24.04 and produce a machine that looks
provisioned and cannot start a sandbox.

**What we do not have:**

- the guest kernel and rootfs *inside* the package. They are release assets
  (`boks-guest_<v>_<arch>.tar.gz`) and `package-linux.sh` will place them if handed them, but
  `release.yml` does not hand them over today — so a `.deb` install still reaches
  `guest image fail`. The Windows zip does carry the guest; Linux is the asymmetry;
- the apt repository. The signing key exists — `packaging/apt/boks-archive-keyring.asc`,
  fingerprint `D5DD07C0F9589C164F7361C20EB93D3C39471E1E`, expiring 2029-08-13, private half a
  CI secret and the only copy — and nothing else does. No index generation, no `InRelease`, no
  hosting. GitHub Pages can serve it; `pages.yml` currently publishes only the docs site;
- any RPM repository metadata, despite the key's comment naming both.

**Where the runtime pieces should go.** `boks daemon` already searches, nearest first:
`$BOKS_RUNTIME_DIR`, then `<exe dir>/../libexec/boks`, then the directory beside the `boks`
executable. For a `.deb` or `.rpm` that means `/usr/libexec/boks/`, which is the FHS location
for a program's private executables and keeps a bundled containerd off the user's `PATH` —
where it would otherwise shadow a containerd they installed on purpose. For a Homebrew keg it
is `libexec/`, reached through the symlink resolution the search already does. For a tarball,
everything sits side by side, so the two derived locations collapse into one.

**The bundle directory comes before the executable's own, and that order is the bug that
shipped.** It was written the other way round, and on an installed `.deb` — where `boks` is in
`/usr/bin`, which also holds the distribution's own `containerd` — the search found the system
binary first. Measured on 2026-08-15: `boks daemon start` reported "containerd v2.2.6" out of
`/usr/bin`, below the 2.3 floor and the version that fails at task start, while the 2.3.3 the
package had just installed sat unused in `/usr/libexec/boks`. Preferring the system copy over
the vendored one is the exact failure vendoring exists to prevent.

---

## Part 5 — versions and upgrades

There are two questions here and they are not equally important. Whether a newer Boks exists is
the one users ask about. Whether the pieces on the machine are compatible with each other is the
one that costs them days.

### 5a. The version that matters is the *set*

Boks orchestrates four independently-versioned things — containerd, the nerdbox shim, libkrun,
and the guest kernel/rootfs — and skew between them produces failures that name nothing.

**Measured, WSL2, 2026-08-15.** The CI-built shim links containerd v2.3.3 and emits version-3
bootstrap parameters. Ubuntu's containerd 2.2.2 cannot decode them, falls back to treating the
whole protobuf reply as an address, and fails with:

```
unsupported protocol: Yunix
```

"Yunix" is three control bytes from the protobuf framing rendered as letters. Both binaries were
individually fine. `boks doctor` reported `containerd ok` and `vm runtime ok`, truthfully. The
set was wrong, and nothing was asking about the set.

**Measured, 2026-08-15.** Linux and Windows need *different libkrun revisions*. nerdbox binds
all nineteen entry points eagerly at `dlopen`, so a libkrun missing any of them fails to load
entirely rather than failing at the call. At the revision the Windows port pins (2.0.0-dev)
four of the nineteen are absent; Windows works only because this project's patches re-export
them.

**Earlier.** `krun_set_exec` returning `-ENOTSUP` and `krun_add_vsock_port` returning `-ENODEV`
were both upstream API removals between the revision nerdbox targets and the one we pinned.

#### What is checkable, and what each would have caught

| Check | Cost | Would have caught |
|---|---|---|
| **containerd's version over its API** | trivial; `doctor` already does it | half of the `Yunix` comparison |
| **The shim's linked containerd**, via `debug/buildinfo` (`go version -m`) | **very cheap** — stdlib, no subprocess, no network, reads a file | `Yunix`, exactly. This is how it was pinned down |
| **libkrun's exported symbols**, via `debug/elf` / `debug/macho` / `debug/pe` | cheap — stdlib, but three formats and a list of 19 names to keep in step with nerdbox | the libkrun ABI skew, and `krun_add_vsock_port` (removed). **Not** `krun_set_exec` returning `-ENOTSUP`, which is present-but-refusing |
| **Guest kernel/rootfs against the shim** | **expensive, and currently impossible** — neither file carries a version, so there is nothing to compare without publishing a manifest beside them | nothing today; it would need a manifest to exist first |
| **libkrun's API behaviour** (present but refusing) | expensive — needs a hypervisor and a boot | `krun_set_exec` |

**The first two are implemented on this branch** as `boks doctor`'s `runtime skew` line and as a
warning from `boks daemon start`. The rule is directional and narrow: a daemon *older* than the
shim's containerd is a problem, a newer one is not, stated at major.minor. An unparseable
version produces nothing at all — a check that guessed would warn on healthy hosts, which is how
a warning becomes something people learn to ignore.

**The libkrun symbol check is the highest-value unimplemented item in this document.** It is a
day's work, it needs no network and no hypervisor, and it converts "the shim fails at `dlopen`"
into a list of missing symbols and the libkrun revision that has them.

Publishing a **manifest** alongside the bundle — one file naming the exact containerd, shim,
libkrun and guest revisions that were tested together — would make the fourth row checkable and
would let `doctor` say "this is not the set that was tested" rather than checking pairs. That is
the right long-term shape and it costs nothing but a CI step, once the artifacts are release
assets.

### 5b. Whether a newer Boks exists

sbx does this in its own diagnostic: observed on a real Docker Sandboxes install, `sbx diagnose`
printed `8 passed, 1 warning (update available, v0.35.0 → v0.38.0)`. `boks doctor` is the
equivalent surface and the natural home.

Per the maintainer's ruling, this is an ordinary feature rather than a privacy question, and it
may run on the hot path of `boks run`. The constraints below are about correctness, not
principle, and they are strict because of who uses Boks.

**1. It must never delay or fail a run.** Boks starts sandboxes for people whose work is
waiting. Concretely: check asynchronously, never blocking the run; a hard timeout of **2
seconds**, well under anything noticeable; cache the answer for **24 hours** in the state
directory; and if the result has not arrived by the time the run is ready to proceed, proceed.
The check never gates anything.

**2. Failure must be silent.** Boks' users are frequently on restricted networks — that is the
product. Corporate proxies, deny-by-default egress, air-gapped machines. A check that hangs,
retries or prints an error on a machine that simply cannot reach the internet would be worse
than no check at all. **No retries. No error output. A failed check is indistinguishable from a
check that found nothing new**, and it must never be attributable to Boks by a confused user.

**3. It is host-side and outside the sandbox's policy.** The check runs in the `boks` process on
the host, before and independently of any sandbox. It is *not* subject to the sandbox's egress
rules, does not appear in the sandbox's decision log, and cannot be blocked by a `--net`
setting. A reader could reasonably assume otherwise, so the documentation must say it plainly.
The corollary is also worth saying: a sandbox's policy is not weakened by this, because nothing
inside the sandbox makes the request.

**4. What it sends, documented as documentation.** A single unauthenticated HTTPS `GET` to the
GitHub releases API for this repository, carrying the current Boks version in the `User-Agent`
and nothing else. No identifier, no account, no machine fingerprint, no telemetry payload. What
the other end can observe is what any HTTPS request reveals: the source IP address, the
approximate time, and — from the `User-Agent` — that a particular Boks version was running.
Once per 24 hours per machine at most.

**5. There is still an off switch**, as an operational need rather than a privacy one: an
air-gapped or policy-restricted machine should be able to stop Boks making the request at all,
rather than relying on it failing quietly. `BOKS_UPDATE_CHECK=0`, and a persisted setting for
people who do not want to export a variable.

**Where it surfaces:** as a `warn` line in `boks doctor` — matching sbx — and as a single line
at the end of `boks run`, never at the start, so it cannot be mistaken for something the run is
waiting on.

**What the update instructions say.** Boks does not update itself. There is deliberately no
`curl | sh` installer and no self-replacing binary: Boks refuses to install agent CLIs that way
and pins every download it makes with a checksum, and installing Boks itself is a poor place to
make an exception. The check reports; the package manager updates:

```
brew upgrade boks                # macOS
winget upgrade boks              # Windows
sudo apt upgrade boks            # Debian/Ubuntu, once the repository exists
sudo dnf upgrade boks            # Fedora, likewise
```

Until the apt/rpm repositories exist, the Linux instruction is "download the new `.deb` and
`dpkg -i` it", and the update check should say exactly that rather than naming a command that
does not work.

**A daemon-specific wrinkle worth designing for now.** Upgrading `boks` while a managed
containerd is running leaves an old supervisor supervising a possibly-old containerd. The
state file already records the containerd binary and version; `boks daemon status` should report
when the running daemon is not the one the current `boks` would start, and `boks daemon start`
after an upgrade should say "a daemon is already running, started from a different build" rather
than silently reusing it. This is not implemented and is small.

---

## Part 6 — sequencing

The ordering principle: **nothing is a package until its artifacts stop expiring, and nothing is
installable until the guest ships.**

### Release 0 — honest and useful today (this branch)

`boks daemon`, and the skew check. It needs no new artifacts, no CI work and no decisions, and
it removes the step every tester has stumbled over on both platforms. A user still installs
containerd themselves; they no longer have to configure it.

### Release 1 — make the artifacts durable

Purely CI work, no decisions. **Done, and unverified by execution** — see "How the runtime
crosses a workflow boundary" above:

1. ~~**Attach the guest images to releases**~~ — `release.yml` calls `guest-image.yml` once per
   architecture and publishes `boks-guest_<v>_<arch>.tar.gz`. The GPL question is answered by
   shipping the corresponding-source pointer rather than by not shipping: every guest archive
   carries a `SOURCE.txt` naming the exact nerdbox revision and the two `docker buildx bake`
   commands that reproduce it, and the whole recipe is public. That satisfies the obligation for
   a public recipe; it does not make the decision for anyone who thinks the obligation is
   larger, and it is the one item here worth a second look before the first tag.
2. ~~**Upload `krun.dll`**~~ — `libkrun-windows.yml` now stages it with a checksum and uploads
   it, and `release.yml` puts it in the Windows archive.
3. ~~**Write the two missing Linux workflows**~~ — `linux-runtime.yml` builds both, and
   `release.yml` publishes them per architecture.
4. **Publish a manifest** naming the exact revisions built together, so 5a's fourth row becomes
   checkable. **Still outstanding.** It is much cheaper now than it was: a tag build has every
   pin in one run, so the manifest is a file that job writes rather than a cross-run
   reconciliation.

Also done here, though it belongs to no numbered item: **a `windows/amd64` CLI is built, and
published inside `boks_<v>_windows_amd64.zip` rather than on its own**. It was excluded
entirely on the grounds that libkrun had no Windows backend and a binary whose only output is a
refusal is worse than none. That has not been true since 2026-08-15, when `boks run` completed
unelevated on Windows 11 with the policy engine judging real traffic — and the same reasoning
is why it does not ship alone now either.

### Release 2 — the smallest honestly usable install

**macOS/arm64 Homebrew, with the guest.** It is the only platform where Boks has been shown to
work end to end, and the guest resource is now in `boks.rb`, so `brew install boks` should
produce a machine where `boks doctor` passes and a sandbox boots. That would be the first
install that is *true* — and "should" is doing real work in that sentence.

It still requires the one `sudo` line for `/var/run/containerd` and the `brew trust` lines.
Neither is avoidable and both are documented.

The tap has never been run, and there is no macOS machine on CI. **Treat the first `brew
install` as the test**, and expect it to fail the first time.

### Release 3 — Linux packages carrying the stack

`.deb` and `.rpm` shipping `boks` plus `/usr/libexec/boks/{containerd, containerd-shim-nerdbox-v1,
libkrun.so, nerdbox-kernel-*, nerdbox-rootfs.erofs}`, with `mkfs.erofs` still a `Recommends`
because distributions do package it (and `boks daemon` already degrades correctly when it is too
old or absent).

Both prerequisites are now met on the release side. Release 1's Linux workflows exist and their
output is a release asset, and a sandbox **has** booted on Linux: 2026-08-15, in WSL2 on Ubuntu
26.04, 25 of 26 checks. Two caveats keep this from being a formality — that run was inside
WSL2, with no bare-metal Linux result recorded, and `linux/arm64` has never been run at all, so
half of what a package would ship is still unexercised. Shipping a 130 MB package for an
unverified path would be the same mistake as a winget package that can only refuse.

### Release 4 — winget

The `virtio-net` gate is passed: on 2026-08-15 `boks run` completed unelevated on Windows 11
with the policy engine judging real traffic, which is the `boks run` bar rather than the `ctr`
one. The manifests are written and schema-checked, and `boks_<v>_windows_amd64.zip` — the
asset they name, carrying the CLI, the runtime and the guest — is assembled by `release.yml`.

What is left is not ours to do from here: **cut a tag**, so the `InstallerUrl` resolves and the
digest can be computed from a real file, then **submit a pull request to
`microsoft/winget-pkgs`** behind a signed CLA and a human moderator.
`packaging/winget/README.md` has that sequence in order, including why the automation that
updates a package cannot bootstrap the first one. The SmartScreen decision can stay "no"
without blocking any of it.

### Can wait, indefinitely

- **The apt repository.** Its whole benefit is upgrades, which matter once Linux is verified and
  someone is upgrading. Publishing a `.deb` per release is the right size for now, and an apt
  repository is a standing commitment: indices that must stay consistent and a URL that must
  keep working.
- **Homebrew bottles.** They save two Go builds, which is minutes, and cost a `brew bottle`
  run per macOS version plus hosting. On Tahoe and Sequoia those two builds are the only
  compiles in the whole install, so the saving is small; on Sonoma the time goes into
  libkrun's source build, which a Boks bottle would not touch.
- **Code signing on Windows.** Costs money and buys a first-run dialog.
- **Embedding containerd.** Revisit when the Windows patch series stops moving. The measurement
  is done and the answer will keep: 0.4 MB.

### The one decision blocking the most

**Whether to distribute a compiled GPL-2.0 guest kernel.** It used to block Release 1, and
through it Releases 2 and 3. `release.yml` now publishes the guest, on the reading that the
corresponding-source obligation is satisfied by publishing the recipe alongside: it is entirely
public — `scripts/build-nerdbox-guest.sh` and nerdbox's bake targets produce both files in about
four minutes on an ordinary Linux runner — and every archive that contains the kernel carries a
`SOURCE.txt` naming the exact revision and the commands.

**That reading now applies in two places, not one.** The guest archives publish the kernel, and
so does `boks_<v>_windows_amd64.zip`, because a winget install that ends in a sandbox which will
not boot is the failure this whole document is written against. `SOURCE.txt` is written into
both from the same code path in `assemble`, so the two cannot drift; the reasoning is under
[What goes in the Windows archive, and the
guest](#what-goes-in-the-windows-archive-and-the-guest). Nothing about winget widens the
obligation on its own — winget hosts a manifest and the bytes come from our own release URL, so
we remain the sole distributor.

**That is an implementation of a reading, not a legal opinion, and it was made by CI rather
than by the owner.** If the owner's reading is that the obligation needs more than a pointer —
an offer valid for three years, or the sources hosted by us rather than by upstream — the fix is
one job in `release.yml`, and it is much easier to make before the first tag than after. What
has changed is only how much rides on it: two archives now, so reversing it means removing the
guest from the Windows bundle as well, and accepting that a `winget install` then needs a second
download.
