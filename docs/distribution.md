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
containerd with the shim on its PATH" as one of three things Homebrew cannot do for the user;
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
  publishing that alongside. It remains the owner's call and is the single largest blocker to a
  bundle, because a bundle without the guest is a bundle whose `doctor` passes and whose first
  sandbox will not boot.

### The CI gap nobody has written down

`.github/workflows/` builds every Windows piece and the guest images. It builds **nothing for
Linux**: there is no workflow producing `libkrun.so`, and none producing a Linux
`containerd-shim-nerdbox-v1`. `libkrun-windows.yml` builds `krun.dll`, prints its size, checks
its twenty exported symbols — and uploads no artifact at all.

So the honest state of the pipeline is:

| Artifact | Built in CI | Retention | Attached to a release |
|---|---|---|---|
| guest kernel + rootfs | yes, per arch | 30 days | **no** |
| Windows containerd + `ctr` + `mkfs.erofs.exe` + bundle | yes | 30–90 days | **no** |
| Windows nerdbox shim | yes | 90 days | **no** |
| `krun.dll` | built, then **discarded** | — | **no** |
| Linux `libkrun.so` | **not built** | — | no |
| Linux nerdbox shim | **not built** | — | no |
| `boks` tarballs, `.deb`, `.rpm` | yes | 7 days | yes, on a tag |

Everything except `boks` itself expires. A bundle cannot be assembled from artifacts that
expire, so **making these release assets is a prerequisite to every packaging item below**, and
two of them have to be built for the first time.

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

**What we do not have:**

- the tap repository `dagsommer/homebrew-boks` does not exist, and cannot be created from this
  repository;
- `boks.rb` carries a placeholder `sha256 "0000…0000"` awaiting the first tag's tarball;
- **no macOS machine on CI**, so neither formula has ever been run — they have been checked for
  syntax and read against Homebrew's source, nothing more;
- no bottles, which means every install is a source build including a Go toolchain;
- the guest kernel and rootfs, which the formula cannot build (no Docker, no Linux
  cross-toolchain in a Homebrew build) and which are published nowhere. This is the gap that
  makes a successful `brew install boks` still produce `guest image  fail`.

The guest gap has a known fix that is not technical: attach `nerdbox-guest-arm64-*.tar.gz` to a
release and add a `resource` block, spelled out in `packaging/homebrew/README.md`. It waits on
the GPL decision.

**Cost of the install, as designed:** four commands, not one — `brew tap`, `brew trust
dagsommer/boks`, `brew trust --formula libkrun/krun/libkrun`, `brew install boks`. Homebrew
6.0.0 requires explicit trust for non-official taps, and `libkrun/krun/libkrun` is always
reached as a dependency so is never implicitly trusted. sbx has the same requirement.

### winget (Windows)

**Now viable in principle, and not yet in fact.** The maintainer's note is right that native
Windows works and needs no elevation — the junction patch removed that requirement — and winget
installs on Windows rather than inside WSL, which is the delivery mechanism the platform needs.

The bar is `boks run` completing on Windows, which is stricter than a microVM booting there. On
2026-08-14 `ctr tasks start` ran a Linux container end to end on real Windows 11 hardware. What
remains is `virtio-net`: no Ethernet frame has crossed one on Windows, and Boks refuses to start
a sandbox whose network policy it cannot enforce. A winget package today would install a binary
whose only possible output is a refusal.

**What we do not have:** a manifest; a release carrying a Windows binary (none is built for
release); `krun.dll` as an artifact at all; a `mkfs.ext4` story.

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

The packages exist and are built by `release.yml`. They contain the CLI, its licence and three
shell completions, and nothing else. They declare `Recommends: erofs-utils` and no `Depends:`,
for a reason stated in their own description: no distribution ships a containerd new enough, and
nerdbox is packaged nowhere. `Depends: containerd` would install 1.7.x on Ubuntu 24.04 and
produce a machine that looks provisioned and cannot start a sandbox.

**What we do not have:**

- the runtime pieces to put in the package: no CI builds a Linux `libkrun.so` or a Linux
  nerdbox shim (see the gap table above). This is the largest single omission in the whole plan
  and it is not tracked anywhere else;
- the apt repository. The signing key exists — `packaging/apt/boks-archive-keyring.asc`,
  fingerprint `D5DD07C0F9589C164F7361C20EB93D3C39471E1E`, expiring 2029-08-13, private half a
  CI secret and the only copy — and nothing else does. No index generation, no `InRelease`, no
  hosting. GitHub Pages can serve it; `pages.yml` currently publishes only the docs site;
- any RPM repository metadata, despite the key's comment naming both.

**Where the runtime pieces should go.** `boks daemon` already searches, nearest first:
`$BOKS_RUNTIME_DIR`, the directory beside the `boks` executable, then `<exe dir>/../libexec/boks`.
For a `.deb` or `.rpm` that means `/usr/libexec/boks/`, which is the FHS location for a
program's private executables and keeps a bundled containerd off the user's `PATH` — where it
would otherwise shadow a containerd they installed on purpose. For a Homebrew keg it is
`libexec/`, reached through the symlink resolution the search already does. For a tarball,
everything sits side by side and the first search location finds it.

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

Purely CI work, no decisions:

1. **Attach the guest images to releases** instead of letting them expire at 30 days. Blocked
   on the GPL decision, which is therefore the first thing to resolve.
2. **Upload `krun.dll`** from `libkrun-windows.yml`, which already builds and validates it and
   then throws it away.
3. **Write the two missing Linux workflows** — `libkrun.so` and the Linux nerdbox shim. These do
   not exist at all and nothing else can proceed without them.
4. **Publish a manifest** naming the exact revisions built together, so 5a's fourth row becomes
   checkable.

### Release 2 — the smallest honestly usable install

**macOS/arm64 Homebrew, with the guest.** It is the only platform where Boks has been shown to
work end to end, and with the guest resource added, `brew install boks` produces a machine where
`boks doctor` passes and a sandbox boots. That is the first install that is *true*.

It still requires the one `sudo` line for `/var/run/containerd` and the `brew trust` lines.
Neither is avoidable and both are documented.

The tap has never been run, and there is no macOS machine on CI. **Treat the first `brew
install` as the test**, and expect it to fail the first time.

### Release 3 — Linux packages carrying the stack

`.deb` and `.rpm` shipping `boks` plus `/usr/libexec/boks/{containerd, containerd-shim-nerdbox-v1,
libkrun.so, nerdbox-kernel-*, nerdbox-rootfs.erofs}`, with `mkfs.erofs` still a `Recommends`
because distributions do package it (and `boks daemon` already degrades correctly when it is too
old or absent).

Prerequisite: Release 1's Linux workflows, and a first Linux end-to-end run — **no sandbox has
yet booted on Linux**, and shipping a 130 MB package for an unverified path would be the same
mistake as a winget package that can only refuse.

### Release 4 — winget

Gated on `virtio-net` on Windows, which is a `boks run` bar rather than a `ctr` bar. Then the
manifest, a Windows release build, and the SmartScreen decision — which can stay "no" without
blocking anything.

### Can wait, indefinitely

- **The apt repository.** Its whole benefit is upgrades, which matter once Linux is verified and
  someone is upgrading. Publishing a `.deb` per release is the right size for now, and an apt
  repository is a standing commitment: indices that must stay consistent and a URL that must
  keep working.
- **Homebrew bottles.** They save a Go toolchain and a build, which is minutes, and cost a
  `brew bottle` run per macOS version plus hosting.
- **Code signing on Windows.** Costs money and buys a first-run dialog.
- **Embedding containerd.** Revisit when the Windows patch series stops moving. The measurement
  is done and the answer will keep: 0.4 MB.

### The one decision blocking the most

**Whether to distribute a compiled GPL-2.0 guest kernel.** It blocks Release 1, which blocks
Releases 2 and 3. It is not a technical problem — `scripts/build-nerdbox-guest.sh` produces both
files in about four minutes on an ordinary Linux runner, and the recipe is entirely public, so
satisfying the corresponding-source obligation is a matter of publishing it alongside. It is the
owner's call, and every packaging route waits on it.
