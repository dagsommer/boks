# Installing Boks

Pick your platform below. Each route is the install that platform is meant to have, marked
with its honest, current status — and the status to know first is this one:

> [!IMPORTANT]
> **Nothing has been released yet.** There are no tags and no published binaries, so none of
> the package-manager routes below is live today. They are written here so you can see what
> installing Boks will look like — and so the first release lands somewhere prepared — but
> until it exists, the working route is [building from source](#building-from-source), which
> takes one `make`.

Whichever route you take, run `boks doctor` immediately afterwards. It is the only thing
that knows whether your machine can actually start a sandbox, and it prints a remedy for
every gap.

**Three platforms have now run a sandbox**, and they did not all get there at the same time.
macOS on Apple silicon was first and is the most thoroughly measured; Windows and Linux both
followed on 2026-08-14 and 2026-08-15. Each section below says what was observed on that
platform and what was not, and [verification.md](verification.md) is the record every one of
those claims is taken from.

## macOS on Apple silicon — Homebrew

**The first platform Boks was shown to work on, and still the most measured.** The VM
boundary and the network policy were both established here; see
[verification.md](verification.md).

```sh
brew tap dagsommer/boks
brew trust dagsommer/boks
brew trust libkrun/krun
brew install boks
```

> [!NOTE]
> **Not published yet.** The tap does not exist. The formulae are written and reviewed — they
> are in [`packaging/homebrew/`](../packaging/homebrew/) with the procedure for creating the
> tap — but no macOS machine has ever run them: there is no macOS CI on this project. They
> have been linted, audited, dependency-resolved and trust-tested against a real Homebrew
> 6.0.17 on Linux, which is a document check and not an install. Until the tap is live,
> [build from source](#building-from-source).

That installs the whole stack — containerd, erofs-utils, libkrun and a nerdbox shim signed
with the entitlement libkrun needs — rather than the CLI alone, because a CLI alone would
land on a machine where every check fails.

Homebrew's own `containerd` formula makes the same point from the other side. Its macOS
caveat reads: *"The macOS version of containerd does not natively support running containers.
You need to install an additional runtime plugin such as nerdbox (not packaged in Homebrew
yet) to run containers on this build of containerd."* That plugin is exactly what
`dagsommer/boks/nerdbox` supplies, and it is why the tap has two formulae rather than one.

**Why the two `brew trust` lines.** Since Homebrew 6.0.0 a non-official tap must be trusted
before Homebrew will load Ruby from it. Any tap-based install has the same requirement; it is
a Homebrew rule, not something specific to this project. The check runs when a formula is
*loaded*, so a missing trust fails in under a second rather than after a build.

Two shortcuts look like they should work and do not, both verified against Homebrew 6.0.17 on
2026-08-16:

- Naming formulae in full trusts only the names you type, so
  `brew install dagsommer/boks/nerdbox dagsommer/boks/boks` still fails — on
  `libkrun/krun/libkrun`, which is a dependency and was never typed.
- `brew trust --formula libkrun/krun/libkrun` is not enough either. `libkrun` depends on
  `libkrunfw` and `virglrenderer` from the same tap and trust is not transitive, so the
  install stops at *"Refusing to load formula libkrun/krun/libkrunfw"*. Trust the tap, or
  name all three formulae.

### Then two things Homebrew cannot do for you

**1. Give yourself `/var/run/containerd`.** containerd derives the shim's socket path from a
compile-time constant, so no configuration setting moves it
([containerd#12444](https://github.com/containerd/containerd/issues/12444)). Without this
you get `creating sandbox process: mkdir /var/run/containerd: permission denied`.

```sh
sudo mkdir -p /var/run/containerd
sudo chown "$(id -u):$(id -g)" /var/run/containerd
```

This is the only step that needs root. Run containerd rootless afterwards — it works,
despite nerdbox's README note, provided you set `[ttrpc] address` alongside `[grpc]
address` and give both a `uid`/`gid`, or startup dies on
`chown …containerd.sock.ttrpc: operation not permitted`.

**2. Start containerd with the shim on *its* PATH.** containerd resolves a runtime handler
to an executable using the daemon's `PATH`, not your shell's. If you start it from a
launchd job or a `brew services` plist, that `PATH` is probably minimal and will not include
`$(brew --prefix)/bin`.

> [!TIP]
> `boks daemon start` handles this one: it starts containerd itself, with the directories
> Boks searches prepended to containerd's `PATH`. It also writes the rootless configuration
> described under point 1 — the `[ttrpc]` section with a `uid`/`gid` — so that half is no
> longer something to get right by hand either.

### The guest kernel and root filesystem, which the formula now fetches

This used to be a third thing to do by hand, and on this platform it no longer is: `boks.rb`
carries a `resource "guest"` pointing at the `boks-guest_<version>_arm64.tar.gz` asset a Boks
release publishes, and installs both files into `$(brew --prefix)/lib`. `nerdbox.rb` still
cannot build them — a Homebrew build has no Docker and no Linux cross-toolchain — so
`brew install nerdbox` on its own leaves the gap and says so in its caveat.

Build them yourself if you want to, or if you are not installing from the tap:

```sh
git clone https://github.com/dagsommer/boks && cd boks
scripts/build-nerdbox-guest.sh arm64
cp dist/nerdbox-guest-arm64/nerdbox-kernel-arm64 \
   dist/nerdbox-guest-arm64/nerdbox-rootfs.erofs \
   "$(brew --prefix)/lib/"
```

The script pins nerdbox's source by SHA-256 and refuses to continue if it does not match.
It needs **Docker**, because that is how nerdbox builds its guest — which is an awkward
requirement for a project whose selling point is not needing Docker Desktop, and the honest
mitigation is that these are *guest* artefacts: build them once on any Linux machine, or in
CI, and copy the two files over. Nothing about them is specific to your Mac.

**This one has been run.** On 2026-08-13, on an 18-core aarch64 Linux host, it produced a
15,835,648-byte `nerdbox-kernel-arm64` carrying the arm64 `ARMd` Image magic and an
8,343,552-byte `nerdbox-rootfs.erofs` carrying the EROFS superblock magic `0xe0f5e1e2`, in
about four minutes from a cold cache. **Those two files have still not been booted** — that
needs a hypervisor, which the machine that built them does not have. Their x86_64 counterparts,
from the same `buildx bake` recipe run in CI, have: a container ran on them under
`ctr` on Windows 11 on 2026-08-14 ([Verification](verification.md)). That is evidence about
the recipe, not about these two files.

`$(brew --prefix)/lib` is already on the shim's own search path on Apple silicon. Any
directory on containerd's `PATH` or on `LIBKRUN_PATH` works too — the shim scans both.

> [!NOTE]
> **`boks doctor` does check for these two files**, as `guest image`, scanning the same
> `PATH` and `LIBKRUN_PATH` the shim does. It used to report ready on a machine that then
> failed at boot with `nerdbox-kernel not found in PATH or LIBKRUN_PATH`; a `fail` on that
> line now says so up front. It also used to scan *your* `PATH` rather than the daemon's,
> which is not the one that decides — fixed on 2026-08-15: `doctor` now searches the `PATH`
> `boks daemon start` gives containerd, which is Boks' bundle directories prepended to your
> own (`internal/doctor/paths.go`). The one case it still cannot see is a containerd you
> started yourself with a different environment.

### What "installed" gets you

Run `boks doctor`. On a correctly set-up machine every line should be `ok`, with
`virtualization` a warning — it reports architecture support only, because
Hypervisor.framework cannot be probed without booting a VM. In particular `runtime
entitlement` should read `com.apple.security.hypervisor`; if it says the shim is unsigned
or lacks the entitlement, a sandbox will die inside libkrun with `krun_start_enter failed:
-22`, an error that names nothing at all.

## Linux — `.deb`, `.rpm`, tarball

**A sandbox boots on Linux, and its network policy is enforced.** Verified for the first
time on 2026-08-15, in WSL2 on Ubuntu 26.04: 25 of 26 checks passed, with three distinct
guest `boot_id`s, `nproc` following `--cpus` downward on an eight-core host, and — with all
eight proxy variables cleared — an allowed address completing TLS against the origin's own
Cloudflare certificate while `1.1.1.1:443` was refused in the same sandbox
([verification.md](verification.md)).

> [!IMPORTANT]
> **Two things about that result you need before you install.** It was measured *in WSL2*,
> not on bare metal, and while KVM is KVM either way, nothing on this project has run on a
> bare-metal Linux host. And **Linux today needs more privilege than Windows does**: as an
> ordinary user, sandbox creation fails with `mount source: "overlay", err: operation not
> permitted` — that is Boks itself host-mounting the image overlay to read the image config.
> Windows no longer has an equivalent requirement, which is the wrong way round and is being
> worked on. Until it is, expect to run as root on Linux.

The single failed check was the tester's own and was proved so: a workspace left `root:root`
by a root client is not writable by the guest's uid 1000, and changing only the ownership —
same binaries, same daemon — produced a successful write-through.

```sh
# Debian, Ubuntu
sudo dpkg -i boks_0.1.0_amd64.deb

# Fedora, RHEL, openSUSE
sudo rpm -i boks-0.1.0-1.x86_64.rpm

# anything else
tar -xzf boks_0.1.0_linux_amd64.tar.gz
sudo install -m0755 boks_0.1.0_linux_amd64/boks /usr/local/bin/boks
```

> [!NOTE]
> **Not published yet.** The release workflow that builds these packages exists and the
> files above are what it names — but no release has been cut, so there is nothing to
> download until the first tag. Until then, [build from source](#building-from-source).

The packages carry the runtime as well as the CLI: `boks` in `/usr/bin`, and `containerd`,
`containerd-shim-nerdbox-v1` and `libkrun.so` under `/usr/libexec/boks`. That directory is
where `boks daemon` looks and is deliberately not on your `PATH`, so a containerd you
installed on purpose is neither shadowed nor collided with. The runtime is vendored rather
than depended on because **no distribution ships a containerd new enough** — Ubuntu 24.04 has
1.7.x and 26.04 has 2.2.2, and a 2.2 daemon fails at task start — so a package declaring
`Depends: containerd` would produce a machine that looks provisioned and cannot start a
sandbox. `mkfs.erofs` stays a `Recommends`, being the one piece distributions package
properly.

What the Linux packages do **not** yet carry is the guest kernel and rootfs, which the Windows
archive does. They are published separately as `boks-guest_<version>_x86_64.tar.gz` and
`_arm64.tar.gz`; [What a sandbox needs](#what-a-sandbox-needs) below is the full list, and
`boks doctor` names each piece as it finds it missing.

### `apt-get install boks` — planned, not yet live

A real apt repository is planned, and its first ingredient exists: the archive signing key
is generated and its public half is committed at
[`packaging/apt/boks-archive-keyring.asc`](../packaging/apt/boks-archive-keyring.asc),
with the private half held as a CI secret. What remains is hosting — an apt repository is
static files over HTTPS, which GitHub Pages can serve — and an index-and-sign job in CI.

It is deliberately not being rushed. An apt repository is a standing commitment — indices
that must stay consistent, a URL that must keep working — and its whole benefit is
upgrades, which matter once there is a release to upgrade *from*. There is not yet.
Publishing a `.deb` on each release is the right size for where the project is; when the
repository goes live, this page will carry the `signed-by` setup lines.

## Windows — winget

**Boks runs a sandbox natively on Windows, with its network policy enforced, from an
ordinary unelevated terminal.** This is not WSL2 and it is not Hyper-V's management stack: it
is the **Windows Hypervisor Platform**, a user-mode hypervisor API, driven by a `krun.dll`
this project builds from a [37-patch series](../packaging/libkrun-windows/) against libkrun.

What was observed on real Windows 11 hardware, and the dates, because the platform changed
quickly ([verification.md](verification.md) has all of it):

| | |
|---|---|
| 2026-08-14 | `boks run --net nat shell <workspace> -- uname -a` exits 0 in 12.2 s and reports `Linux 6.12.44 … x86_64`. The guest attaches to Boks' own link socket, and the policy engine judges real traffic: `github.com:443` allowed, `example.com:443` denied at CONNECT |
| 2026-08-15 | The guest's wall clock is correct to the second against the host, and the allowed request returns **HTTP 200 from github.com** — fetched by a Linux container in a microVM on Windows, through Boks' own network stack. The denied host still fails `403` |
| 2026-08-15 | **No elevation, and no UAC prompt anywhere.** Every step from a shell reporting `elevated = False`, with Developer Mode proven *off* first. `A required privilege is not held` appears zero times in 565 KB of debug log |
| 2026-08-15 | Workspace write-through in both directions at the exact host path, LF preserved; `boks stop` / `boks rm` leave nothing behind; 2, 4 and 8 vCPUs all boot and `nproc` agrees |

```powershell
winget install boks
```

> [!NOTE]
> **Not published yet.** The manifests are written and schema-checked — they are in
> [`packaging/winget/`](../packaging/winget/) — but no release exists to install, nothing has
> been submitted to `microsoft/winget-pkgs`, and no one has run `winget` against any of it.
> That directory's README says exactly what has and has not been checked. Until it is live,
> [build from source](#building-from-source) or use the WSL2 route below.

### What the Windows download contains

Windows is the one platform where the whole stack arrives in a single file.
`boks_<version>_windows_amd64.zip` is what `winget install` fetches and what to download by
hand until it is live, and it holds:

| | |
|---|---|
| `boks.exe` | the CLI |
| `containerd.exe`, `ctr.exe` | the patched containerd this project builds, and its client |
| `containerd-shim-nerdbox-v1.exe` | turns a container into a microVM |
| `krun.dll` | the VMM, built from the 37-patch libkrun series |
| `mkfs.erofs.exe` | unpacking images for the guest |
| `nerdbox-kernel-x86_64`, `nerdbox-rootfs.erofs` | **the guest the microVM boots** |
| `config.toml`, `new-containerd-root.ps1`, `rwlayer-64m.img` | the configuration and the pre-created root an unelevated containerd does not work without, and the writable layer Windows cannot format for itself |
| `SHA256SUMS`, `SOURCE.txt`, `LICENSE`, `README.md`, `README-windows-runtime.md` | checksums over everything above, the guest kernel's GPL-2.0 source pointer, and the two READMEs |

Unzip it anywhere. Everything sits in one flat directory beside `boks.exe`, and **nothing needs
to go on your `PATH`**: `boks daemon start` prepends that directory to the `PATH` it starts
containerd with, and the shim finds `krun.dll`, the kernel and the rootfs by scanning that same
`PATH`.

> [!IMPORTANT]
> That only holds for the containerd `boks daemon start` runs. If you start the bundled
> `containerd.exe` yourself — which [windows-e2e.md](windows-e2e.md) does deliberately — its
> `PATH` is whatever your shell gave it, and you have to put the directory on `PATH` by hand.

**None of this has been through a tagged release.** The archive is assembled by
[`release.yml`](../.github/workflows/release.yml) from five other workflows' outputs plus its
own build of the CLI, and the layout above is what that job produces; no tag has run it, so
treat the first download as the test.

There is deliberately **no Windows CLI-only archive**. It would be a strict subset of the zip
above — the same `boks.exe` — differing only in that it cannot start a sandbox.
`boks-runtime_<version>_windows_amd64.zip` is published for the opposite case: everything above
*except* the CLI and the guest, for a `boks.exe` you built yourself or for driving containerd by
hand.

### Elevation is not required, and this page used to say it was

Worth stating plainly because earlier revisions of this document, and of
[windows-e2e.md](windows-e2e.md), told you to open an elevated terminal or to switch on
Developer Mode. That was true and is no longer.

The requirement was never Boks'. containerd's `NewBundle` created a **symlink** for every
task bundle, and unprivileged Windows will not create a symlink without
`SeCreateSymbolicLinkPrivilege` — which Windows grants only under Developer Mode.
[`packaging/containerd-windows/patches/0006`](../packaging/containerd-windows/) makes that
link a **junction** instead, which an ordinary user can create, keeping the symlink as a
fallback. Measured on 2026-08-15: the link is a junction (reparse tag `0xa0000003`), the
fallback never fired, `symlink` appears nowhere in the log, and teardown leaves neither the
bundle nor its target behind.

Nothing in the stack needs a service, an installer that writes to `Program Files`, or a
kernel driver. There is no `.sys` file anywhere in it.

### About that SmartScreen warning

**Boks ships unsigned, and Windows will say so — but only on one route.** Measured on
2026-08-16 against the first release archive, and the result is narrower than we expected:

- downloaded with a tool and run from a shell — `gh release download`, then `.\boks.exe` in
  PowerShell — **nothing happens**. `gh` writes no Mark of the Web, and a console
  `CreateProcess` does not consult SmartScreen's reputation check at all;
- downloaded in a **browser** and started by **double-clicking** it in Explorer, SmartScreen
  shows *"Windows protected your PC — Microsoft Defender SmartScreen prevented an
  unrecognized app from starting"*, with `Publisher: Unknown publisher`. **More info → Run
  anyway** works.

Nothing is wrong with your download when that happens. Defender's antivirus engine is not
involved and blocks nothing — this is the application-reputation check, which judges a file on
the reputation of the certificate that signed it. An unsigned binary has no certificate, so
there is nothing for a reputation to accrue against, and it does not improve with time the way
a signed one does. SmartScreen fires on a file carrying the Mark of the Web —
metadata Windows attaches to anything it considers downloaded — and it judges the file on the
reputation of the certificate that signed it. An unsigned binary has no certificate, so there
is nothing for a reputation to accrue against, and it does not improve with time the way a
signed one does.

Whether `winget install` trips it too depends on how winget marks what it fetched, and that
has not been measured — the observation above was a directly downloaded archive, not a winget
delivery.

This is a deliberate decision rather than an oversight. Code-signing certificates cost money
per year and an OV certificate still earns its reputation slowly over download volume; an EV
certificate clears SmartScreen immediately but needs a hardware token or a cloud HSM, which
complicates signing in CI. Neither winget nor Homebrew requires signing, so the cost buys
exactly one thing: a first-run dialog. For a project at this stage that is not yet worth
paying for, and the honest consequence is the paragraph you are reading rather than silence.

**What to do instead of trusting the dialog**: verify the download.
[Verifying what you downloaded](#verifying-what-you-downloaded) covers it. That is a stronger
check than a signature would give you, because it tells you which workflow at which commit
produced the file, rather than only that somebody paid for a certificate.

### What is not on Windows yet

- **`mkfs.ext4`**, which containerd wants for a container's writable layer, has no Windows
  build anywhere. The bundle ships a pre-formatted 64 MiB image instead. That is a workaround
  and is labelled as one.
- **One machine.** Every Windows result above comes from a single Windows 11 x64 host. There
  is no Windows arm64 build at all — the WHP backend and the guest kernel are both x86_64.
- **No release has been cut.** Every Windows piece is now a *release asset* rather than an
  expiring artifact — `release.yml` calls the five workflows that build them and assembles the
  zip described above — but that workflow has never run on a tag. See
  [distribution.md](distribution.md).

[windows.md](windows.md) has the full architectural picture and
[windows-e2e.md](windows-e2e.md) the by-hand procedure using `ctr` rather than `boks`.

### The WSL2 route still works

If you would rather run the Linux build inside WSL2, that is still a supported answer, and
since 2026-08-15 it is the *verified* one: the end-to-end Linux run above happened in WSL2.
Boks is then an ordinary Linux program on an ordinary Linux kernel, and the exact-path
workspace behaviour is preserved because a WSL2 workspace is already a Linux path. It needs
WSL ≥ 2.5.1, `kvm` and `erofs` modules loaded, and `/dev/kvm` made group-accessible —
`boks doctor` diagnoses each of those specifically, and
[Troubleshooting](troubleshooting.md#wsl2) has the fixes.

Choosing between them: the native route needs no elevation, and the WSL2 route currently does
need root inside the distribution (see the Linux section above). That is the reverse of what
anyone would guess, and it is the current state rather than the intended one.

## What a sandbox needs

> [!IMPORTANT]
> **A `boks` binary on its own cannot start a sandbox.** It orchestrates a stack it does not
> contain: containerd, a VM shim, a hypervisor library and a filesystem tool. On a fresh
> machine every one of those is missing, and `boks doctor` will tell you so.

**One of them Boks will now run for you.** `boks daemon start` writes a containerd
configuration for this host and starts containerd with it:

```sh
boks daemon start     # writes the config, runs containerd, waits until it answers
boks daemon status    # is it running, and is it actually serving
boks daemon config    # the configuration, with the reason for every setting
boks daemon logs      # what containerd wrote
```

It does not install containerd — you still need one, version 2.3 or later — and it does not
touch a containerd you already run. The daemon it starts has its own root, its own state and
its own endpoint under your state directory, so a machine running containerd for Docker keeps
doing so and the two never see each other. Nothing is installed as a service and nothing runs
at boot. Once it is up, Boks talks to it by default; `--containerd-address` and
`BOKS_CONTAINERD_ADDRESS` still override that.

The point of it is the configuration rather than the convenience. containerd's defaults are
wrong for Boks in ways that fail late and name something else: on Linux its diff-service order
is `['walking']`, which cannot unpack the stacked EROFS layers a sandbox boots from, and its
listeners are chowned to uid 0, which kills a rootless daemon before it serves anything. Both
are settings, and `boks daemon config` prints them with the failure each one prevents. See
[distribution.md](distribution.md) for the full list and the evidence.

| Piece | Why | macOS/arm64 | Linux | Windows |
|---|---|---|---|---|
| `boks` | the CLI | Homebrew, or a tarball | tarball, `.deb`, `.rpm` | in the zip |
| containerd ≥ 2.3 | Boks drives it through its Go API | `brew install containerd` (2.3.4) | in the `.deb`/`.rpm` | in the zip, patched |
| `containerd-shim-nerdbox-v1` | turns a container into a microVM | built from source by the tap's formula | in the `.deb`/`.rpm`, or [from CI](#prebuilt-shim-and-libkrun-for-linux) | in the zip, patched |
| nerdbox guest kernel + `nerdbox-rootfs.erofs` | what the microVM boots | **build with Docker**, or the `arm64` guest archive by hand | the guest archive, or [from CI](#prebuilt-shim-and-libkrun-for-linux) | in the zip |
| libkrun ≥ 1.18 | the VMM | `brew install libkrun/krun/libkrun` | in the `.deb`/`.rpm`, or [from CI](#prebuilt-shim-and-libkrun-for-linux) | in the zip, as `krun.dll` |
| `mkfs.erofs` (erofs-utils ≥ 1.8) | unpacking images for the guest | `brew install erofs-utils` (1.9.3) | packaged, often too old | in the zip, patched |
| `mkfs.ext4` (e2fsprogs) | formatting each sandbox's writable layer | `brew install e2fsprogs` — pulled in by the formula; keg-only, and Boks puts the keg on containerd's PATH | **not needed**: Linux runs the snapshotter in ovlfs mode and never formats one | in the zip, cross-compiled |

The one cell still in bold is why this page is longer than one install command — and note what
the rest of the table says now: **the Windows column is the only one with no gap in it**, which
is the reverse of where this project started. "In the zip" means
`boks_<version>_windows_amd64.zip`, described under [What the Windows download
contains](#what-the-windows-download-contains). None of it has been through a tagged release.

The Windows column went from "—" to "build it yourself" on 2026-08-14, and to "in the zip"
when `release.yml` learned to assemble one: every one of those pieces has been built for
Windows and run together — and, since 2026-08-15, driven by `boks`
itself rather than by `ctr`. It takes 48 carried patches to get there: 37 against libkrun, 6
against containerd and 5 against nerdbox, all in [`packaging/`](../packaging/). None of it is
packaged yet, and `mkfs.ext4` — which containerd wants for a container's writable layer —
still has no Windows build at all, so the bundle ships a pre-formatted image and
[windows-e2e.md](windows-e2e.md) supplies one by hand.

**nerdbox is packaged nowhere.** Not homebrew-core, not the AUR, not nixpkgs, not Debian,
and not by Repology in any of the ~400 repositories it tracks. Its own release workflow has
failed on every tag since v0.2.0, so all ten of its GitHub releases carry zero assets — there
is no prebuilt shim, kernel or rootfs to download *from upstream*, for any platform.

### Prebuilt shim and libkrun for Linux

Because upstream publishes nothing, this repository builds them. Two workflows between them
cover everything on the list above except `mkfs.erofs`:

| Workflow | Artifact | Contents |
|---|---|---|
| [`linux-runtime`](../.github/workflows/linux-runtime.yml) | `boks-runtime-linux-<arch>` | `containerd`, `containerd-shim-nerdbox-v1`, `libkrun.so` |
| [`guest-image`](../.github/workflows/guest-image.yml) | `nerdbox-guest-<arch>` | `nerdbox-kernel-<arch>`, `nerdbox-rootfs.erofs` — the rootfs is **not** architecture-suffixed |

Download the artifacts from a run's summary page, then:

```sh
sudo install -m0755 containerd-shim-nerdbox-v1 /usr/local/bin/
sudo install -m0644 libkrun.so /usr/local/lib/
sudo install -m0644 nerdbox-kernel-* nerdbox-rootfs.erofs /usr/local/lib/
# containerd is found beside the boks binary, or in ../libexec/boks, or wherever
# BOKS_RUNTIME_DIR names — not on PATH.
sudo install -m0755 containerd /usr/local/libexec/boks/
```

All three binaries are **unpatched upstream**, built for amd64 and arm64 from the revisions
pinned in [`packaging/nerdbox/NERDBOX_REV`](../packaging/nerdbox/NERDBOX_REV),
[`packaging/linux/LIBKRUN_REV`](../packaging/linux/LIBKRUN_REV) and
[`packaging/containerd-linux/CONTAINERD_VERSION`](../packaging/containerd-linux/CONTAINERD_VERSION).
[`packaging/linux/README.md`](../packaging/linux/README.md) covers where each file has to go
and why `libkrun.so`'s filename matters — the shim stats for two exact names and never asks
the dynamic linker.

> [!NOTE]
> **Downloading these is not the same as them working.** CI proves the files are ELF objects
> of the right architecture and that `libkrun.so` exports every symbol the shim resolves at
> `dlopen` — a load-time contract, and a real one, since libkrun 2.x drops four of them. It
> does not start a VM: GitHub's runners have no `/dev/kvm`. These artifacts exist to make an
> end-to-end Linux run possible without a two-project build first, and that is what they were
> used for on 2026-08-15 — but the run that verified Linux is not the same thing as CI
> verifying these files, and CI still only checks the load-time contract.

### Or build it yourself

The build route still works and is not going away; it is simply no longer the only one.
Concretely, on Linux:

- **containerd ≥ 2.3** from upstream's static binaries or from source. Not 2.2:
  the nerdbox shim emits version-3 bootstrap parameters, which a 2.2 daemon cannot
  decode — it falls back to reading the whole protobuf reply as an address and fails
  with `unsupported protocol: Yunix`, the three leading control bytes rendering as
  letters. Measured on Ubuntu 26.04's containerd 2.2.2, 2026-08-15;
- **nerdbox**, built from source — `task build` builds everything including the guest, and
  needs Docker with buildx; on Linux you also want its `libkrun` bake target, since libkrun
  is not generally packaged either. [`packaging/linux/README.md`](../packaging/linux/README.md)
  has the two short commands if you want only the shim and the library rather than the whole
  `task build`, and the assertion script to check what you built;
- **erofs-utils ≥ 1.8** — Ubuntu 24.04's 1.7.1 is too old for containerd's erofs
  snapshotter, where it surfaces as a confusing failure partway through an image unpack;
  `boks doctor` reads `mkfs.erofs -V` and fails on anything older;
- **`/dev/kvm`**, and membership of the `kvm` group.

Here is the whole of what `boks doctor` says on a machine with none of them, which is a
fair picture of where a fresh Linux install starts:

```
platform             ok     linux/arm64
virtualization       fail   /dev/kvm missing
containerd           fail   unreachable at /run/containerd/containerd.sock
snapshotter          warn   could not list snapshotters
snapshotter tools    ok     /usr/bin/mkfs.erofs (erofs-utils 1.9)
vm runtime           fail   containerd-shim-nerdbox-v1 not found on PATH
runtime skew         skip   no shim to compare (see vm runtime)
hypervisor library   warn   libkrun.so not found where the shim looks
guest image          fail   nerdbox-kernel-arm64 and nerdbox-rootfs.erofs not found

Not ready: virtualization, containerd, vm runtime, guest image must be fixed before sandboxes can start.
```

## Verifying what you downloaded

**The binaries are not code-signed on any platform.** Nothing carries an Authenticode
signature on Windows or a Developer ID signature on macOS, and the packaging routes do not
require one — neither winget nor Homebrew asks for a signature. What each release carries
instead is a `SHA256SUMS` covering every artifact, and a build provenance attestation per
artifact.

```sh
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify boks_0.1.0_darwin_arm64.tar.gz --repo dagsommer/boks
```

The attestation says which workflow, at which commit, produced the file — which is more than
a code-signing certificate tells you, since a certificate says only that somebody paid for
one. The **CLI tarballs** are also byte-reproducible: `scripts/build-release.sh` sorts the
archive, zeroes ownership and takes timestamps from the commit date, so building the same tag
on your own machine gives the same checksum rather than merely a working binary. The zips are
not, and are not claimed to be — zip records real mtimes and directory order, and neither
libkrun nor the guest kernel is a reproducible build. For those, the `SHA256SUMS` **inside**
the archive is the thing to check: it covers the files rather than the container.

**`SHA256SUMS` is GPG-signed**, with the archive key committed at
[`packaging/apt/boks-archive-keyring.asc`](../packaging/apt/boks-archive-keyring.asc)
(fingerprint `D5DD07C0F9589C164F7361C20EB93D3C39471E1E`) — the same key the apt repository will
use, so there is one key to trust rather than two. A detached signature over the checksum file
covers every asset transitively.

```sh
curl -fsSL https://raw.githubusercontent.com/dagsommer/boks/main/packaging/apt/boks-archive-keyring.asc | gpg --import
gpg --verify SHA256SUMS.asc SHA256SUMS
```

> [!NOTE]
> **The signing step exists in `release.yml`; no tag has ever run it.** A tag that cannot reach
> the key fails rather than publishing an unsigned checksum file, and a manually dispatched dry
> run warns and continues — but "the workflow contains the step" is a claim about the file, and
> whether GPG signs cleanly on a runner is a claim about a run that has not happened. The
> attestation is the stronger of the two checks either way, because it does not require
> trusting a key at all.

On Windows this is also the answer to the SmartScreen dialog: see
[About that SmartScreen warning](#about-that-smartscreen-warning).

There is deliberately no `curl | sh` installer. Boks refuses to install agent CLIs that
way and pins every download it makes with a checksum; installing Boks itself is a poor
place to make an exception.

## Building from source

The route that works today, and the one the [walkthrough](walkthrough.md) assumes.

```sh
git clone https://github.com/dagsommer/boks && cd boks
make build       # ./bin/boks
./bin/boks doctor
```

Go 1.26 or later. Nothing in Boks uses cgo, so `make dist` cross-compiles every published
target from any host:

```sh
make dist        # tarballs + checksums for darwin/arm64, linux/{amd64,arm64} and windows/amd64
```

`make dist` and `release.yml`'s build matrix name the same four platforms, and a test in
`internal/release/` fails if they ever stop doing so. The one difference is what happens to the
Windows tarball afterwards: a release does not publish it, because the Windows CLI ships inside
`boks_<version>_windows_amd64.zip` together with the runtime and guest it cannot start a
sandbox without. Locally it is just a tarball with a `boks.exe` in it.

`.deb` and `.rpm` additionally need `dpkg-deb` and `rpmbuild`:

```sh
scripts/package-linux.sh amd64
```

Building the CLI does not build the stack under it — `boks doctor` will name what your
machine is still missing, and [What a sandbox needs](#what-a-sandbox-needs) is the list.

## What is not offered, and what each would need

| | Status | What it needs |
|---|---|---|
| Homebrew tap | formulae written, linted and audited against a real Homebrew; tap not created | a published release — the draft `v0.1.0` has no tag, so the formula's `url` 404s — and a `dagsommer/homebrew-boks` repository. [`packaging/homebrew/README.md`](../packaging/homebrew/README.md) is the numbered procedure |
| Homebrew bottles | none | a `brew bottle` run per macOS version on Apple silicon, and somewhere to host them |
| apt repository | signing key created; not hosted | hosting for the static repository, and an index-and-sign job in CI |
| winget | manifests written and schema-checked; nothing submitted | a tag, so the `InstallerUrl` resolves, then a pull request to `microsoft/winget-pkgs` behind a signed CLA and a human moderator's approval — [`packaging/winget/README.md`](../packaging/winget/) has the whole list |
| code signing | **deliberately not offered** | money, and — for the EV certificate that clears SmartScreen at once — a hardware token or cloud HSM in CI |
| a `mkfs.ext4` for Windows | no build exists anywhere | someone to port it. The Windows archive ships a pre-formatted 64 MiB image instead, which is a workaround and is labelled as one |
| an aarch64 Windows build | not possible today | a `krun.dll` and an `mkfs.erofs.exe` for aarch64 Windows; neither recipe cross-compiles beyond x86-64 |

**The guest kernel and rootfs used to be the row that mattered most here**, because they are
the difference between an install that works and one that reaches a passing `doctor` and a
sandbox that will not boot. `release.yml` now publishes them — as `boks-guest_<version>_x86_64.tar.gz`
and `boks-guest_<version>_arm64.tar.gz`, and inside `boks_*_windows_amd64.zip` — so on Windows
there is nothing left to fetch. `boks.rb` fetches the `arm64` archive as a `resource` and
installs both files into `$(brew --prefix)/lib`, so the tap closes it on macOS too. The `.deb`
and `.rpm` are the remaining gap: `scripts/package-linux.sh` will place the guest if handed
it, and `release.yml` does not hand it over.

The kernel is GPL-2.0 and nerdbox patches it before building, so distributing the compiled
result carries a corresponding-source obligation. Every archive that contains it ships a
`SOURCE.txt` naming the exact nerdbox revision and the two `docker buildx bake` commands that
reproduce it, on the reading that a wholly public recipe plus a precise pointer to it satisfies
the obligation. **That reading was implemented in CI rather than ruled on by the owner**, and
[distribution.md](distribution.md#the-one-decision-blocking-the-most) is where it is stated and
where reversing it would start.
