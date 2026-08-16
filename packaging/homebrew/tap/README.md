# dagsommer/homebrew-boks

The Homebrew tap for [Boks](https://github.com/dagsommer/boks) — a local-first tool for
running coding agents in isolated microVMs.

Two formulae:

| Formula | What it is |
|---|---|
| `boks` | the CLI, plus the whole stack it needs, plus the guest kernel and root filesystem |
| `nerdbox` | [containerd's nerdbox shim](https://github.com/containerd/nerdbox), built from source and signed with the hypervisor entitlement libkrun needs |

**Apple silicon only.** Both formulae declare `depends_on arch: :arm64` and
`depends_on :macos`, and refuse to install anywhere else. libkrun supports
Hypervisor.framework on arm64 and nothing else, so this is a restriction inherited from the
hypervisor rather than a packaging choice. Linux and Windows are served by the archives,
`.deb` and `.rpm` on the [Boks releases page](https://github.com/dagsommer/boks/releases).

> [!IMPORTANT]
> **No macOS machine has ever run these formulae.** This project has no macOS CI. What has
> been run against them is Homebrew itself — `brew style`, `brew audit --strict --new`,
> dependency resolution, bottle fetches, and a build of `boks`'s own `install` body with the
> platform requirements removed — all on Linux, which loads and lints a macOS-only formula
> perfectly well and can never install one. The macOS-specific half is exactly the half that
> is untested: the code signing, the entitlement, and whether a VM boots. Treat your first
> `brew install boks` as the test and please
> [open an issue](https://github.com/dagsommer/boks/issues) with what happened.

## Install

```sh
brew tap dagsommer/boks
brew trust dagsommer/boks
brew trust libkrun/krun
brew install boks
```

Then, once, as root — and this is the only step that needs root:

```sh
sudo mkdir -p /var/run/containerd
sudo chown "$(id -u):$(id -g)" /var/run/containerd
boks doctor
```

containerd derives each shim's socket path from a compile-time constant, so no configuration
setting moves it ([containerd#12444](https://github.com/containerd/containerd/issues/12444)).
`boks daemon start` does everything else: it runs containerd rootless with the right `PATH`,
which is the daemon's and not your shell's.

## Why three commands before `install`, and why whole-tap trust

Since **Homebrew 6.0.0** a non-official tap must be trusted before Homebrew will load Ruby
from it — loading a formula runs code from that repository, so Homebrew asks you to say which
non-official code you accept. See [Homebrew's Tap-Trust
documentation](https://docs.brew.sh/Tap-Trust).

Homebrew prefers item-level trust (`brew trust --formula user/tap/name`) because whole-tap
trust "allows Homebrew to load every current and future formula, cask and external command"
from a tap. **For `libkrun/krun` item-level trust is not enough**, and this is worth being
precise about because the obvious incantation fails:

```
$ brew trust --formula libkrun/krun/libkrun
$ brew install boks
Error: Refusing to load formula libkrun/krun/libkrunfw from untrusted tap libkrun/krun.
```

`libkrun` depends on `libkrunfw` and `virglrenderer` from the same tap, so trusting the one
formula leaves its dependencies untrusted. Either trust the tap, or name all three:

```sh
brew trust --formula libkrun/krun/libkrun libkrun/krun/libkrunfw libkrun/krun/virglrenderer
```

Naming a formula in full on the command line trusts *that formula only*, so
`brew install dagsommer/boks/boks` on its own also fails — `nerdbox` is then a dependency
rather than something you typed, and a dependency is never implicitly trusted.

**The trust check happens before anything is downloaded or built.** It is a formula-loading
error, so you find out in under a second, not after a compile.

## What `brew install boks` pulls in

On macOS 26 (Tahoe) and macOS 15 (Sequoia), **14 kegs**, of which only `boks` and `nerdbox`
compile — both are Go builds. Everything else pours from a bottle:

```
containerd  erofs-utils  dtc  libepoxy  libyaml  lz4  molten-vk  xz
libkrun/krun/libkrun  libkrun/krun/libkrunfw  libkrun/krun/virglrenderer
go (build only)  nerdbox  boks
```

On **macOS 14 (Sonoma) there are no libkrun bottles**, so `libkrun`, `libkrunfw` and
`virglrenderer` build from source and drag in `rust`, `lld`, `llvm`, `meson`, `ninja`,
`python@3.14` and eleven more build-only kegs — 31 in total. `llvm` alone is a large
download. Expect tens of minutes rather than a few. Nothing is broken about it; it is the
difference between having a bottle and not.

`boks` also downloads a `boks-guest_<version>_arm64.tar.gz` release asset — the guest kernel
and EROFS root filesystem the microVM boots — and installs both into
`$(brew --prefix)/lib`, which is the last directory nerdbox's shim scans on Apple silicon.

## Licensing

The formulae are Apache-2.0, like Boks and nerdbox. The guest kernel `boks` installs is
GPL-2.0 and nerdbox patches it; the archive carries a `SOURCE.txt` naming the
`cdn.kernel.org` tarball, the config and the patch set it was built from.

## Reporting problems, and where these files come from

**Do not send pull requests here.** The formulae are generated: they live as templates in
[`packaging/homebrew/tap/Formula/`](https://github.com/dagsommer/boks/tree/main/packaging/homebrew)
in the Boks repository and are rendered by `packaging/homebrew/render.sh`, which stamps the
version and the two checksums a release changes. An edit made here is overwritten by the next
release. File issues and pull requests against
[dagsommer/boks](https://github.com/dagsommer/boks) instead.
