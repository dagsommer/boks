# Installing Boks

> [!IMPORTANT]
> **A `boks` binary on its own cannot start a sandbox.** It orchestrates a stack it does not
> contain: containerd, a VM shim, a hypervisor library and a filesystem tool. On a fresh
> machine every one of those is missing, and `boks doctor` will tell you so. This document
> is organised around that fact rather than around the download.

Run `boks doctor` immediately after installing, whichever route you take. It is the only
thing that knows whether your machine can actually start a sandbox, and it prints a remedy
for every gap. There is one thing it does *not* check, and it is called out below.

## What a sandbox needs

| Piece | Why | macOS/arm64 | Linux | Windows |
|---|---|---|---|---|
| `boks` | the CLI | Homebrew, or a tarball | tarball, `.deb`, `.rpm` | not shipped |
| containerd ≥ 2.2 | Boks drives it through its Go API | `brew install containerd` (2.3.4) | **not packaged at a usable version** — Ubuntu 24.04 has 1.7.x | — |
| `containerd-shim-nerdbox-v1` | turns a container into a microVM | built from source by the tap's formula | **build from source** | — |
| nerdbox guest kernel + `nerdbox-rootfs.erofs` | what the microVM boots | **build with Docker** | **build with Docker** | — |
| libkrun ≥ 1.18 | the VMM | `brew install libkrun/krun/libkrun` | build from source, or a distro that has it | — |
| `mkfs.erofs` (erofs-utils ≥ 1.8) | unpacking images for the guest | `brew install erofs-utils` (1.9.3) | packaged, often too old | — |

The cells in bold are why this document is longer than `brew install boks`.

**nerdbox is packaged nowhere.** Not homebrew-core, not the AUR, not nixpkgs, not Debian,
and not by Repology in any of the ~400 repositories it tracks. Its own release workflow has
failed on every tag since v0.2.0, so all ten of its GitHub releases carry zero assets —
there is no prebuilt shim, kernel or rootfs to download for any platform. Everything below
that involves nerdbox involves building it.

## macOS on Apple silicon

**The only platform where Boks has been shown to work.** The VM boundary and the network
policy were both measured here; see [verification.md](verification.md).

### Install

```sh
brew tap dagsommer/boks
brew trust dagsommer/boks
brew trust --formula libkrun/krun/libkrun
brew install boks
```

That pulls in containerd, erofs-utils, libkrun and a nerdbox shim signed with the
entitlement libkrun needs. It is a whole-stack install rather than a CLI, because a CLI
alone would land on a machine where every check fails.

**Why the two `brew trust` lines.** Since Homebrew 6.0.0 a non-official tap must be trusted
before Homebrew will load Ruby from it. Naming a formula in full on the command line trusts
that formula implicitly, so `brew install dagsommer/boks/nerdbox dagsommer/boks/boks` also
works — but only for the names you actually type. `libkrun/krun/libkrun` is always reached
as a dependency and so always needs saying out loud. `sbx` has the same requirement, which
is why its install line begins `brew trust docker/tap`.

> The tap does not exist yet. The formulae are in
> [`packaging/homebrew/`](../packaging/homebrew/) with instructions for creating it, and
> **none of this has been run** — there is no macOS machine on this project's CI and the
> formulae have been checked for syntax and read against Homebrew's source, nothing more.

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

## Linux

**Untested end to end.** The Linux/KVM path is designed for and has never been exercised:
no sandbox has ever booted on Linux. The binaries build and pass their tests, and that is
the whole of the claim.

```sh
# Debian, Ubuntu
sudo dpkg -i boks_0.1.0_amd64.deb

# Fedora, RHEL, openSUSE
sudo rpm -i boks-0.1.0-1.x86_64.rpm

# anything else
tar -xzf boks_0.1.0_linux_amd64.tar.gz
sudo install -m0755 boks_0.1.0_linux_amd64/boks /usr/local/bin/boks
```

The packages contain the CLI, its licence and shell completions. They install no
dependencies beyond a `Recommends: erofs-utils`, and the reason is in their own description:
**no distribution ships a containerd new enough**, and nerdbox is packaged nowhere. A
package that declared `Depends: containerd` would install 1.7.x on Ubuntu 24.04 and produce
a machine that looks provisioned and cannot start a sandbox.

So on Linux you are assembling the stack yourself:

- **containerd ≥ 2.2** from upstream's static binaries or from source;
- **nerdbox**, built from source — `task build` builds everything including the guest, and
  needs Docker with buildx; on Linux you also want its `libkrun` bake target, since libkrun
  is not generally packaged either;
- **erofs-utils ≥ 1.8** — Ubuntu 24.04's 1.7.1 is too old for containerd's erofs
  snapshotter, where it surfaces as a confusing failure partway through an image unpack;
  `boks doctor` reads `mkfs.erofs -V` and fails on anything older;
- **`/dev/kvm`**, and membership of the `kvm` group.

`boks doctor` names each of these as it finds them missing. Here is the whole of what it
says on a machine with none of them, which is a fair picture of where a fresh Linux install
starts:

```
platform             ok     linux/arm64
virtualization       fail   /dev/kvm missing
containerd           fail   unreachable at /run/containerd/containerd.sock
snapshotter          warn   could not list snapshotters
snapshotter tools    ok     /usr/bin/mkfs.erofs (erofs-utils 1.9)
vm runtime           fail   containerd-shim-nerdbox-v1 not found on PATH
hypervisor library   warn   libkrun.so not found where the shim looks

Not ready: virtualization, containerd, vm runtime must be fixed before sandboxes can start.
```

### There is no apt repository

`.deb` and `.rpm` files on the release are what exists. A real `apt-get install boks`
needs three things this repository cannot provide, listed in the order they would have to
be decided:

1. **A GPG signing key, generated and held by the owner.** apt trusts a repository through
   a signed `InRelease` file; without one, users must pass `[trusted=yes]`, which disables
   the only integrity check apt has. The key is the owner's to create and keep — nobody
   else should generate it, and it is deliberately not generated here. A signing *subkey*
   exported for CI, with the primary key kept offline, is the usual arrangement.
2. **Hosting.** An apt repository is static files over HTTPS: a `pool/` of `.deb`s and a
   `dists/stable/` of indices. **GitHub Pages can serve one** — it is exactly static files,
   and no directory listing is needed since apt requests known paths. Two caveats: a Pages
   site is capped at 1 GB with a soft 100 GB/month bandwidth limit, and every `.deb` would
   live in the repository serving the site, so a documentation site and a package pool
   would share a history and a size budget. A separate `dagsommer/boks-apt` repository with
   its own Pages site avoids that and costs nothing.
3. **Index generation in CI.** `apt-ftparchive` (from `apt-utils`) or `reprepro` builds
   `Packages`, `Packages.gz` and `Release`; the key then signs `Release` into `InRelease`.
   That job holds the signing key and so — following the same rule as the rest of this
   project's CI — must not be the job that runs any third-party build.

Users would then run something like:

```sh
curl -fsSL https://<host>/boks-archive-keyring.gpg \
  | sudo tee /etc/apt/keyrings/boks.gpg >/dev/null
echo "deb [signed-by=/etc/apt/keyrings/boks.gpg] https://<host>/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/boks.list
sudo apt-get update && sudo apt-get install boks
```

**The recommendation is to wait.** An apt repository is a standing commitment — a key that
must not leak, indices that must stay consistent, a URL that must keep working — and its
whole benefit is upgrades. Boks has no users, and its Linux support has never started a
sandbox. Publishing a `.deb` on each release costs nothing and is the right size for where
the project is; the repository is worth building when Linux is verified and someone is
upgrading.

## Windows

**There is no Windows binary, and that is deliberate.** `GOOS=windows go build ./...`
succeeds, and that is the entire extent of it: libkrun has no Windows virtio-net backend,
so a guest cannot get the NIC on which Boks enforces network policy, and there is no VMM
behind the shim to boot a VM at all. Shipping a binary whose only possible output is a
refusal would make the platform look supported and move the disappointment to a later,
more annoying point.

**Run the Linux build inside WSL2.** Boks is then an ordinary Linux program on an ordinary
Linux kernel, and the exact-path workspace behaviour is preserved because a WSL2 workspace
is already a Linux path. It needs WSL ≥ 2.5.1, `kvm` and `erofs` modules loaded, and
`/dev/kvm` made group-accessible — `boks doctor` diagnoses each of those specifically.
Nobody has run it. [windows.md](windows.md) has the full picture, including what a native
port would need and why the obstacle is one device driver rather than the platform.

### There is no winget package, and it should stay that way for now

A winget package would require a manifest PR to `microsoft/winget-pkgs` and, before that,
something to install. The two candidates both fail:

- **the native Windows binary** — it cannot start a sandbox, so publishing it into the
  mainstream Windows package index would be a claim of support that is not true;
- **the WSL2 route** — winget installs packages *on Windows*, not inside a WSL
  distribution. It cannot deliver the one Windows story Boks actually has.

So winget waits on libkrun's Windows virtio-net backend, which is upstream work targeting
libkrun 2.0. See [upstream-libkrun-virtio-net.md](upstream-libkrun-virtio-net.md).

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

## What is not offered, and what each would need

| | Status | What it needs |
|---|---|---|
| Homebrew tap | formulae written, tap not created | a `dagsommer/homebrew-boks` repository, and the release tarball's checksum |
| Homebrew bottles | none | a `brew bottle` run per macOS version on Apple silicon, and somewhere to host them |
| apt repository | none | a GPG key the owner generates and holds, hosting, and an index job |
| winget | none | a Windows binary that can start a sandbox, which needs libkrun 2.0 |
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
