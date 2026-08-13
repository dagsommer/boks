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
every gap. There is one thing it does *not* check, called out under macOS below.

## macOS on Apple silicon — Homebrew

**The only platform where Boks has been shown to work.** The VM boundary and the network
policy were both measured here; see [verification.md](verification.md).

```sh
brew tap dagsommer/boks
brew trust dagsommer/boks
brew trust --formula libkrun/krun/libkrun
brew install boks
```

> [!NOTE]
> **Not published yet.** The tap does not exist. The formulae are written and reviewed — they
> are in [`packaging/homebrew/`](../packaging/homebrew/) with instructions for creating the
> tap — but none of this has been run: there is no macOS machine on this project's CI, and
> the formulae have been checked for syntax and read against Homebrew's source, nothing more.
> Until the tap is live, [build from source](#building-from-source).

That installs the whole stack — containerd, erofs-utils, libkrun and a nerdbox shim signed
with the entitlement libkrun needs — rather than the CLI alone, because a CLI alone would
land on a machine where every check fails.

**Why the two `brew trust` lines.** Since Homebrew 6.0.0 a non-official tap must be trusted
before Homebrew will load Ruby from it. Naming a formula in full on the command line trusts
that formula implicitly, so `brew install dagsommer/boks/nerdbox dagsommer/boks/boks` also
works — but only for the names you actually type. `libkrun/krun/libkrun` is always reached
as a dependency and so always needs saying out loud. `sbx` has the same requirement, which
is why its install line begins `brew trust docker/tap`.

### Then three things Homebrew cannot do for you

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

**3. Build the guest kernel and root filesystem.** This is the gap, and it is the one
`boks doctor` will not warn you about.

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
about four minutes from a cold cache. What has *not* been shown is either file booting:
that needs a hypervisor, which the machine that built them does not have.

`$(brew --prefix)/lib` is already on the shim's own search path on Apple silicon. Any
directory on containerd's `PATH` or on `LIBKRUN_PATH` works too — the shim scans both.

> [!WARNING]
> **`boks doctor` does not check for these two files.** It checks the shim, its
> entitlement, libkrun, containerd and `mkfs.erofs` — so a machine without them reports
> ready and then fails at boot with `nerdbox-kernel not found in PATH or LIBKRUN_PATH`.
> Until `doctor` grows a check for the guest assets, this paragraph is the check.

### What "installed" gets you

Run `boks doctor`. On a correctly set-up machine every line should be `ok`, with
`virtualization` a warning — it reports architecture support only, because
Hypervisor.framework cannot be probed without booting a VM. In particular `runtime
entitlement` should read `com.apple.security.hypervisor`; if it says the shim is unsigned
or lacks the entitlement, a sandbox will die inside libkrun with `krun_start_enter failed:
-22`, an error that names nothing at all.

## Linux — `.deb`, `.rpm`, tarball

**The KVM path is built and designed for, and has not been verified end to end**: no sandbox
has yet booted on Linux. The binaries build and pass their tests, and that is the whole of
the claim today. See [which platforms work](get-started.md#which-platforms-work).

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

The packages contain the CLI, its licence and shell completions. They install no
dependencies beyond a `Recommends: erofs-utils`, and the reason is in their own description:
**no distribution ships a containerd new enough**, and nerdbox is packaged nowhere. A
package that declared `Depends: containerd` would install 1.7.x on Ubuntu 24.04 and produce
a machine that looks provisioned and cannot start a sandbox. So on Linux you are assembling
the rest of the stack yourself — the list is under
[What a sandbox needs](#what-a-sandbox-needs) below, and `boks doctor` names each piece as
it finds it missing.

### `apt-get install boks` — planned, not yet live

A real apt repository is planned, and its first ingredient exists: the archive signing key
is generated and its public half is committed at
[`packaging/apt/boks-archive-keyring.asc`](../packaging/apt/boks-archive-keyring.asc),
with the private half held as a CI secret. What remains is hosting — an apt repository is
static files over HTTPS, which GitHub Pages can serve — and an index-and-sign job in CI.

It is deliberately not being rushed. An apt repository is a standing commitment — indices
that must stay consistent, a URL that must keep working — and its whole benefit is
upgrades, which matter once Linux is verified and someone is upgrading. Publishing a `.deb`
on each release is the right size for where the project is; when the repository goes live,
this page will carry the `signed-by` setup lines.

## Windows — in progress

**Native Windows support is being built, and no sandbox has ever booted on Windows.** Both
halves of that sentence matter. The work is real and visible: a Windows Hypervisor Platform
backend for libkrun is being developed in this repository's
[patch series](../packaging/libkrun-windows/), most of the VMM now compiles for Windows in
CI, and upstream nerdbox already builds a Windows shim that loads the VMM as `krun.dll`.
What remains is the `krun.dll` C API layer and `virtio-net` — the one device Boks'
enforcement depends on — and until a VM boots there, no Windows binary is shipped: a binary
whose only possible output is a refusal would make the platform look done and move the
disappointment to a later, more annoying point. [windows.md](windows.md) has the full
picture.

### winget — waits on the above

```powershell
winget install boks   # not published — nothing to install yet
```

A winget manifest needs something that works to install. The native binary cannot start a
sandbox yet, and winget installs packages *on Windows*, not inside a WSL distribution — so
it cannot deliver the one Windows story Boks has today. The manifest gets written when a
native sandbox boots.

### Meanwhile: run the Linux build inside WSL2

Boks is then an ordinary Linux program on an ordinary Linux kernel, and the exact-path
workspace behaviour is preserved because a WSL2 workspace is already a Linux path. It needs
WSL ≥ 2.5.1, `kvm` and `erofs` modules loaded, and `/dev/kvm` made group-accessible —
`boks doctor` diagnoses each of those specifically, and
[Troubleshooting](troubleshooting.md#wsl2) has the fixes. Designed for, not yet run by
anyone on this project.

## What a sandbox needs

> [!IMPORTANT]
> **A `boks` binary on its own cannot start a sandbox.** It orchestrates a stack it does not
> contain: containerd, a VM shim, a hypervisor library and a filesystem tool. On a fresh
> machine every one of those is missing, and `boks doctor` will tell you so.

| Piece | Why | macOS/arm64 | Linux | Windows |
|---|---|---|---|---|
| `boks` | the CLI | Homebrew, or a tarball | tarball, `.deb`, `.rpm` | in progress — see above |
| containerd ≥ 2.2 | Boks drives it through its Go API | `brew install containerd` (2.3.4) | **not packaged at a usable version** — Ubuntu 24.04 has 1.7.x | — |
| `containerd-shim-nerdbox-v1` | turns a container into a microVM | built from source by the tap's formula | **build from source** | — |
| nerdbox guest kernel + `nerdbox-rootfs.erofs` | what the microVM boots | **build with Docker** | **build with Docker** | — |
| libkrun ≥ 1.18 | the VMM | `brew install libkrun/krun/libkrun` | build from source, or a distro that has it | — |
| `mkfs.erofs` (erofs-utils ≥ 1.8) | unpacking images for the guest | `brew install erofs-utils` (1.9.3) | packaged, often too old | — |

The cells in bold are why this page is longer than one install command.

**nerdbox is packaged nowhere.** Not homebrew-core, not the AUR, not nixpkgs, not Debian,
and not by Repology in any of the ~400 repositories it tracks. Its own release workflow has
failed on every tag since v0.2.0, so all ten of its GitHub releases carry zero assets —
there is no prebuilt shim, kernel or rootfs to download for any platform. Everything above
that involves nerdbox involves building it.

On Linux, concretely, that means:

- **containerd ≥ 2.2** from upstream's static binaries or from source;
- **nerdbox**, built from source — `task build` builds everything including the guest, and
  needs Docker with buildx; on Linux you also want its `libkrun` bake target, since libkrun
  is not generally packaged either;
- **erofs-utils ≥ 1.8** — Ubuntu 24.04's 1.7.1 is too old for containerd's erofs
  snapshotter, and `boks doctor` checks that `mkfs.erofs` exists but not its version, so
  this surfaces later as a confusing failure during an image unpack;
- **`/dev/kvm`**, and membership of the `kvm` group.

Here is the whole of what `boks doctor` says on a machine with none of them, which is a
fair picture of where a fresh Linux install starts:

```
platform             ok     linux/arm64
virtualization       fail   /dev/kvm missing
containerd           fail   unreachable at /run/containerd/containerd.sock
snapshotter          warn   could not list snapshotters
snapshotter tools    ok     /usr/bin/mkfs.erofs
vm runtime           fail   containerd-shim-nerdbox-v1 not found on PATH
hypervisor library   warn   libkrun.so.1 not found

Not ready: virtualization, containerd, vm runtime must be fixed before sandboxes can start.
```

## Verifying what you downloaded

Every release carries a `SHA256SUMS` covering every artifact, and each artifact carries a
build provenance attestation.

```sh
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify boks_0.1.0_darwin_arm64.tar.gz --repo dagsommer/boks
```

The attestation says which workflow, at which commit, produced the file. The tarballs are
also byte-reproducible: `scripts/build-release.sh` sorts the archive, zeroes ownership and
takes timestamps from the commit date, so building the same tag on your own machine gives
the same checksum rather than merely a working binary.

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
make dist        # tarballs + checksums for darwin/arm64 and linux/{amd64,arm64}
```

`.deb` and `.rpm` additionally need `dpkg-deb` and `rpmbuild`:

```sh
scripts/package-linux.sh amd64
```

Building the CLI does not build the stack under it — `boks doctor` will name what your
machine is still missing, and [What a sandbox needs](#what-a-sandbox-needs) is the list.

## What is not offered, and what each would need

| | Status | What it needs |
|---|---|---|
| Homebrew tap | formulae written, tap not created | a `dagsommer/homebrew-boks` repository, and the release tarball's checksum |
| Homebrew bottles | none | a `brew bottle` run per macOS version on Apple silicon, and somewhere to host them |
| apt repository | signing key created; not hosted | hosting for the static repository, and an index-and-sign job in CI |
| winget | none | a Windows binary that can start a sandbox — see [Windows](#windows--in-progress) |
| nerdbox guest assets on the release | buildable, not published | a decision about distributing a GPL-2.0 kernel binary |

The last row is the one that matters most: it is the difference between an install that
works and an install that gets you to a passing `doctor` and a sandbox that will not boot.
It is also no longer a technical problem. `scripts/build-nerdbox-guest.sh` produces both
files in about four minutes on an ordinary Linux runner, so a release job could attach them
and `packaging/homebrew/nerdbox.rb` could then fetch them pinned by SHA-256 — the formula
change is a `resource` block and one `lib.install`, spelled out in
[packaging/homebrew/README.md](../packaging/homebrew/README.md).

What stands in the way is a decision rather than an obstacle. The kernel is GPL-2.0 and
nerdbox patches it before building, so distributing the compiled result carries a
corresponding-source obligation. The recipe is entirely public — a pinned `cdn.kernel.org`
tarball, a config and a patch set from nerdbox's repository — so satisfying it is a matter
of publishing that alongside. But it is the owner's call to make deliberately, which is why
this repository builds those files and does not ship them.
